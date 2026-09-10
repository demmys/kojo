package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

type goalHandoffOriginRequest struct {
	Capability string `json:"capability,omitempty"`
	Action     string `json:"action"`
	OpID       string `json:"op_id"`
	AgentID    string `json:"agent_id"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	SessionKey string `json:"session_key"`
	UserID     string `json:"user_id"`
	RunID      string `json:"run_id"`
}

const goalHandoffOrigins = "goal-handoff-origins"

type goalSourceBarrier interface{ WaitSourceComplete(context.Context) error }

func (s *Server) callGoalHandoffOrigin(ctx context.Context, origin string, q goalHandoffOriginRequest) error {
	if origin == s.peerID.DeviceID {
		return s.applyGoalHandoffOrigin(ctx, s.peerID.DeviceID, q)
	}
	rec, err := s.agents.Store().GetPeer(ctx, origin)
	if err != nil {
		return err
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(q)
	req, err := http.NewRequestWithContext(ctx, "POST", addr+"/api/v1/peers/goals/handoff", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("handoff origin did not authorize operation (stopped, changed, or unavailable)")
	}
	return nil
}
func (s *Server) handleGoalHandoffOrigin(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	var q goalHandoffOriginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&q); err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	signer := p.PeerID
	if p.IsOwner() && s.peerID != nil {
		signer = s.peerID.DeviceID
	}
	if err := s.applyGoalHandoffOrigin(r.Context(), signer, q); err != nil {
		writeError(w, 409, "handoff_not_authorized", err.Error())
		return
	}
	writeJSONResponse(w, 200, map[string]bool{"authorized": true})
}
func (s *Server) applyGoalHandoffOrigin(ctx context.Context, signer string, q goalHandoffOriginRequest) error {
	if len(q.OpID) != 36 || q.AgentID == "" || len(q.SessionKey) > 1024 {
		return errors.New("invalid handoff identity")
	}
	if q.Action == "register" {
		lock, err := s.agents.Store().GetAgentLock(ctx, q.AgentID)
		if err != nil || lock.HolderPeer != signer || q.Source != signer || q.Target == "" {
			return errors.New("source is not current holder")
		}
		if q.SessionKey != "" && !strings.HasPrefix(q.SessionKey, q.AgentID+":slack:") {
			return errors.New("unsupported surface")
		}
		var barrier goalSourceBarrier
		if q.SessionKey != "" {
			s.handoffArrivalMu.Lock()
			cap := s.handoffArrivalCaps[q.Capability]
			if cap != nil && cap.AgentID == q.AgentID && cap.SessionKey == q.SessionKey && !cap.TurnDone {
				barrier, _ = cap.Reservation.(goalSourceBarrier)
			}
			s.handoffArrivalMu.Unlock()
			if barrier == nil {
				return errors.New("origin adapter has no source completion barrier")
			}
		}
		q.Capability = "" // keep only the passive barrier in memory, never persist bearer capabilities
		b, _ := json.Marshal(q)
		if _, err = s.agents.Store().PutKV(ctx, &store.KVRecord{Namespace: goalHandoffOrigins, Key: q.OpID, Value: string(b), Type: store.KVTypeJSON, Scope: store.KVScopeMachine}, store.KVPutOptions{IfMatchETag: store.IfMatchAny}); err != nil {
			return err
		}
		if barrier != nil {
			s.goalHandoffBarriers.Store(q.OpID, barrier)
		}
		_, err = s.agents.Store().PutKV(ctx, &store.KVRecord{Namespace: goalHandoffOrigins, Key: q.AgentID + "/" + q.SessionKey, Value: q.OpID, Type: store.KVTypeString, Scope: store.KVScopeMachine}, store.KVPutOptions{})
		return err
	}
	rec, err := s.agents.Store().GetKV(ctx, goalHandoffOrigins, q.OpID)
	if err != nil {
		return err
	}
	var saved goalHandoffOriginRequest
	if json.Unmarshal([]byte(rec.Value), &saved) != nil || saved.AgentID != q.AgentID {
		return errors.New("handoff destination mismatch")
	}
	if q.Action == "settled" {
		if saved.Source != signer {
			return errors.New("source identity mismatch")
		}
		if saved.SessionKey != "" {
			raw, ok := s.goalHandoffBarriers.Load(q.OpID)
			if !ok {
				return errors.New("origin adapter completion unknown after restart")
			}
			if err := raw.(goalSourceBarrier).WaitSourceComplete(ctx); err != nil {
				return err
			}
		}
		if err := s.checkGoalStop(ctx, q.AgentID, q.OpID); err != nil {
			return err
		}
		_, err := s.agents.Store().PutKV(ctx, &store.KVRecord{Namespace: goalHandoffOrigins, Key: q.OpID + "/settled", Value: "true", Type: store.KVTypeString, Scope: store.KVScopeMachine}, store.KVPutOptions{})
		s.goalHandoffBarriers.Delete(q.OpID)
		return err
	}
	if saved.Target != signer {
		return errors.New("destination identity mismatch")
	}
	if _, err := s.agents.Store().GetKV(ctx, goalHandoffOrigins, q.OpID+"/settled"); err != nil {
		return errors.New("source adapter completion not recorded")
	}
	if q.Action != "check" {
		return errors.New("unsupported action")
	}
	lock, err := s.agents.Store().GetAgentLock(ctx, q.AgentID)
	if err != nil || lock.HolderPeer != saved.Target {
		return errors.New("handoff target is not current holder")
	}
	return s.checkGoalStop(ctx, q.AgentID, q.OpID)
}

// StopIdleGoal is optional on Slack's router so !stop can fence the saved move
// after the source FIFO turn has already ended. It does not create a model turn.
func (r *externalChatRouter) StopIdleGoal(ctx context.Context, id, key, user string) (bool, error) {
	s := r.server
	index, err := s.agents.Store().GetKV(ctx, goalHandoffOrigins, id+"/"+key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rec, err := s.agents.Store().GetKV(ctx, goalHandoffOrigins, index.Value)
	if err != nil {
		return true, err
	}
	var q goalHandoffOriginRequest
	if err = json.Unmarshal([]byte(rec.Value), &q); err != nil {
		return true, err
	}
	if q.UserID != user {
		return false, nil // a historical operation owner cannot veto a replacement goal
	}
	if err = s.recordGoalStop(id, q.OpID); err != nil {
		return true, err
	}
	// Tombstone is durable at the origin even if holder is in the finalize gap.
	lock, routeErr := s.agents.Store().GetAgentLock(ctx, id)
	if routeErr != nil {
		return true, routeErr
	}
	holder, local := lock.HolderPeer, lock.HolderPeer == r.selfPeerID()
	if local {
		if err := s.agents.Store().CheckFencing(ctx, id, holder, lock.FencingToken); err != nil {
			return true, err
		}
		if err := s.agents.CancelGoalHandoff(id, key, q.OpID); err != nil {
			return true, err
		}
		if err := s.agents.Store().CheckFencing(ctx, id, holder, lock.FencingToken); err != nil {
			return true, errors.New("handoff changed holders during stop; automatic resume remains fenced")
		}
		return true, nil
	}
	body, _ := json.Marshal(goalStopRequest{SessionKey: key, HandoffID: q.OpID})
	peerRec, err := s.agents.Store().GetPeer(ctx, holder)
	if err != nil {
		return true, err
	}
	addr, err := peer.NormalizeAddress(peerRec.URL)
	if err != nil {
		return true, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", addr+"/api/v1/agents/"+id+"/external-chat/goal-stop", bytes.NewReader(body))
	if err != nil {
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return true, errors.New("automatic handoff resume fenced; current holder has not confirmed stop")
	}
	return true, nil
}

// Status is read-only on the source even after it has released the agent.
func (s *Server) handleGoalHandoffStatus(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	rec, err := s.agents.Store().GetKV(r.Context(), goalHandoffNamespace, r.PathValue("op"))
	if err != nil {
		writeError(w, 404, "not_found", "handoff operation not found")
		return
	}
	var op goalHandoffOperation
	if json.Unmarshal([]byte(rec.Value), &op) != nil {
		writeError(w, 500, "internal", "invalid journal")
		return
	}
	if !p.IsOwner() {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	writeJSONResponse(w, 200, op)
}

func (s *Server) stopMainGoalHandoff(ctx context.Context, id string) error {
	b, err := agent.GoalHandoffBinding(id, "")
	if err != nil {
		return err
	}
	if b != nil && b.Handoff != nil {
		if err = s.recordGoalStop(id, b.Handoff.ID); err != nil {
			return err
		}
		if err = s.agents.CancelGoalHandoff(id, "", b.Handoff.ID); err != nil {
			return err
		}
	}
	if s.peerID != nil {
		_, err = newExternalChatRouter(s).StopIdleGoal(ctx, id, "", "")
	}
	return err
}
