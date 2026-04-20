package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleGetAPIKey returns whether an API key is configured for the given provider.
// Does NOT return the actual key — only a configured/not-configured status.
func (s *Server) handleGetAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	_, err := creds.GetToken(provider, "", "", "api_key")
	configured := err == nil

	// Check nanobanana fallback for gemini
	hasFallback := false
	if provider == "gemini" {
		if home, err := os.UserHomeDir(); err == nil {
			data, err := os.ReadFile(filepath.Join(home, ".config", "nanobanana", "credentials"))
			hasFallback = err == nil && strings.TrimSpace(string(data)) != ""
		}
	}

	resp := map[string]any{
		"provider":    provider,
		"configured":  configured,
		"hasFallback": hasFallback,
	}

	// Include embedding model setting for gemini
	if provider == "gemini" {
		embModel := "gemini-embedding-001" // default
		if m, err := creds.GetToken("gemini", "", "", "embedding_model"); err == nil && m != "" {
			embModel = m
		}
		resp["embeddingModel"] = embModel
	}

	writeJSONResponse(w, http.StatusOK, resp)
}

// handleSetAPIKey stores an API key for the given provider.
func (s *Server) handleSetAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "apiKey is required")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.SetToken(provider, "", "", "api_key", req.APIKey, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save API key: "+err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetEmbeddingModel saves the embedding model name and clears existing embeddings
// when the model changes (since dimensions may differ).
func (s *Server) handleSetEmbeddingModel(w http.ResponseWriter, r *http.Request) {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model is required")
		return
	}

	creds := s.agents.Credentials()

	// Check if model changed
	oldModel, _ := creds.GetToken("gemini", "", "", "embedding_model")
	modelChanged := oldModel != "" && oldModel != req.Model

	if err := creds.SetToken("gemini", "", "", "embedding_model", req.Model, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save: "+err.Error())
		return
	}

	// Clear all embeddings if model changed (dimensions may differ)
	if modelChanged {
		s.agents.ClearAllEmbeddings()
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "model": req.Model, "embeddingsCleared": modelChanged})
}

// handleDeleteAPIKey removes an API key for the given provider.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.DeleteToken(provider, "", "", "api_key"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
