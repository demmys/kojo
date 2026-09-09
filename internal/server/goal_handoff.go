package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// This journal is intentionally not a job queue: an interrupted transfer is
// never replayed at startup. The operator must inspect current ownership first.
const goalHandoffNamespace = "goal-handoffs"

type goalHandoffOperation struct {
	ID           string    `json:"id"`
	AgentID      string    `json:"agent_id"`
	SessionKey   string    `json:"session_key"`
	Source       string    `json:"source"`
	Target       string    `json:"target"`
	FencingToken int64     `json:"fencing_token"`
	Phase        string    `json:"phase"`
	Error        string    `json:"error,omitempty"`
	Origin       string    `json:"origin"`
	CreatedAt    time.Time `json:"created_at"`
}
type goalHandoffExecutionKey struct{}

func (s *Server) saveGoalHandoff(ctx context.Context, op *goalHandoffOperation) error {
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = s.agents.Store().PutKV(ctx, &store.KVRecord{Namespace: goalHandoffNamespace, Key: op.ID, Value: string(b), Type: store.KVTypeJSON, Scope: store.KVScopeMachine}, store.KVPutOptions{})
	return err
}
func (s *Server) targetSupportsGoalHandoff(ctx context.Context, addr, id string) bool {
	body, _ := json.Marshal(peerAgentSyncStateRequest{SourceDeviceID: s.peerID.DeviceID, AgentID: id})
	req, err := http.NewRequestWithContext(ctx, "POST", addr+"/api/v1/peers/agent-sync/state", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200 && resp.Header.Get("X-Kojo-Goal-Handoff") == "v1"
}
func (s *Server) queueGoalHandoff(w http.ResponseWriter, r *http.Request, id, target, addr string) {
	key := strings.TrimSpace(r.Header.Get("X-Kojo-Session-Key"))
	if len(key) > 1024 {
		writeError(w, 400, "bad_request", "session key too long")
		return
	}
	if key != "" && !strings.HasPrefix(key, id+":slack:") {
		writeError(w, 409, "unsupported_surface", "Goal handoff currently supports Slack and main WebUI only")
		return
	}
	if !s.targetSupportsGoalHandoff(r.Context(), addr, id) {
		writeError(w, 409, "goal_handoff_unsupported", "Target must support safe Goal handoff; update both peers before migrating a running Goal")
		return
	}
	origin, err := agent.GoalHandoffOrigin(id, key)
	if err != nil {
		writeError(w, 409, "goal_changed", err.Error())
		return
	}
	if key != "" && origin != "" && origin != s.peerID.DeviceID && origin != target {
		originRec, err := s.agents.Store().GetPeer(r.Context(), origin)
		if err != nil {
			writeError(w, 409, "origin_unavailable", err.Error())
			return
		}
		originAddr, err := peer.NormalizeAddress(originRec.URL)
		if err != nil || !s.targetSupportsGoalHandoff(r.Context(), originAddr, id) {
			writeError(w, 409, "goal_handoff_unsupported", "The original Slack Hub must also support safe Goal handoff")
			return
		}
	}
	lock, err := s.agents.Store().GetAgentLock(r.Context(), id)
	if err != nil || lock.HolderPeer != s.peerID.DeviceID {
		writeError(w, 409, "wrong_source", "source ownership changed")
		return
	}
	op := &goalHandoffOperation{ID: uuid.NewString(), AgentID: id, SessionKey: key, Source: s.peerID.DeviceID, Target: target, FencingToken: lock.FencingToken, Phase: "queued", CreatedAt: time.Now().UTC()}
	binding, err := agent.GoalHandoffBinding(id, key)
	if err != nil || binding == nil {
		writeError(w, 409, "goal_changed", "goal disappeared")
		return
	}
	if origin == "" {
		origin = s.peerID.DeviceID
	}
	op.Origin = origin
	capability := ""
	if key != "" {
		caller, ok := s.agents.InFlightOneShotOrigin(id, key)
		if !ok || caller.HandoffCapability == "" {
			writeError(w, 409, "origin_unavailable", "source adapter completion capability missing")
			return
		}
		capability = caller.HandoffCapability
	}
	if err = s.saveGoalHandoff(r.Context(), op); err != nil {
		writeError(w, 500, "internal", err.Error())
		return
	}
	if err = s.callGoalHandoffOrigin(r.Context(), origin, goalHandoffOriginRequest{Capability: capability, Action: "register", OpID: op.ID, AgentID: id, Source: op.Source, Target: target, SessionKey: key, UserID: binding.UserID, RunID: binding.RunID}); err != nil {
		op.Phase = "failed"
		op.Error = err.Error()
		_ = s.saveGoalHandoff(context.Background(), op)
		writeError(w, 409, "origin_unavailable", err.Error())
		return
	}
	done, err := s.agents.QueueGoalHandoff(id, key, agent.GoalHandoff{ID: op.ID, SourcePeerID: op.Source, TargetPeerID: target})
	if err != nil {
		op.Phase = "failed"
		op.Error = err.Error()
		_ = s.saveGoalHandoff(context.Background(), op)
		writeError(w, 409, "goal_changed", err.Error())
		return
	}
	// Retain only authenticated request metadata; the new execution context has
	// no caller-disconnection lifetime and cannot be supplied through HTTP.
	next := r.Clone(context.WithoutCancel(r.Context()))
	go s.runGoalHandoff(next, op, done)
	writeJSONResponse(w, 202, map[string]string{"outcome": "queued", "op_id": op.ID, "reason": "Finish this turn now. Do not poll or invoke switch again from this turn. Kojo will checkpoint, transfer, and resume the same Goal after the turn ends. Acceptance is not transfer completion."})
}
func (s *Server) runGoalHandoff(r *http.Request, op *goalHandoffOperation, done <-chan error) {
	fail := func(err error) {
		op.Phase = "failed"
		op.Error = err.Error()
		_ = s.saveGoalHandoff(context.Background(), op)
		// Never mutate source's goal after ownership has moved.
		if l, e := s.agents.Store().GetAgentLock(context.Background(), op.AgentID); e == nil && l.HolderPeer == op.Source && l.FencingToken == op.FencingToken {
			_ = s.agents.FailGoalHandoff(op.AgentID, op.SessionKey, op.ID, err.Error())
		} else {
			op.Phase = "ownership_or_resume_uncertain"
			_ = s.saveGoalHandoff(context.Background(), op)
		}
		s.logger.Warn("goal handoff requires operator attention", "agent", op.AgentID, "op", op.ID, "error", err)
	}
	timer := time.NewTimer(15 * time.Minute)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			fail(err)
			return
		}
	case <-timer.C:
		fail(errors.New("goal did not reach a clean checkpoint before deadline"))
		return
	}
	if s.agents.NativeGoalsShuttingDown() {
		fail(errors.New("daemon is shutting down; handoff not replayed"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.agents.WaitChatIdle(ctx, op.AgentID); err != nil {
		fail(err)
		return
	}
	if err := s.callGoalHandoffOrigin(ctx, op.Origin, goalHandoffOriginRequest{Action: "settled", OpID: op.ID, AgentID: op.AgentID}); err != nil {
		fail(err)
		return
	}
	op.Phase = "ready"
	if err := s.saveGoalHandoff(ctx, op); err != nil {
		fail(err)
		return
	}
	body, _ := json.Marshal(switchDeviceRequest{TargetPeerID: op.Target})
	next := r.Clone(context.WithValue(context.WithoutCancel(r.Context()), goalHandoffExecutionKey{}, op))
	next.Body = http.NoBody
	next.Body = io.NopCloser(bytes.NewReader(body))
	recorder := &goalSwitchResult{header: make(http.Header)}
	// Same self principal, but a typed checkpoint execution replaces the live
	// caller exemption. Normal preflight/transfer/finalize remain shared.
	s.handleAgentHandoffSwitch(recorder, next)
	var result switchDeviceResponse
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Outcome != "completed" {
		fail(fmt.Errorf("transfer outcome requires inspection (HTTP %d): %s", recorder.Code, recorder.Body.String()))
		return
	}
	op.Phase = "resume_requested"
	if err := s.saveGoalHandoff(context.Background(), op); err != nil {
		s.logger.Error("save goal transfer result", "op", op.ID, "err", err)
	}
}

// Captures the shared orchestrator's bounded JSON response for its internal
// checkpoint caller; no owner impersonation or second HTTP request is involved.
type goalSwitchResult struct {
	header http.Header
	Code   int
	Body   bytes.Buffer
}

func (w *goalSwitchResult) Header() http.Header { return w.header }
func (w *goalSwitchResult) WriteHeader(n int) {
	if w.Code == 0 {
		w.Code = n
	}
}
func (w *goalSwitchResult) Write(b []byte) (int, error) {
	if w.Code == 0 {
		w.Code = 200
	}
	if w.Body.Len()+len(b) > 1<<20 {
		return 0, errors.New("switch result exceeds journal budget")
	}
	return w.Body.Write(b)
}
