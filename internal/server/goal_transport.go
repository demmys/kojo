package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/loppo-llc/kojo/internal/store"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
)

type goalStopRequest struct {
	SessionKey string `json:"sessionKey"`
	RunID      string `json:"runId"`
}

// A socket closing is not a stop instruction. The origin explicitly fences
// the exact admitted run before cancelling its stream; daemon shutdown omits
// this RPC so the holder can reconnect the persisted goal after origin return.
func (s *Server) handleExternalGoalStop(w http.ResponseWriter, r *http.Request) {
	if !s.externalChatPeerAllowed(w, r) {
		return
	}
	var q goalStopRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&q); err != nil || len(q.RunID) != 64 || q.SessionKey == "" {
		writeError(w, 400, "bad_request", "sessionKey and goal run nonce required")
		return
	}
	p := auth.FromContext(r.Context())
	if err := s.agents.FenceGoalRun(r.PathValue("id"), q.SessionKey, q.RunID, p.PeerID); err != nil {
		writeError(w, 409, "goal_changed", err.Error())
		return
	}
	writeJSONResponse(w, 200, map[string]bool{"paused": true})
}

func (r *externalChatRouter) fenceRemoteGoalRun(id, holder, key, runID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, err := r.server.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return err
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(goalStopRequest{SessionKey: key, RunID: runID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/agents/"+id+"/external-chat/goal-stop", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("goal stop HTTP %d", resp.StatusCode)
	}
	return nil
}

// Stop tombstones stay on the response-surface owner even if the holder is
// unreachable. Recovery checks again at dispatch, after any FIFO wait.
func (s *Server) recordGoalStop(id, run string) error {
	s.goalStopFences.Store(id+"/"+run, struct{}{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.agents.Store().PutKV(ctx, &store.KVRecord{Namespace: "native-goal-stops", Key: id + "/" + run, Value: "stopped", Type: store.KVTypeString, Scope: store.KVScopeMachine}, store.KVPutOptions{})
	return err
}
func (s *Server) checkGoalStop(ctx context.Context, id, run string) error {
	if run == "" {
		return nil
	}
	if _, stopped := s.goalStopFences.Load(id + "/" + run); stopped {
		return errors.New("goal was stopped at its origin; explicit resume required")
	}
	if s.agents == nil || s.agents.Store() == nil {
		return errors.New("goal stop store unavailable")
	}
	_, err := s.agents.Store().GetKV(ctx, "native-goal-stops", id+"/"+run)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("goal was stopped at its origin; explicit resume required")
}
