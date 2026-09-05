package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// PreserveNativeGoalsOnShutdown distinguishes daemon shutdown from user stop.
// It must run before cancellation of any active response surface.
func (m *Manager) PreserveNativeGoalsOnShutdown() { m.goalShutdown.Store(true) }

// RecoverableGoals only enumerates already-authorized active bindings. It does
// not read commands out of checkpoints or infer goals from ordinary chats.
func (m *Manager) RecoverableGoals() map[string][]GoalBinding {
	out := map[string][]GoalBinding{}
	for _, a := range m.List() {
		if a.Archived || a.Tool != ToolCodex {
			continue
		}
		entries, err := os.ReadDir(codexThreadRefDir(a.ID))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !validCodexThreadRefName(entry.Name()) {
				continue
			}
			ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(a.ID), entry.Name()))
			if err != nil || ref.Goal == nil || ref.Goal.DesiredPaused || ref.Goal.State == nil || ref.Goal.State.Status != "active" {
				continue
			}
			if codexThreadRefName(ref.Goal.SessionKey) != entry.Name() {
				continue
			}
			if _, running := codexGoalRuntimes.Load(codexThreadRefPath(a.ID, ref.Goal.SessionKey)); running {
				continue
			}
			out[a.ID] = append(out[a.ID], *ref.Goal)
		}
	}
	return out
}

func (m *Manager) SetGoalRecoveryPaused(id, key string) error {
	b, err := goalBindingFor(id, key)
	if err != nil {
		return err
	}
	if b == nil {
		return errors.New("goal binding missing")
	}
	return updateGoalBinding(id, key, func(b *GoalBinding) { b.DesiredPaused = true })
}

// Explicit transport metadata for trusted arrival paths, not parsed prose.
type goalRequestContextKey struct{}

func (m *Manager) SetNativeGoalLifecycle(ctx context.Context) { m.goalLifecycle.Store(&ctx) }
func (m *Manager) NativeGoalsShuttingDown() bool {
	ctx := m.goalLifecycle.Load()
	return m.goalShutdown.Load() || (ctx != nil && (*ctx).Err() != nil)
}

func (m *Manager) FenceGoalRun(id, key, runID, origin string) error {
	if runID == "" {
		return errors.New("goal run id required")
	}
	if raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key)); ok {
		r := raw.(*codexGoalRuntime)
		r.mu.Lock()
		if r.runID != runID || r.origin != origin {
			r.mu.Unlock()
			return errors.New("goal run changed or origin mismatch")
		}
		r.stopRequested = true
		r.mu.Unlock()
	}
	b, err := goalBindingFor(id, key)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	} // setup checks the runtime fence before activation
	if b.RunID != runID || b.OriginPeerID != origin {
		return errors.New("goal run changed or origin mismatch")
	}
	return updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.RunID == runID {
			b.DesiredPaused = true
			b.RecoveryPending = false
			b.Generation++
		}
	})
}

func (m *Manager) ClaimGoalRecovery(id, key string, generation int64) bool {
	claimed := false
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.Generation != generation || b.DesiredPaused {
			return
		}
		if b.RecoveryAttempts >= 3 {
			b.DesiredPaused = true
			b.RecoveryPending = false
			return
		}
		b.RecoveryAttempts++
		b.RecoveryPending = true
		claimed = true
	})
	return claimed && err == nil
}

func goalArrivalContext(ctx context.Context, id string) context.Context {
	b, err := goalBindingFor(id, "")
	if err != nil || b == nil || b.DesiredPaused || b.State == nil || b.State.Status != "active" {
		return ctx
	}
	generation := b.Generation
	return context.WithValue(ctx, goalRequestContextKey{}, &GoalRequest{Action: "resume", ExpectedThreadID: b.State.ThreadID, ExpectedGeneration: &generation})
}
