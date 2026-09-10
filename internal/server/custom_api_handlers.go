package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/customapi"
)

func (s *Server) handleGetCustomAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !auth.FromContext(r.Context()).CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "not permitted")
		return
	}
	configuredAgent, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	key, err := agent.LoadCustomAPIKey(s.agents.Credentials(), id, configuredAgent.CustomBaseURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "load custom API key")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"configured": key != ""})
}

func (s *Server) handleSetCustomAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !auth.FromContext(r.Context()).CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "not permitted")
		return
	}
	releasePatch := s.agents.LockPatch(id)
	defer releasePatch()
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
	configuredAgent, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, agent.CustomAPIKeyMaxBytes+1024)
	var req struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "apiKey is required")
		return
	}
	if len(req.APIKey) > agent.CustomAPIKeyMaxBytes {
		writeError(w, http.StatusBadRequest, "bad_request", "apiKey is too large")
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "baseURL is required")
		return
	}
	if strings.TrimSpace(req.BaseURL) != strings.TrimSpace(configuredAgent.CustomBaseURL) {
		writeError(w, http.StatusConflict, "base_url_not_saved", "save custom Base URL before storing its API key")
		return
	}
	if err := agent.StoreCustomAPIKey(s.agents.Credentials(), id, req.BaseURL, req.APIKey); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "store custom API key")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"configured": true})
}

func (s *Server) handleDeleteCustomAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !auth.FromContext(r.Context()).CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "not permitted")
		return
	}
	releasePatch := s.agents.LockPatch(id)
	defer releasePatch()
	releaseMut, err := s.agents.AcquireMutation(id)
	if err != nil {
		writeError(w, http.StatusConflict, "agent_busy", err.Error())
		return
	}
	defer releaseMut()
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	if !s.requireCredentialStore(w) {
		return
	}
	if err := agent.StoreCustomAPIKey(s.agents.Credentials(), id, "", ""); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "delete custom API key")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"configured": false})
}

// handleCustomModels queries an OpenAI-compatible /v1/models endpoint. A
// global request is used by AgentCreate with a transient API key; the
// agent-scoped route loads the encrypted key and is transparently proxied to
// the holder peer by remoteAgentProxyMiddleware.
func (s *Server) handleCustomModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if id == "" {
		if !p.IsOwner() {
			writeError(w, http.StatusForbidden, "forbidden", "owner access required")
			return
		}
	} else {
		if !p.CanReadFull(id) {
			writeError(w, http.StatusForbidden, "forbidden", "not permitted")
			return
		}
		_, ok := s.agents.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
			return
		}
	}

	baseURL := strings.TrimSpace(r.URL.Query().Get("baseURL"))
	var explicitKey *string
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, agent.CustomAPIKeyMaxBytes+4096)
		var req struct {
			BaseURL string  `json:"baseURL"`
			APIKey  *string `json:"apiKey,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
			return
		}
		baseURL = strings.TrimSpace(req.BaseURL)
		explicitKey = req.APIKey
	}
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	apiKey := ""
	if explicitKey != nil {
		apiKey = strings.TrimSpace(*explicitKey)
		if len(apiKey) > agent.CustomAPIKeyMaxBytes {
			writeError(w, http.StatusBadRequest, "bad_request", "apiKey is too large")
			return
		}
	} else if id != "" {
		// A missing credential store still permits unauthenticated custom
		// endpoints; persistence routes report the store failure separately.
		if s.agents.HasCredentials() {
			var err error
			apiKey, err = agent.LoadCustomAPIKey(s.agents.Credentials(), id, baseURL)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "load custom API key")
				return
			}
		}
	}

	client, err := customapi.NewLocalOrTailnetClient(r.Context(), baseURL, 5*time.Second)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	defer client.CloseIdleConnections()
	parsed, _ := customapi.ParseBaseURL(baseURL) // already validated above
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/models"
	parsed.RawPath = ""

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid baseURL")
		return
	}
	if apiKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connection_error", fmt.Sprintf("cannot reach %s: %v", baseURL, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("endpoint returned %d", resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read_error", err.Error())
		return
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		writeError(w, http.StatusBadGateway, "parse_error", "invalid JSON from models endpoint")
		return
	}
	models := make([]string, 0, len(result.Data))
	for _, model := range result.Data {
		if model.ID != "" {
			models = append(models, model.ID)
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"models": models})
}
