package server

import (
	"context"
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
// package a keyed one-shot turn whose backend session is resumed per
// key — the primitive the Slack bot ran on when it was built into kojo
// — behind the `chat:session` scope.
//
// The session key a package supplies is only a suffix. The server
// prefixes the agent and package IDs, so a package cannot resume
// another package's session. The one deliberate exception is the
// first-party Slack package (extensionSlackPackageID), which owns the
// namespace the built-in Slack bot used before it was removed.

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

// extensionSlackPackageID is the one package ID that does not get an
// "ext.<id>" session namespace. The first-party Slack package replaced
// the built-in Slack bot and inherits its "<agentID>:slack:<suffix>"
// keys, so the thread sessions, goal bindings and lingering background
// sessions that already exist keep resolving after the move, and the
// core checks keyed on ":slack:" (goal ownership, NO_REPLY filtering,
// keyed background surfaces, the remote relay) keep applying to it.
//
// The same ID is what admits the goal, handoff, question and lingering
// options below: their supporting machinery is single-slot per agent
// and keyed on that namespace, so handing them to an "ext.<id>" session
// would produce goals nobody can recover and background turns delivered
// to somebody else's surface.
const extensionSlackPackageID = "slack"

// extensionOwnsSlackNamespace reports whether the package is the
// first-party Slack package.
func extensionOwnsSlackNamespace(extensionID string) bool {
	return extensionID == extensionSlackPackageID
}

// extensionGoalUserRe is the shape of a Slack user ID. Goal recovery
// (resumeGoalSurface) refuses anything else, so a goal started under a
// different shape could never be resumed.
var extensionGoalUserRe = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)

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

	// PreserveTerminalOnCancel keeps the terminal event of a turn stopped
	// through ext-chat/cancel in the stream. This handler always reads
	// the turn to its end, so any package may ask for it.
	PreserveTerminalOnCancel bool `json:"preserveTerminalOnCancel,omitempty"`

	// The fields below are reserved for the first-party Slack package;
	// see extensionSlackPackageID and reservedOption.

	// Goal is a native goal command and GoalUserID the platform user who
	// issued it (or, without Goal, the author of an ordinary reply).
	Goal       *agent.GoalRequest `json:"goal,omitempty"`
	GoalUserID string             `json:"goalUserId,omitempty"`
	// InteractiveQuestions lets the agent ask a question the package
	// answers through ext-chat/question.
	InteractiveQuestions bool `json:"interactiveQuestions,omitempty"`
	// LingerBackgroundTasks keeps the keyed session alive for
	// run_in_background tasks; their completion arrives on the events
	// stream as a background turn.
	LingerBackgroundTasks bool `json:"lingerBackgroundTasks,omitempty"`
	// ForceFreshSession and ExpectedHolderPeer mark a device-handoff
	// arrival turn: start from History instead of the transferred
	// session, on exactly the holder that asked for it.
	ForceFreshSession  bool   `json:"forceFreshSession,omitempty"`
	ExpectedHolderPeer string `json:"expectedHolderPeer,omitempty"`
	// HandoffReservationID names a slot the package reserved behind this
	// turn. If the turn moves the agent to another device, the events
	// stream asks the package to run the arrival turn in that slot.
	HandoffReservationID string `json:"handoffReservationId,omitempty"`
}

// reservedOption names the first field of the request that only the
// first-party Slack package may set, or "" when there is none.
func (req *extensionChatRequest) reservedOption() string {
	switch {
	case req.Goal != nil:
		return "goal"
	case req.GoalUserID != "":
		return "goalUserId"
	case req.InteractiveQuestions:
		return "interactiveQuestions"
	case req.LingerBackgroundTasks:
		return "lingerBackgroundTasks"
	case req.ForceFreshSession:
		return "forceFreshSession"
	case req.ExpectedHolderPeer != "":
		return "expectedHolderPeer"
	case req.HandoffReservationID != "":
		return "handoffReservationId"
	}
	return ""
}

type extensionChatSteerRequest struct {
	SessionKey string `json:"sessionKey"`
	Content    string `json:"content"`
	// UserID is the platform user who wrote Content. Core only reads it
	// for sessions in the Slack namespace, where a running goal accepts
	// steering from its owner alone.
	UserID string `json:"userId,omitempty"`
}

type extensionChatSessionRequest struct {
	SessionKey string `json:"sessionKey"`
}

type extensionChatQuestionRequest struct {
	SessionKey string               `json:"sessionKey"`
	Answer     agent.QuestionAnswer `json:"answer"`
}

type extensionChatGoalStopRequest struct {
	SessionKey string `json:"sessionKey"`
	UserID     string `json:"userId,omitempty"`
}

// extensionChatBackend is what these routes need from the external chat
// router. It is the set of methods the built-in Slack bot called on it,
// so an out-of-process bot gets the same capabilities.
type extensionChatBackend interface {
	ChatOneShot(ctx context.Context, agentID, message string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error)
	SteerOneShot(ctx context.Context, agentID, sessionKey, content string) error
	SteerOneShotAsUser(ctx context.Context, agentID, sessionKey, content, userID string) error
	AnswerOneShotQuestion(ctx context.Context, agentID, sessionKey string, answer agent.QuestionAnswer) error
	StopThreadBackgroundTasks(ctx context.Context, agentID, sessionKey string) error
	StopIdleGoal(ctx context.Context, agentID, sessionKey, userID string) (bool, error)
	RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func()
}

// extensionChatRouter returns the backend for the extension chat
// routes, or nil on a host without an external chat router.
func (s *Server) extensionChatRouter() extensionChatBackend {
	if s.extensionChatBackend != nil {
		return s.extensionChatBackend
	}
	if s.externalChat == nil {
		return nil
	}
	return s.externalChat
}

// extensionChatSessions tracks the keyed turns currently streaming so a
// second request for the same session is refused instead of racing the
// first for one backend session. The caller is expected to steer.
type extensionChatSessions struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// acquire claims key for one turn. cancel stops that turn; it is what
// ext-chat/cancel calls.
func (e *extensionChatSessions) acquire(key string, cancel context.CancelFunc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, busy := e.active[key]; busy {
		return false
	}
	if e.active == nil {
		e.active = map[string]context.CancelFunc{}
	}
	e.active[key] = cancel
	return true
}

// cancel stops the turn holding key and reports whether there was one.
// The key stays held until the turn's stream has ended.
func (e *extensionChatSessions) cancel(key string) bool {
	e.mu.Lock()
	cancel, busy := e.active[key]
	e.mu.Unlock()
	if busy && cancel != nil {
		cancel()
	}
	return busy
}

func (e *extensionChatSessions) release(key string) {
	e.mu.Lock()
	delete(e.active, key)
	e.mu.Unlock()
}

// extensionSessionKey namespaces a package-chosen key. The "ext." tag
// matches auth.ExtensionKVNamespace. Only the first-party Slack package
// lands in the "slack:" namespace the built-in bot derived its keys in;
// no other package ID can produce that prefix, because every other key
// carries the "ext." tag.
func extensionSessionKey(agentID, extensionID, key string) string {
	if extensionOwnsSlackNamespace(extensionID) {
		return agentID + ":slack:" + key
	}
	return agentID + ":ext." + extensionID + ":" + key
}

// extensionSessionSuffix reverses extensionSessionKey: it returns the
// package-relative key for a session in the package's namespace.
func extensionSessionSuffix(agentID, extensionID, sessionKey string) (string, bool) {
	suffix, ok := strings.CutPrefix(sessionKey, extensionSessionKey(agentID, extensionID, ""))
	return suffix, ok && suffix != ""
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
	backend := s.extensionChatRouter()
	if backend == nil {
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
	if field := req.reservedOption(); field != "" && !extensionOwnsSlackNamespace(p.ExtensionID) {
		writeError(w, http.StatusForbidden, "forbidden", field+" is reserved for the first-party Slack package")
		return
	}
	// A goal command is complete without text ("pause" has nothing to
	// say to the model); every other turn needs a message.
	if strings.TrimSpace(req.Message) == "" && req.Goal == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	if req.GoalUserID != "" && !extensionGoalUserRe.MatchString(req.GoalUserID) {
		writeError(w, http.StatusBadRequest, "bad_request", "goalUserId must match "+extensionGoalUserRe.String())
		return
	}
	if req.HandoffReservationID != "" && !extensionSessionKeyRe.MatchString(req.HandoffReservationID) {
		writeError(w, http.StatusBadRequest, "bad_request", "handoffReservationId must match "+extensionSessionKeyRe.String())
		return
	}
	if len(req.ExpectedHolderPeer) > 256 {
		writeError(w, http.StatusBadRequest, "bad_request", "expectedHolderPeer is too long")
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
	// The turn ends with the request, or earlier through ext-chat/cancel.
	// The second way exists because a disconnect cannot deliver the
	// terminal event a stopped turn still has to report.
	turnCtx, cancelTurn := context.WithCancel(r.Context())
	defer cancelTurn()
	if !s.extensionChats.acquire(sessionKey, cancelTurn) {
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

	opts := agent.OneShotOpts{
		SessionKey:        sessionKey,
		History:           req.History,
		HistorySelfUserID: req.HistorySelfUserID,
		SystemPromptExtra: req.SystemPromptExtra,
		// The package delivers text; files go out through whatever
		// tool the package contributes, not kojo's attachment staging.
		DisableKojoAttachmentInstructions: true,
		Attachments:                       attachments,
		PreserveTerminalOnCancel:          req.PreserveTerminalOnCancel,
		Goal:                              req.Goal,
		GoalUserID:                        req.GoalUserID,
		InteractiveQuestions:              req.InteractiveQuestions,
		LingerBackgroundTasks:             req.LingerBackgroundTasks,
		ForceFreshSession:                 req.ForceFreshSession,
		ExpectedHolderPeer:                req.ExpectedHolderPeer,
	}
	// Core can only activate a reservation while its turn is streaming.
	// Once the stream has ended, tell the package the slot will not be
	// used (a no-op if it was). Like the session key, that can be after
	// this handler returns.
	releaseReservation := func() {}
	if req.HandoffReservationID != "" {
		reservation := &extensionHandoffReservation{
			hub: &s.extensionEvents,
			key: extensionEventKey{extensionID: p.ExtensionID, agentID: agentID},
			id:  req.HandoffReservationID,
		}
		opts.HandoffArrivalReservation = reservation
		releaseReservation = reservation.Release
	}
	defer func() {
		if !handedOff {
			releaseReservation()
		}
	}()
	events, err := backend.ChatOneShot(turnCtx, agentID, req.Message, opts)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", "agent not found")
		case errors.Is(err, agent.ErrGoalOwnerForbidden):
			writeError(w, http.StatusForbidden, "goal_owner_forbidden", err.Error())
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
			defer releaseReservation()
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
	backend := s.extensionChatRouter()
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "external chat is not available on this host")
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatSteerRequest
	if !decodeExtensionChatBody(w, r, &req, req.sessionKey) {
		return
	}
	if strings.TrimSpace(req.Content) == "" || len(req.Content) > extensionChatMaxMessage {
		writeError(w, http.StatusBadRequest, "bad_request", "content is required and bounded")
		return
	}
	if len(req.UserID) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "userId is too long")
		return
	}
	sessionKey := extensionSessionKey(agentID, p.ExtensionID, req.SessionKey)
	var err error
	if req.UserID != "" {
		err = backend.SteerOneShotAsUser(r.Context(), agentID, sessionKey, req.Content, req.UserID)
	} else {
		err = backend.SteerOneShot(r.Context(), agentID, sessionKey, req.Content)
	}
	if err != nil {
		writeExtensionChatInputError(w, err, "steer_failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (r *extensionChatSteerRequest) sessionKey() string    { return r.SessionKey }
func (r *extensionChatSessionRequest) sessionKey() string  { return r.SessionKey }
func (r *extensionChatQuestionRequest) sessionKey() string { return r.SessionKey }
func (r *extensionChatGoalStopRequest) sessionKey() string { return r.SessionKey }

// decodeExtensionChatBody reads a small JSON control request into v and
// validates the package-relative session key it carries. sessionKey is
// read after decoding. It answers the request itself on failure.
func decodeExtensionChatBody(w http.ResponseWriter, r *http.Request, v any, sessionKey func() string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, extensionChatBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request: "+err.Error())
		return false
	}
	if sessionKey != nil && !extensionSessionKeyRe.MatchString(sessionKey()) {
		writeError(w, http.StatusBadRequest, "bad_request", "sessionKey must match "+extensionSessionKeyRe.String())
		return false
	}
	return true
}

// writeExtensionChatInputError maps the sentinels the router returns
// for input aimed at a session (steer, answer, stop) onto wire codes.
// The codes are a contract: a package maps them back to the same
// sentinels on its side. fallback is the code for everything else.
func writeExtensionChatInputError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
	case errors.Is(err, agent.ErrAgentNotBusy):
		writeError(w, http.StatusConflict, "not_busy", "no turn is running for this session")
	case errors.Is(err, agent.ErrSteerUnsupported):
		writeError(w, http.StatusConflict, "steer_unsupported", "the agent's backend does not support steering")
	case errors.Is(err, agent.ErrSteerDeliveryUncertain):
		// The backend may have taken it. A package that resent the
		// content as a fresh turn could run the instruction twice, so
		// this gets its own code rather than a flat failure.
		writeError(w, http.StatusBadGateway, "steer_uncertain", err.Error())
	case errors.Is(err, agent.ErrQuestionNotFound):
		writeError(w, http.StatusNotFound, "question_not_found", err.Error())
	case errors.Is(err, agent.ErrInvalidQuestionAnswer):
		writeError(w, http.StatusBadRequest, "invalid_answers", err.Error())
	case errors.Is(err, agent.ErrGoalOwnerForbidden):
		writeError(w, http.StatusForbidden, "goal_owner_forbidden", err.Error())
	case errors.Is(err, agent.ErrSteerOriginForbidden):
		// The turn was started by another origin (a peer, the WebUI);
		// only that origin may feed it.
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, agent.ErrBackgroundSessionNotFound):
		writeError(w, http.StatusNotFound, "background_not_found", "this session has no background tasks")
	default:
		writeError(w, http.StatusBadGateway, fallback, err.Error())
	}
}

// extensionChatControl is the shared front half of the control routes:
// an extension token and a router. It answers the request itself when
// either is missing.
func (s *Server) extensionChatControl(w http.ResponseWriter, r *http.Request) (auth.Principal, extensionChatBackend, bool) {
	p, ok := extensionChatPrincipal(w, r)
	if !ok {
		return p, nil, false
	}
	backend := s.extensionChatRouter()
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "external chat is not available on this host")
		return p, nil, false
	}
	return p, backend, true
}

// handleExtensionChatCancel stops the turn streaming for a session
// without closing its stream, so the stream still carries the turn's
// terminal event (with preserveTerminalOnCancel) — the partial reply
// and the count of background tasks that outlive the stop.
func (s *Server) handleExtensionChatCancel(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatSessionRequest
	if !decodeExtensionChatBody(w, r, &req, req.sessionKey) {
		return
	}
	if !s.extensionChats.cancel(extensionSessionKey(agentID, p.ExtensionID, req.SessionKey)) {
		writeError(w, http.StatusConflict, "not_busy", "no turn is running for this session")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleExtensionChatQuestion answers a question the agent asked in a
// turn started with interactiveQuestions. An answer is never turned
// into steering text, whatever the failure.
func (s *Server) handleExtensionChatQuestion(w http.ResponseWriter, r *http.Request) {
	p, backend, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatQuestionRequest
	if !decodeExtensionChatBody(w, r, &req, req.sessionKey) {
		return
	}
	if req.Answer.RequestID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "answer.requestId is required")
		return
	}
	if err := backend.AnswerOneShotQuestion(r.Context(), agentID, extensionSessionKey(agentID, p.ExtensionID, req.SessionKey), req.Answer); err != nil {
		writeExtensionChatInputError(w, err, "question_failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleExtensionChatGoalStop fences a goal whose device handoff is
// still pending for an idle session — the case a stop command meets
// when there is no running turn to cancel.
func (s *Server) handleExtensionChatGoalStop(w http.ResponseWriter, r *http.Request) {
	p, backend, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatGoalStopRequest
	if !decodeExtensionChatBody(w, r, &req, req.sessionKey) {
		return
	}
	if req.UserID != "" && !extensionGoalUserRe.MatchString(req.UserID) {
		writeError(w, http.StatusBadRequest, "bad_request", "userId must match "+extensionGoalUserRe.String())
		return
	}
	handled, err := backend.StopIdleGoal(r.Context(), agentID, extensionSessionKey(agentID, p.ExtensionID, req.SessionKey), req.UserID)
	switch {
	case err == nil:
		writeJSONResponse(w, http.StatusOK, map[string]any{"handled": handled})
	case errors.Is(err, agent.ErrGoalOwnerForbidden):
		writeError(w, http.StatusForbidden, "goal_owner_forbidden", err.Error())
	case handled:
		// The stop was this user's to make and is recorded, but the
		// holder has not confirmed it. That is an outcome to report in
		// the conversation, not a failed request: answering with an
		// error status would lose "handled" and read as "nothing to stop".
		writeJSONResponse(w, http.StatusOK, map[string]any{"handled": true, "error": err.Error()})
	default:
		writeError(w, http.StatusBadGateway, "goal_stop_failed", err.Error())
	}
}

// handleExtensionChatBackgroundStop stops every background task of a
// session's lingering process (a thread's "stop all").
func (s *Server) handleExtensionChatBackgroundStop(w http.ResponseWriter, r *http.Request) {
	p, backend, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	agentID := r.PathValue("id")
	var req extensionChatSessionRequest
	if !decodeExtensionChatBody(w, r, &req, req.sessionKey) {
		return
	}
	if err := backend.StopThreadBackgroundTasks(r.Context(), agentID, extensionSessionKey(agentID, p.ExtensionID, req.SessionKey)); err != nil {
		writeExtensionChatInputError(w, err, "background_stop_failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
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
