package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
)

type goalRecoveryRequest struct {
	UserID     string `json:"userId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey"`
	ThreadID   string `json:"threadId"`
	Generation int64  `json:"generation"`
	HolderID   string `json:"holderId"`
}

// RecoverNativeGoals is called once after listeners and ownership pruning are
// ready. A lost request is NOT replayed: query/explicit resume remains possible.
func (s *Server) RecoverNativeGoals() { s.recoverNativeGoals("", "", false) }

// Retry detached response surfaces, not arbitrary failed tool executions.
func (s *Server) RunNativeGoalRecovery(ctx context.Context) {
	s.RecoverNativeGoals()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recoverNativeGoals("", "", true)
		}
	}
}
func (s *Server) recoverNativeGoals(onlyID, excludeKey string, pendingOnly bool) {
	if s.agents == nil || s.agents.NativeGoalsShuttingDown() {
		return
	}
	for id, bindings := range s.agents.RecoverableGoals() {
		if onlyID != "" && id != onlyID {
			continue
		}
		for _, b := range bindings {
			if onlyID != "" && b.SessionKey == excludeKey {
				continue
			}
			if pendingOnly && !b.RecoveryPending {
				continue
			}
			if !s.agents.ClaimGoalRecovery(id, b.SessionKey, b.Generation) {
				continue
			}
			req := goalRecoveryRequest{UserID: b.UserID, RunID: b.RunID, AgentID: id, SessionKey: b.SessionKey, ThreadID: b.State.ThreadID, Generation: b.Generation}
			if s.peerID != nil {
				req.HolderID = s.peerID.DeviceID
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			var err error
			if b.OriginPeerID != "" && b.OriginPeerID != req.HolderID {
				err = s.requestGoalRecovery(ctx, b.OriginPeerID, req)
			} else {
				err = s.resumeGoalSurface(ctx, req)
			}
			cancel()
			if err != nil {
				s.logger.Warn("native goal recovery not admitted; explicit resume available", "agent", id, "sessionKey", b.SessionKey, "err", err)
			}
		}
	}
}
func (s *Server) requestGoalRecovery(ctx context.Context, origin string, req goalRecoveryRequest) error {
	rec, err := s.agents.Store().GetPeer(ctx, origin)
	if err != nil {
		return err
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/peers/goals/resume", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := peer.NoKeepAliveHTTPClient(20 * time.Second).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("goal recovery HTTP %d", response.StatusCode)
	}
	return nil
}
func (s *Server) handlePeerGoalResume(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsPeer() {
		writeError(w, 403, "forbidden", "peer or owner required")
		return
	}
	var req goalRecoveryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if p.IsPeer() && p.PeerID != req.HolderID {
		writeError(w, 403, "forbidden", "holder identity mismatch")
		return
	}
	if s.externalChat == nil {
		writeError(w, 503, "unavailable", "no external chat router")
		return
	}
	holder, local, err := s.externalChat.initialRoute(r.Context(), req.AgentID)
	if err != nil || local || holder != req.HolderID {
		writeError(w, 409, "wrong_holder", "goal recovery must come from the current remote holder")
		return
	}
	if err = s.resumeGoalSurface(r.Context(), req); err != nil {
		writeError(w, 409, "recovery_unavailable", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
}
func (s *Server) resumeGoalSurface(ctx context.Context, req goalRecoveryRequest) error {
	if len(req.UserID) > 64 || strings.Trim(req.UserID, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return errors.New("invalid recovery user identity")
	}
	if err := s.checkGoalStop(ctx, req.AgentID, req.RunID); err != nil {
		return err
	}
	command := fmt.Sprintf("!goal resume-if %s %d", req.ThreadID, req.Generation)
	if req.RunID != "" {
		command += " " + req.RunID
	}
	q, err := agent.ParseGoalCommand(command)
	if err != nil || q == nil {
		return errors.New("invalid native goal recovery identity")
	}
	if strings.HasPrefix(req.SessionKey, "groupdm:") {
		return s.agents.WakeThread(req.AgentID, req.SessionKey, command)
	}
	if strings.HasPrefix(req.SessionKey, req.AgentID+":slack:") {
		if s.slackHub == nil || !s.slackHub.ResumeGoal(req.AgentID, req.SessionKey+"\n"+req.UserID+"\n"+command) {
			return errors.New("Slack bot unavailable")
		}
		return nil
	}
	if req.SessionKey != "" {
		return errors.New("unsupported goal response surface")
	}
	events, err := s.agents.Chat(context.Background(), req.AgentID, command, "user", nil)
	if err != nil {
		return err
	}
	go func() {
		for range events {
		}
	}()
	return nil
}
