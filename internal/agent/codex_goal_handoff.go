package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GoalHandoff is a portable checkpoint identity, not permission to replay a
// move. Pending handoffs always suppress ordinary/crash-driven goal recovery.
// A destination may resume only the exact finalized identity and generation.
type GoalHandoff struct {
	ID           string `json:"id"`
	SourcePeerID string `json:"sourcePeerId"`
	TargetPeerID string `json:"targetPeerId"`
	Phase        string `json:"phase"`
	Error        string `json:"error,omitempty"`
}

func (h *GoalHandoff) Pending() bool {
	return h != nil && h.Phase != "resumed" && h.Phase != "failed" && h.Phase != "cancelled"
}

type nativeGoalHandoff struct {
	requestingTurn     uint64 // protected by runtime.mu
	requestingTurnDone bool   // protected by runtime.mu
	id                 string
	done               chan error
	once               sync.Once
	checkpointed       bool // protected by runtime.mu
	cancelled          bool // protected by runtime.mu
}

func (h *nativeGoalHandoff) finish(err error) { h.once.Do(func() { h.done <- err; close(h.done) }) }

// QueueGoalHandoff only marks intent. It never interrupts the curl tool call
// requesting the move. The stream parks itself after that native turn ends.
func (m *Manager) QueueGoalHandoff(id, key string, h GoalHandoff) (<-chan error, error) {
	unlock := goalAdmissions.Lock(codexThreadRefPath(id, key))
	defer unlock()
	raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key))
	if !ok {
		return nil, errors.New("goal runner no longer active")
	}
	r := raw.(*codexGoalRuntime)
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isGoal || r.closed || !r.ready || !r.inNativeTurn || r.stopRequested || r.handoff != nil {
		return nil, errors.New("goal is not ready for a new handoff")
	}
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.DesiredPaused || b.State == nil || b.State.Status != "active" || b.Handoff.Pending() {
			return
		}
		h.Phase = "queued"
		b.Handoff = &h
		b.Generation++
		// Fail closed if the process crashes before the clean checkpoint. Internal
		// parking does not erase this fence; only finalized destination intent can.
		b.DesiredPaused = true
		b.RecoveryPending = false
	})
	if err != nil {
		return nil, err
	}
	b, err := goalBindingFor(id, key)
	if err != nil {
		return nil, err
	}
	if b == nil || b.Handoff == nil || b.Handoff.ID != h.ID || b.Handoff.Phase != "queued" {
		return nil, errors.New("goal changed or was stopped before handoff")
	}
	ticket := &nativeGoalHandoff{id: h.ID, requestingTurn: r.turnSequence, done: make(chan error, 1)}
	r.handoff = ticket
	return ticket.done, nil
}

func (r *codexGoalRuntime) wantsHandoff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handoff != nil && !r.handoff.cancelled && !r.stopRequested
}
func (r *codexGoalRuntime) checkpointHandoff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handoff != nil && !r.stopRequested {
		r.handoff.checkpointed = true
	}
}

// Called only after shutdown has waited for the child process. EOF/forced kill
// is never a valid clean checkpoint, even if the last status said "paused".
func (r *codexGoalRuntime) finishHandoff(shutdownErr error) {
	r.mu.Lock()
	h := r.handoff
	stopped := r.stopRequested
	ready := h != nil && h.checkpointed
	r.mu.Unlock()
	if h == nil {
		return
	}
	err := shutdownErr
	if err == nil && (!ready || stopped) {
		err = errors.New("goal stopped before a clean handoff checkpoint")
	}
	var diskGoal *CodexGoal
	if err == nil {
		diskGoal, err = pausedGoalFromDisk(r.threadID)
	}
	applied := false
	persistErr := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) {
		if b.Handoff == nil || b.Handoff.ID != h.id || b.Handoff.Phase != "queued" {
			return
		}
		b.DesiredPaused = true
		b.RecoveryPending = false
		b.ActivationPending = false
		if err == nil && b.State != nil && b.State.Status == "paused" {
			b.State = diskGoal
			b.Handoff.Phase = "ready"
			applied = true
		} else {
			if err == nil {
				err = errors.New("native goal did not acknowledge paused checkpoint")
			}
			b.Handoff.Phase = "failed"
			b.Handoff.Error = err.Error()
		}
	})
	if persistErr != nil {
		err = persistErr
	}
	if err == nil && !applied {
		err = errors.New("handoff cancelled or goal changed")
	}
	h.finish(err)
}

func cancelGoalHandoff(b *GoalBinding, reason string) {
	if b.Handoff.Pending() {
		b.Handoff.Phase = "cancelled"
		b.Handoff.Error = reason
	}
}

func (m *Manager) GoalHandoffCheckpoint(id, key, op string) (*GoalBinding, error) {
	b, err := goalBindingFor(id, key)
	if err != nil {
		return nil, err
	}
	if b == nil || b.Handoff == nil || b.Handoff.ID != op || b.Handoff.Phase != "ready" || b.State == nil || b.State.Status != "paused" || !b.DesiredPaused {
		return nil, errors.New("clean goal handoff checkpoint is no longer available")
	}
	return b, nil
}

func (m *Manager) FailGoalHandoff(id, key, op, reason string) error {
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.Handoff == nil || b.Handoff.ID != op || !b.Handoff.Pending() {
			return
		}
		b.Handoff.Phase = "failed"
		b.Handoff.Error = reason
		b.DesiredPaused = true
		b.RecoveryPending = false
		b.Generation++
	})
	if raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key)); ok {
		r := raw.(*codexGoalRuntime)
		r.mu.Lock()
		matching := r.handoff != nil && r.handoff.id == op && !r.closed
		if matching {
			r.stopRequested = true
		}
		r.mu.Unlock()
		if matching {
			r.cancelHandoffRuntime(reason)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = r.control(ctx, &GoalRequest{Action: "pause"})
			}()
		}
	}
	return err
}

// AcceptGoalHandoff is called on the destination only, after lock transfer and
// finalize activation. Replays preserve the original generation. A manual
// pause/clear after transfer cancels the identity and cannot be undone here.
func (m *Manager) AcceptGoalHandoff(id, key, op, source, target string) (*GoalBinding, error) {
	unlock := goalAdmissions.Lock(codexThreadRefPath(id, key))
	defer unlock()
	accepted := false
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		h := b.Handoff
		if h == nil || h.ID != op || h.SourcePeerID != source || h.TargetPeerID != target {
			return
		}
		if h.Phase == "resume_pending" || h.Phase == "resuming" || h.Phase == "resumed" {
			accepted = true
			return
		}
		if h.Phase != "ready" || b.State == nil || b.State.Status != "paused" || !b.DesiredPaused {
			return
		}
		h.Phase = "resume_pending"
		accepted = true
	})
	if err != nil {
		return nil, err
	}
	if !accepted {
		return nil, fmt.Errorf("goal handoff %s changed or was cancelled", op)
	}
	return goalBindingFor(id, key)
}
func goalHandoffResumeAllowed(ref *codexThreadRef, q *GoalRequest) bool {
	if ref == nil || ref.Goal == nil || q == nil || q.ExpectedHandoffID == "" || q.ExpectedGeneration == nil {
		return false
	}
	b := ref.Goal
	return b.Handoff != nil && b.Handoff.ID == q.ExpectedHandoffID && b.Handoff.Phase == "resume_pending" && b.Generation == *q.ExpectedGeneration && ref.ThreadID == q.ExpectedThreadID && b.State != nil && b.State.Status == "paused"
}

// PendingGoalHandoff reads the portable fence, including after a daemon restart.
func PendingGoalHandoff(id string) (*GoalBinding, error) {
	entries, err := os.ReadDir(codexThreadRefDir(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !validCodexThreadRefName(e.Name()) {
			continue
		}
		ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(id), e.Name()))
		if err != nil {
			return nil, err
		}
		if ref.Goal != nil && ref.Goal.Handoff.Pending() {
			return ref.Goal, nil
		}
	}
	return nil, nil
}

// Controls stay available while queued, but replies/new goals must not race a
// reserved checkpoint. Explicit resume is allowed only after cancel/pause.
func checkGoalHandoffAdmission(id, key string, q *GoalRequest) error {
	b, err := PendingGoalHandoff(id)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	if b.SessionKey == key && q != nil {
		if q.Action == "status" || q.Action == "pause" || q.Action == "clear" {
			return nil
		}
		if q.ExpectedHandoffID == b.Handoff.ID && b.Handoff.Phase == "resume_pending" {
			return nil
		}
	}
	return errors.New("Goal handoff pending; finish the initiating turn, or use !goal pause in that conversation to cancel before starting new work")
}

func pausedGoalFromDisk(tid string) (*CodexGoal, error) {
	tr, err := readCodexGoalTransfer(tid)
	if err != nil {
		return nil, err
	}
	if tr == nil || tr.Row == nil {
		return nil, errors.New("native goal checkpoint missing")
	}
	if err := validateGoalRow(tr.Row, tid); err != nil {
		return nil, err
	}
	g := &CodexGoal{ThreadID: tid}
	for i, c := range tr.Row.Columns {
		v := tr.Row.Values[i]
		switch c {
		case "status":
			g.Status = v.Text
		case "objective":
			g.Objective = v.Text
		case "token_budget":
			if v.Type == "int" {
				n := v.Int
				g.TokenBudget = &n
			}
		case "tokens_used":
			g.TokensUsed = v.Int
		case "time_used_seconds":
			g.TimeUsedSeconds = v.Int
		case "updated_at_ms":
			g.UpdatedAt = v.Int
		}
	}
	if g.Status != "paused" {
		return nil, errors.New("native database did not commit a paused goal")
	}
	return g, nil
}

func GoalHandoffOrigin(id, key string) (string, error) {
	b, err := goalBindingFor(id, key)
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", errors.New("goal binding missing")
	}
	return b.OriginPeerID, nil
}
func goalHandoffSummary(id, key string) string {
	b, err := goalBindingFor(id, key)
	if err != nil || b == nil || b.Handoff == nil {
		return ""
	}
	h := b.Handoff
	result := fmt.Sprintf("\nMove: %s · %s → %s\nOperation: %s", h.Phase, h.SourcePeerID, h.TargetPeerID, h.ID)
	if h.Error != "" {
		result += "\n" + h.Error
	}
	if h.Pending() {
		result += "\nUse !goal pause to cancel before transfer; do not force-reclaim or retry an uncertain move."
	}
	return result
}

// cancelHandoffRuntime is separate from stopping native work: the caller's
// pause/clear RPC still owns the normal drain and reply.
func (r *codexGoalRuntime) cancelHandoffRuntime(reason string) {
	r.mu.Lock()
	h := r.handoff
	if h != nil {
		h.cancelled = true
	}
	r.mu.Unlock()
	if h != nil {
		h.finish(errors.New(reason))
	}
}

func GoalHandoffBinding(id, key string) (*GoalBinding, error) { return goalBindingFor(id, key) }

// CancelGoalHandoff fences a specific move even between runner completion and
// resume admission. An old operation can never stop a later explicit resume.
func (m *Manager) CancelGoalHandoff(id, key, op string) error {
	raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key))
	var r *codexGoalRuntime
	if ok {
		r = raw.(*codexGoalRuntime)
		r.mu.Lock()
	}
	runtimeMatches := r != nil && (r.resumeHandoffID == op || (r.handoff != nil && r.handoff.id == op))
	matched := false
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.Handoff == nil || b.Handoff.ID != op {
			return
		}
		matched = true
		b.Handoff.Phase = "cancelled"
		b.Handoff.Error = "operator stopped the handoff"
		b.DesiredPaused = true
		b.RecoveryPending = false
		b.Generation++
		if runtimeMatches {
			r.stopRequested = true
			if r.handoff != nil {
				r.handoff.cancelled = true
				r.handoff.finish(errors.New("operator stopped handoff"))
			}
		}
	})
	if r != nil {
		r.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if !matched {
		return errors.New("goal handoff changed")
	}
	if runtimeMatches {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				r.mu.Lock()
				ready, closed := r.ready, r.closed
				r.mu.Unlock()
				if closed {
					return
				}
				if ready {
					_, _ = r.control(ctx, &GoalRequest{Action: "pause"})
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	return nil
}

func (r *codexGoalRuntime) handoffReadyToPark() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handoff != nil && !r.handoff.cancelled && !r.stopRequested && r.handoff.requestingTurnDone
}
func (r *codexGoalRuntime) nativeTurnStarted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnSequence++
	r.inNativeTurn = true
}
func (r *codexGoalRuntime) nativeTurnCompleted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handoff != nil && r.handoff.requestingTurn == r.turnSequence {
		r.handoff.requestingTurnDone = true
	}
	r.inNativeTurn = false
}
