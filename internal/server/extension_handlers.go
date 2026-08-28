package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/eventbus"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

// extensionGitTimeout bounds a single install/update/preview. Network
// git operations are the only unbounded work these handlers do, and a
// wedged remote must not pin a request goroutine forever.
const extensionGitTimeout = 2 * time.Minute

// Every route in this file is absent from auth.AllowNonOwner, so the
// policy layer already restricts them to the Owner. Installing code
// from an arbitrary git URL is the most privileged thing kojo can be
// asked to do; no agent token reaches it.

// requireExtensions resolves the registry or writes 503.
func (s *Server) requireExtensions(w http.ResponseWriter) (*extpkg.Manager, bool) {
	if s.extensions == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "extension registry not available")
		return nil, false
	}
	return s.extensions, true
}

// writeExtensionError maps registry errors onto status codes. A scope
// mismatch returns the manifest so the UI can re-render the consent
// dialog without re-fetching the repository.
func writeExtensionError(w http.ResponseWriter, err error) {
	var mismatch *extpkg.ScopeMismatchError
	switch {
	case errors.Is(err, extpkg.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, extpkg.ErrAlreadyInstalled):
		writeError(w, http.StatusConflict, "already_installed", err.Error())
	case errors.As(err, &mismatch):
		writeJSONResponse(w, http.StatusConflict, map[string]any{
			"error": map[string]string{
				"code":    "scope_mismatch",
				"message": err.Error(),
			},
			"manifest": mismatch.Manifest,
			"scopes":   mismatch.Manifest.ScopeSummaries(),
			"missing":  mismatch.Missing,
			"extra":    mismatch.Extra,
		})
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}

// decodeJSONBody reads a small JSON body into dst.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// handleListExtensions returns every installed extension.
func (s *Server) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	rows := mgr.List()
	// Encode as a non-nil slice so the UI always sees an array.
	if rows == nil {
		rows = []extpkg.Installed{}
	}
	services := s.runningExtensionServices()
	if services == nil {
		services = []extpkg.ServiceStatus{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"extensions": rows,
		// Which supervised processes are actually up. Separate from the
		// rows because it is runtime state, not registry state: a
		// package can be enabled and still have a crashing service.
		"services": services,
	})
}

// handleGetExtension returns one installed extension.
func (s *Server) handleGetExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	row, err := mgr.Get(r.PathValue("id"))
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, row)
}

// handlePreviewExtension fetches a package and returns its manifest
// and requested scopes without installing anything. This is the first
// half of the two-step install: fetch → consent → install.
func (s *Server) handlePreviewExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
		Ref string `json:"ref"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), extensionGitTimeout)
	defer cancel()
	mf, commit, err := mgr.Preview(ctx, req.URL, req.Ref)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	installed := false
	if _, err := mgr.Get(mf.ID); err == nil {
		installed = true
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"manifest": mf,
		"scopes":   mf.ScopeSummaries(),
		// The commit this manifest came from. The UI hands it back on
		// install so a branch that moves in between is refused rather
		// than silently installed under the approval given here.
		"commit":    commit,
		"installed": installed,
	})
}

// handleInstallExtension installs a package the operator has consented to.
func (s *Server) handleInstallExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	var req struct {
		URL       string   `json:"url"`
		Ref       string   `json:"ref"`
		Commit    string   `json:"commit"`
		AckScopes []string `json:"ackScopes"`
		// Enabled defaults to true: an operator who just approved
		// the scopes wants the extension on.
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// Over HTTP the commit is mandatory, even though extpkg.Install
	// treats it as optional. Install is reachable only after a preview
	// returned the manifest and scopes the operator consented to, and
	// the commit is what ties the consent to specific code; letting a
	// client omit it would make the two-step flow bypassable by simply
	// not asking for step one.
	if strings.TrimSpace(req.Commit) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"commit is required; preview the package first")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ctx, cancel := context.WithTimeout(r.Context(), extensionGitTimeout)
	defer cancel()
	row, err := mgr.Install(ctx, extpkg.InstallRequest{
		URL:       req.URL,
		Ref:       req.Ref,
		Commit:    req.Commit,
		AckScopes: req.AckScopes,
		Enabled:   enabled,
	})
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusCreated, row)
}

// handleUpdateExtension re-fetches an installed package.
func (s *Server) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	var req struct {
		Ref       string   `json:"ref"`
		AckScopes []string `json:"ackScopes"`
	}
	// An empty body is a valid "update at the stored ref".
	if r.ContentLength > 0 && !decodeJSONBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), extensionGitTimeout)
	defer cancel()
	row, err := mgr.Update(ctx, r.PathValue("id"), extpkg.UpdateRequest{
		Ref:       req.Ref,
		AckScopes: req.AckScopes,
	})
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusOK, row)
}

// handlePatchExtension toggles a package's global enablement.
func (s *Server) handlePatchExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled is required")
		return
	}
	row, err := mgr.SetEnabled(r.PathValue("id"), *req.Enabled)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusOK, row)
}

// handleDeleteExtension uninstalls a package.
func (s *Server) handleDeleteExtension(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	if err := mgr.Remove(r.PathValue("id")); err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// handleExtensionSchema serves the package's settings JSON Schema, which
// the web UI renders as a form.
func (s *Server) handleExtensionSchema(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	data, err := mgr.SettingsSchema(r.PathValue("id"))
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.logger.Warn("write extension schema failed", "id", r.PathValue("id"), "error", err)
	}
}

// handlePutExtensionConfig replaces a package's global settings object.
func (s *Server) handlePutExtensionConfig(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	var cfg map[string]any
	if !decodeJSONBody(w, r, &cfg) {
		return
	}
	row, err := mgr.SetConfig(r.PathValue("id"), cfg)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusOK, row)
}

// handlePutExtensionAgentBinding enables/configures an extension for one
// agent. Per-agent binding is what makes an extension like Slack — one
// workspace per agent — expressible without a bespoke settings table.
func (s *Server) handlePutExtensionAgentBinding(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	agentID := r.PathValue("agentId")
	// Reject bindings to agents that do not exist: a typo would
	// otherwise sit in the registry forever, invisible in the UI.
	if s.agents != nil {
		if _, ok := s.agents.Get(agentID); !ok {
			writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
			return
		}
	}
	var req struct {
		Enabled bool           `json:"enabled"`
		Config  map[string]any `json:"config"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	row, err := mgr.SetAgentBinding(r.PathValue("id"), agentID, extpkg.AgentBinding{
		Enabled: req.Enabled,
		Config:  req.Config,
	})
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusOK, row)
}

// publishExtensionsChanged broadcasts a cache-invalidation event so
// other open clients refresh their extension list, and brings the
// supervised service processes in line with the change that just
// landed. Every mutating handler funnels through here, which is what
// keeps "registry changed" and "processes restarted" from drifting
// apart.
func (s *Server) publishExtensionsChanged() {
	s.reconcileExtensionServices()
	s.PublishEvent(eventbus.Event{Table: "extensions", Op: "update"})
}

// handleGetExtensionToken returns a package's bearer token. Owner-only
// like every route in this file. An operator needs it to run a
// package's service by hand during development, and to configure a
// service kojo does not supervise (one hosted elsewhere).
func (s *Server) handleGetExtensionToken(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	token, err := mgr.Token(id)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"id":      id,
		"token":   token,
		"apiBase": mgr.APIBase(),
	})
}

// handleRotateExtensionToken issues a fresh token and restarts the
// package's service so it picks the new value up. The old token stops
// resolving immediately.
func (s *Server) handleRotateExtensionToken(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.requireExtensions(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	token, err := mgr.RotateToken(id)
	if err != nil {
		writeExtensionError(w, err)
		return
	}
	s.publishExtensionsChanged()
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "token": token})
}
