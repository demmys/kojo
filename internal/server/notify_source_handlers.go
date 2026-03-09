package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
	gmailpkg "github.com/loppo-llc/kojo/internal/notifysource/gmail"
)

// --- Notify Source CRUD ---

func (s *Server) handleListNotifySources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	sources := a.NotifySources
	if sources == nil {
		sources = []notifysource.Config{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleCreateNotifySource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	var req struct {
		Type            string            `json:"type"`
		IntervalMinutes int               `json:"intervalMinutes"`
		Query           string            `json:"query"`
		Options         map[string]string `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "type is required")
		return
	}
	if req.IntervalMinutes <= 0 {
		req.IntervalMinutes = 10
	}

	cfg := notifysource.Config{
		ID:              generateSourceID(),
		Type:            req.Type,
		Enabled:         false, // disabled until OAuth2 is set up
		IntervalMinutes: req.IntervalMinutes,
		Query:           req.Query,
		Options:         req.Options,
	}

	sources := append(a.NotifySources, cfg)
	if err := s.agents.UpdateNotifySources(id, sources); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusCreated, map[string]any{"source": cfg})
}

func (s *Server) handleUpdateNotifySource(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	var req struct {
		Enabled         *bool             `json:"enabled"`
		IntervalMinutes *int              `json:"intervalMinutes"`
		Query           *string           `json:"query"`
		Options         map[string]string `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	// If enabling, verify OAuth tokens exist
	if req.Enabled != nil && *req.Enabled {
		if !s.agents.HasCredentials() {
			writeError(w, http.StatusBadRequest, "bad_request", "credential store not available")
			return
		}
		var srcType string
		for _, cfg := range a.NotifySources {
			if cfg.ID == sourceID {
				srcType = cfg.Type
				break
			}
		}
		if srcType != "" {
			creds := s.agents.Credentials()
			if _, err := creds.GetToken(srcType, agentID, sourceID, "access_token"); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "OAuth not configured — authorize first")
				return
			}
		}
	}

	found := false
	sources := make([]notifysource.Config, len(a.NotifySources))
	copy(sources, a.NotifySources)
	for i := range sources {
		if sources[i].ID != sourceID {
			continue
		}
		found = true
		if req.Enabled != nil {
			sources[i].Enabled = *req.Enabled
		}
		if req.IntervalMinutes != nil {
			sources[i].IntervalMinutes = *req.IntervalMinutes
		}
		if req.Query != nil {
			sources[i].Query = *req.Query
		}
		if req.Options != nil {
			sources[i].Options = req.Options
		}
	}

	if !found {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	if err := s.agents.UpdateNotifySources(agentID, sources); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	for _, cfg := range sources {
		if cfg.ID == sourceID {
			writeJSONResponse(w, http.StatusOK, map[string]any{"source": cfg})
			return
		}
	}
}

func (s *Server) handleDeleteNotifySource(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	found := false
	var sourceType string
	var sources []notifysource.Config
	for _, cfg := range a.NotifySources {
		if cfg.ID == sourceID {
			found = true
			sourceType = cfg.Type
			continue
		}
		sources = append(sources, cfg)
	}

	if !found {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	if err := s.agents.UpdateNotifySources(agentID, sources); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Clean up tokens for this source
	if s.agents.HasCredentials() && sourceType != "" {
		s.agents.Credentials().DeleteTokensBySource(sourceType, agentID, sourceID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- OAuth2 Flow ---

func (s *Server) handleNotifySourceAuth(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	// Find the source config
	var srcCfg *notifysource.Config
	for _, cfg := range a.NotifySources {
		if cfg.ID == sourceID {
			c := cfg
			srcCfg = &c
			break
		}
	}
	if srcCfg == nil {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	// Get OAuth2 client credentials (stored globally)
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}
	creds := s.agents.Credentials()
	clientID, err := creds.GetToken(srcCfg.Type, "", "", "client_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("OAuth2 client_id not configured for %s. Set it via POST /api/v1/oauth-clients/%s", srcCfg.Type, srcCfg.Type))
		return
	}

	// Build redirect URI based on the request's host.
	// Host header manipulation is mitigated by:
	// 1. Tailscale network controls access
	// 2. Google requires redirect_uri to be pre-registered in GCP Console
	scheme := "https"
	if s.devMode {
		scheme = "http"
	}
	redirectURI := fmt.Sprintf("%s://%s/oauth2/callback", scheme, r.Host)

	oauth2Mgr := s.getOAuth2Manager()
	authURL, err := oauth2Mgr.StartAuthFlow(clientID, agentID, sourceID, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"authUrl": authURL})
}

func (s *Server) handleOAuth2Callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		http.Error(w, "Authorization denied: "+errParam, http.StatusBadRequest)
		return
	}
	if code == "" || state == "" {
		http.Error(w, "Missing code or state", http.StatusBadRequest)
		return
	}

	oauth2Mgr := s.getOAuth2Manager()

	// Look up the pending auth to determine the source type (peek only, don't consume)
	pending := oauth2Mgr.PeekPending(state)
	if pending == nil {
		http.Error(w, "Unknown or expired state", http.StatusBadRequest)
		return
	}

	// Get the source config to determine provider type
	a, ok := s.agents.Get(pending.AgentID)
	if !ok {
		http.Error(w, "Agent not found", http.StatusBadRequest)
		return
	}
	var provider string
	for _, cfg := range a.NotifySources {
		if cfg.ID == pending.SourceID {
			provider = cfg.Type
			break
		}
	}
	if provider == "" {
		http.Error(w, "Source not found", http.StatusBadRequest)
		return
	}

	if !s.agents.HasCredentials() {
		http.Error(w, "Credential store not available", http.StatusInternalServerError)
		return
	}
	creds := s.agents.Credentials()
	clientID, err := creds.GetToken(provider, "", "", "client_id")
	if err != nil {
		http.Error(w, "OAuth client_id not found", http.StatusInternalServerError)
		return
	}
	clientSecret, err := creds.GetToken(provider, "", "", "client_secret")
	if err != nil {
		http.Error(w, "OAuth client_secret not found", http.StatusInternalServerError)
		return
	}

	auth, tokenResp, err := oauth2Mgr.CompleteAuthFlow(r.Context(), state, code, clientID, clientSecret)
	if err != nil {
		http.Error(w, "Token exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Store tokens — check each operation for errors
	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	// Store client_id and client_secret per-source so the source can refresh tokens independently
	for _, kv := range []struct{ k, v string }{
		{"client_id", clientID},
		{"client_secret", clientSecret},
		{"access_token", tokenResp.AccessToken},
	} {
		exp := time.Time{}
		if kv.k == "access_token" {
			exp = expiry
		}
		if err := creds.SetToken(provider, auth.AgentID, auth.SourceID, kv.k, kv.v, exp); err != nil {
			http.Error(w, "Failed to save token: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if tokenResp.RefreshToken != "" {
		if err := creds.SetToken(provider, auth.AgentID, auth.SourceID, "refresh_token", tokenResp.RefreshToken, time.Time{}); err != nil {
			http.Error(w, "Failed to save refresh token: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Enable the source
	updatedA, ok := s.agents.Get(auth.AgentID)
	if ok {
		sources := make([]notifysource.Config, len(updatedA.NotifySources))
		copy(sources, updatedA.NotifySources)
		for i := range sources {
			if sources[i].ID == auth.SourceID {
				sources[i].Enabled = true
				break
			}
		}
		if err := s.agents.UpdateNotifySources(auth.AgentID, sources); err != nil {
			http.Error(w, "Failed to enable source: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Notify the opener window and close
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html><body>
<p>Authorization successful.</p>
<script>
if(window.opener){window.opener.postMessage({type:"oauth_complete"},"*")}
window.close()
</script>
</body></html>`)
}

// --- OAuth Client Configuration ---

func (s *Server) handleListOAuthClients(w http.ResponseWriter, r *http.Request) {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	providers := []string{"gmail"}
	type clientInfo struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
	}

	var clients []clientInfo
	creds := s.agents.Credentials()
	for _, p := range providers {
		_, err := creds.GetToken(p, "", "", "client_id")
		clients = append(clients, clientInfo{
			Provider:   p,
			Configured: err == nil,
		})
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"clients": clients})
}

func (s *Server) handleSetOAuthClient(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "clientId and clientSecret are required")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.SetToken(provider, "", "", "client_id", req.ClientID, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save client_id: "+err.Error())
		return
	}
	if err := creds.SetToken(provider, "", "", "client_secret", req.ClientSecret, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save client_secret: "+err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteOAuthClient(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.DeleteToken(provider, "", "", "client_id"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if err := creds.DeleteToken(provider, "", "", "client_secret"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Source Type Registry ---

func (s *Server) handleListNotifySourceTypes(w http.ResponseWriter, r *http.Request) {
	types := []map[string]any{
		{
			"type":        "gmail",
			"name":        "Gmail",
			"description": "Google Gmail notifications",
			"authType":    "oauth2",
		},
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"types": types})
}

// --- OAuth2 Manager (lazy init) ---

func (s *Server) getOAuth2Manager() *gmailpkg.OAuth2Manager {
	s.oauth2Once.Do(func() {
		s.oauth2Mgr = gmailpkg.NewOAuth2Manager()
	})
	return s.oauth2Mgr
}

// helpers

func generateSourceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "ns_" + hex.EncodeToString(b)
}
