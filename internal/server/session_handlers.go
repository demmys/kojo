package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/session"
)

// --- Session Handlers ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	homeDir, _ := os.UserHomeDir()
	resp := map[string]any{
		"version":   s.version,
		"hostname":  hostname,
		"homeDir":   homeDir,
		"tools":     session.ToolAvailability(s.sessions.GetLMSProxyPort()),
		"shellTool": session.ShellToolName(),
	}
	if s.agents != nil {
		resp["agentBackends"] = s.agents.BackendAvailability()
		resp["lmStudioModels"] = s.agents.LMStudioModels()
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list := s.sessions.List()
	infos := make([]session.SessionInfo, len(list))
	for i, sess := range list {
		infos[i] = sess.Info()
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool     string   `json:"tool"`
		WorkDir  string   `json:"workDir"`
		Args     []string `json:"args"`
		YoloMode bool     `json:"yoloMode"`
		ParentID string   `json:"parentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool is required")
		return
	}
	if req.WorkDir == "" {
		home, _ := os.UserHomeDir()
		req.WorkDir = home
	}

	sess, err := s.sessions.Create(req.Tool, req.WorkDir, req.Args, req.YoloMode, req.ParentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	var err error
	if sess.Info().Status == session.StatusRunning {
		err = s.sessions.Stop(id)
	} else {
		err = s.sessions.Remove(id)
	}
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}

	var req struct {
		YoloMode *bool `json:"yoloMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	if req.YoloMode != nil {
		sess.SetYoloMode(*req.YoloMode)
	}

	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleTerminalSession(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	sess, ok := s.sessions.FindChildSession(parentID, session.ShellToolName())
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no terminal session for parent: "+parentID)
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.sessions.Restart(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleTmuxAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Action == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "action is required")
		return
	}
	if err := s.sessions.TmuxAction(id, req.Action); err != nil {
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, session.ErrSessionNotRunning), errors.Is(err, session.ErrNotTerminal):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Attachment Handlers ---

func (s *Server) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	attachments := sess.Attachments()
	if attachments == nil {
		attachments = []*session.Attachment{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"attachments": attachments})
}

func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return
	}
	// Only allow deleting files that are tracked as attachments
	absPath, _ := filepath.Abs(path)
	if !sess.HasAttachment(absPath) {
		writeError(w, http.StatusBadRequest, "bad_request", "path is not a tracked attachment")
		return
	}
	if err := s.files.ValidatePath(path); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	sess.RemoveAttachment(absPath)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Directory Suggestion Handler ---

func (s *Server) handleDirSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": []string{}})
		return
	}

	// expand ~ to home directory
	if strings.HasPrefix(prefix, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			prefix = home + prefix[1:]
		}
	}

	// determine parent dir and partial name
	dir := filepath.Dir(prefix)
	partial := filepath.Base(prefix)

	// if prefix ends with a path separator, list that directory
	if strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, string(filepath.Separator)) {
		dir = prefix
		partial = ""
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": []string{}})
		return
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(partial)) {
			continue
		}
		full := filepath.Join(dir, name)
		dirs = append(dirs, full)
		if len(dirs) >= 10 {
			break
		}
	}

	if dirs == nil {
		dirs = []string{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": dirs})
}
