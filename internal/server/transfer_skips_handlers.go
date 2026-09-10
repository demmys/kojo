package server

import (
	"errors"
	"net/http"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

type dismissTransferSkipsResponse struct {
	Dismissed bool `json:"dismissed"`
}

// handleDismissTransferSkips POST /api/v1/agents/{id}/transfer-skips/dismiss
//
// Acknowledges the latest device-transfer loss notice. This only hides the
// owner-facing warning; it does not delete or alter any session file.
// Idempotent so a repeated click/retry is harmless.
func (s *Server) handleDismissTransferSkips(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "transfer notices are owner-only")
		return
	}
	generation := r.URL.Query().Get("generation")
	if generation == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "generation query parameter required")
		return
	}

	release := s.agents.LockPatch(id)
	defer release()
	dismissed, err := s.agents.DismissTransferSkips(id, generation)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, agent.ErrTransferSkipsChanged):
			writeError(w, http.StatusConflict, "transfer_skips_changed",
				"a newer transfer warning replaced the one being dismissed")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, dismissTransferSkipsResponse{Dismissed: dismissed})
}
