package server

import (
	"encoding/json"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// --- Web Push Handlers ---

func (s *Server) handleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{
		"publicKey": s.notify.VAPIDPublicKey(),
	})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid subscription")
		return
	}
	s.notify.Subscribe(&sub)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	s.notify.Unsubscribe(req.Endpoint)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}
