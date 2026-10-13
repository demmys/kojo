package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/chathistory"
)

// Extension chat sessions
//
// POST /api/v1/agents/{id}/messages drops an extension's text into the
// agent's main conversation, which is the wrong shape for a chat
// bridge: a Discord channel must not share a transcript with the
// operator's own chat, or with another channel. These routes give a
// package the same primitive the built-in Slack bot uses — a keyed
// one-shot turn whose backend session is resumed per key — behind the
// `chat:session` scope.
//
// The session key a package supplies is only a suffix. The server
// prefixes the agent and package IDs, so a package can neither resume
// a Slack thread's session nor another package's.

const (
	extensionChatBodyLimit    = 2 << 20
	extensionChatMaxMessage   = 256 << 10
	extensionChatMaxPrompt    = 16 << 10
	extensionChatMaxHistory   = 200
	extensionChatMaxAttach    = 10
	extensionChatHeartbeat    = 15 * time.Second
	extensionChatWriteTimeout = 10 * time.Second
	extensionChatContentType  = "application/x-ndjson"
)

// extensionSessionKeyRe constrains the package-chosen key suffix. It
// ends up in file names on some backends, so the alphabet is closed.
var extensionSessionKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,159}$`)

type extensionChatRequest struct {
	SessionKey string `json:"sessionKey"`
	Message    string `json:"message"`
	// SystemPromptExtra is appended to the agent's system prompt for
	// this turn: where the message came from and how the reply will be
	// delivered.
	SystemPromptExtra string `json:"systemPromptExtra,omitempty"`
	// History is the conversation as the external platform has it,
	// excluding the current message. It seeds a fresh session and is
	// offered as a bounded recap on resume.
	History           []chathistory.HistoryMessage `json:"history,omitempty"`
	HistorySelfUserID string                       `json:"historySelfUserId,omitempty"`
	// Attachments are files the package already wrote into kojo's
	// upload directory (KOJO_UPLOAD_DIR).
	Attachments []agent.MessageAttachment `json:"attachments,omitempty"`
}

type extensionChatSteerRequest struct {
	SessionKey string `json:"sessionKey"`
	Content    string `json:"content"`
}

// extensionChatSessions tracks the keyed turns currently streaming so a
// second request for the same session is refused instead of racing the
// first for one backend session. The caller is expected to steer.
type extensionChatSessions struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func (e *extensionChatSessions) acquire(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, busy := e.active[key]; busy {
		return false
	}
	if e.active == nil {
		e.active = map[string]struct{}{}
	}
	e.active[key] = struct{}{}
	return true
}

func (e *extensionChatSessions) release(key string) {
	e.mu.Lock()
	delete(e.active, key)
	e.mu.Unlock()
}

// extensionSessionKey namespaces a package-chosen key. The "ext." tag
// matches auth.ExtensionKVNamespace and cannot collide with the
// "slack:" keys the built-in bot derives.
func extensionSessionKey(agentID, extensionID, key string) string {
	return agentID + ":ext." + extensionID + ":" + key
}

// extensionChatPrincipal admits only an extension token. The policy
// layer has already checked the scope and the agent binding; this
// keeps the Owner and agents off a route whose session namespace is
// derived from an extension ID they do not have.
func extensionChatPrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p := auth.FromContext(r.Context())
	if !p.IsExtension() || p.ExtensionID == "" {
		writeError(w, http.StatusForbidden, "forbidden", "extension chat is available to extension tokens only")
		return p, false
	}
	return p, true
}

func (s *Server) handleExtensionChat(w http.ResponseWriter, r *http.Request) {
	p, ok := extensionChatPrincipal(w, r)
	if !ok {
		return
	}
	if s.externalChat == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "external chat is not available on this host")
		return
	}
	agentID := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, extensionChatBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req extensionChatRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid extension chat request: "+err.Error())
		return
	}
	if !extensionSessionKeyRe.MatchString(req.SessionKey) {
		writeError(w, http.StatusBadRequest, "bad_request", "sessionKey must match "+extensionSessionKeyRe.String())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	if len(req.Message) > extensionChatMaxMessage || len(req.SystemPromptExtra) > extensionChatMaxPrompt {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "message or systemPromptExtra is too long")
		return
	}
	if len(req.History) > extensionChatMaxHistory || len(req.Attachments) > extensionChatMaxAttach {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "too many history entries or attachments")
		return
	}
	attachments := make([]agent.MessageAttachment, 0, len(req.Attachments))
	for _, a := range req.Attachments {
		// Same rule as every other upload consumer: the file has to
		// sit in kojo's upload directory. Without it a package holding
		// only chat:session could point the agent at any path.
		f, size, kind := openUploadPath(a.Path)
		if kind != "" {
			writeError(w, http.StatusBadRequest, "invalid_attachment", uploadPathUserMessage(kind))
			return
		}
		resolved := f.Name()
		_ = f.Close()
		attachments = append(attachments, agent.MessageAttachment{
			Path: resolved,
			Name: a.Name,
			Mime: a.Mime,
			Size: size,
		})
	}

	sessionKey := extensionSessionKey(agentID, p.ExtensionID, req.SessionKey)
	if !s.extensionChats.acquire(sessionKey) {
		writeError(w, http.StatusConflict, "busy", "a turn is already running for this session; steer it instead")
		return
	}
	// The key stays held until the turn's event stream has ended, which
	// can be after this handler returns: on a disconnect the backend is
	// still winding down, and a retry let in before it finishes would
	// run a second turn on the same backend session.
	handedOff := false
	defer func() {
		if !handedOff {
			s.extensionChats.release(sessionKey)
		}
	}()

	events, err := s.externalChat.ChatOneShot(r.Context(), agentID, req.Message, agent.OneShotOpts{
		SessionKey:        sessionKey,
		History:           req.History,
		HistorySelfUserID: req.HistorySelfUserID,
		SystemPromptExtra: req.SystemPromptExtra,
		// The package delivers text; files go out through whatever
		// tool the package contributes, not kojo's attachment staging.
		DisableKojoAttachmentInstructions: true,
		Attachments:                       attachments,
	})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", "agent not found")
		default:
			writeError(w, http.StatusConflict, "chat_rejected", err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", extensionChatContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	enc := json.NewEncoder(w)
	heartbeat := time.NewTicker(extensionChatHeartbeat)
	defer heartbeat.Stop()
	// A failed write means the package went away. Returning cancels
	// r.Context(), which is the turn's context, so the backend stops
	// too; the drain keeps the producer from blocking on a channel
	// nobody reads while it winds down.
	write := func(v any) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(extensionChatWriteTimeout))
		err := enc.Encode(v)
		if err == nil {
			err = controller.Flush()
		}
		_ = controller.SetWriteDeadline(time.Time{})
		return err == nil
	}
	drain := func() {
		handedOff = true
		go func() {
			defer s.extensionChats.release(sessionKey)
			for range events {
			}
		}()
	}
	for {
		select {
		case evt, open := <-events:
			if !open {
				return
			}
			if !write(evt) {
				drain()
				return
			}
		case <-heartbeat.C:
			if !write(agent.ChatEvent{Type: "ping"}) {
				drain()
				return
			}
		case <-r.Context().Done():
			drain()
			return
		}
	}
}

func (s *Server) handleExtensionChatSteer(w http.ResponseWriter, r *http.Request) {
	p, ok := extensionChatPrincipal(w, r)
	if !ok {
		return
	}
	if s.externalChat == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "external chat is not available on this host")
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatSteerRequest
	r.Body = http.MaxBytesReader(w, r.Body, extensionChatBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid steer request: "+err.Error())
		return
	}
	if !extensionSessionKeyRe.MatchString(req.SessionKey) {
		writeError(w, http.StatusBadRequest, "bad_request", "sessionKey must match "+extensionSessionKeyRe.String())
		return
	}
	if strings.TrimSpace(req.Content) == "" || len(req.Content) > extensionChatMaxMessage {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required and bounded")
		return
	}
	err := s.externalChat.SteerOneShot(r.Context(), agentID, extensionSessionKey(agentID, p.ExtensionID, req.SessionKey), req.Content)
	switch {
	case err == nil:
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
	case errors.Is(err, agent.ErrAgentNotBusy):
		writeError(w, http.StatusConflict, "not_busy", "no turn is running for this session")
	case errors.Is(err, agent.ErrSteerUnsupported):
		writeError(w, http.StatusConflict, "steer_unsupported", "the agent's backend does not support steering")
	case errors.Is(err, agent.ErrSteerDeliveryUncertain):
		// The backend may have taken it. A package that resent the
		// content as a fresh turn could run the instruction twice, so
		// this gets its own code rather than a flat failure.
		writeError(w, http.StatusBadGateway, "steer_uncertain", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "steer_failed", err.Error())
	}
}

// handleExtensionSecrets returns the calling package's own secret
// settings. A supervised service already has them in KOJO_EXT_SECRETS;
// this is for a contributed MCP server, whose environment kojo cannot
// keep private (see extpkg/secrets.go).
func (s *Server) handleExtensionSecrets(w http.ResponseWriter, r *http.Request) {
	p, ok := extensionChatPrincipal(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	agentID := r.URL.Query().Get("agentId")
	// The policy layer sees only the path, so the binding check for a
	// query-addressed agent has to happen here.
	if agentID != "" && !p.BoundTo(agentID) {
		writeError(w, http.StatusForbidden, "forbidden", "extension is not enabled for this agent")
		return
	}
	secrets, err := mgr.Secrets(p.ExtensionID, agentID)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, map[string]any{"secrets": secrets})
}
