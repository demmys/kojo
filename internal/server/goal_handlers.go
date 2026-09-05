package server

import (
	"github.com/loppo-llc/kojo/internal/auth"
	"net/http"
)

// Snapshot reads never resume a CLI. The native event stream refreshes it;
// explicit !goal status queries the authoritative native store when idle.
func (s *Server) handleGetGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !auth.FromContext(r.Context()).CanReadFull(id) {
		writeError(w, 403, "forbidden", "not permitted")
		return
	}
	goal, err := s.agents.GoalSnapshot(id, r.URL.Query().Get("sessionKey"))
	if err != nil {
		writeError(w, 400, "goal_unavailable", err.Error())
		return
	}
	writeJSONResponse(w, 200, goal)
}
