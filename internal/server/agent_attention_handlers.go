package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/loppo-llc/kojo/internal/auth"
)

// attentionBodyCap caps POST /attention. The only field is a one-line
// reason the manager truncates anyway, so anything past 8 KiB is
// certainly a client bug.
const attentionBodyCap = 8 << 10

// attentionRequest is the POST body. `reason` is optional — a bare
// `{}` (or no body at all) raises a page with no note.
type attentionRequest struct {
	Reason string `json:"reason"`
}

// attentionResponse is returned by both POST and DELETE so a client
// always learns the resulting state without a follow-up GET.
type attentionResponse struct {
	Attention bool   `json:"attention"`
	Reason    string `json:"reason,omitempty"`
	At        int64  `json:"at,omitempty"`
	// Cleared is only meaningful on DELETE: false means there was no
	// outstanding page (the dashboard clears on every chat open, so
	// that's the common case). Deliberately NOT omitempty — "there was
	// nothing to clear" is the interesting answer, and omitting it would
	// make it indistinguishable from an older server that never sent the
	// field.
	Cleared bool `json:"cleared"`
}

// readOptionalJSONBody decodes the request body into dst, treating an
// absent or blank body as the zero value rather than a 400. POST
// /attention takes an entirely optional payload ("page me, no note"), and
// a chunked request without a body reports ContentLength == -1, so an
// emptiness check has to happen after the read, not before it.
// It writes the error response itself and returns false on failure.
func readOptionalJSONBody(w http.ResponseWriter, r *http.Request, capBytes int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, capBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 8 KiB cap")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

// handleRaiseAgentAttention POST /api/v1/agents/{id}/attention
//
// The non-blocking counterpart to AskUserQuestion: the agent flags itself
// as wanting the operator's eyes and keeps running. The dashboard row
// highlights until the operator opens the chat. Self-scoped for agent
// tokens (an agent may only page on its own behalf); owner and peer
// principals may raise for any agent.
func (s *Server) handleRaiseAgentAttention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only raise attention for themselves")
		return
	}
	var req attentionRequest
	if !readOptionalJSONBody(w, r, attentionBodyCap, &req) {
		return
	}
	reason, at := s.agents.RaiseAttention(id, req.Reason)
	writeJSONResponse(w, http.StatusOK, attentionResponse{
		Attention: true,
		Reason:    reason,
		At:        at.UnixMilli(),
	})
}

// handleClearAgentAttention DELETE /api/v1/agents/{id}/attention
//
// Idempotent. Called by the web UI whenever the operator has the agent's
// chat open, and available to the agent itself so it can retract a page
// it no longer needs.
func (s *Server) handleClearAgentAttention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanMutateSelf(id) {
		writeError(w, http.StatusForbidden, "forbidden", "agents may only clear their own attention flag")
		return
	}
	cleared := s.agents.ClearAttention(id)
	writeJSONResponse(w, http.StatusOK, attentionResponse{Attention: false, Cleared: cleared})
}
