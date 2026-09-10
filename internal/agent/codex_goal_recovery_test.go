package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
)

func TestGoalArrivalCarriesGuardAndRespectsPause(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: tid, Goal: &GoalBinding{Generation: 9, State: &CodexGoal{ThreadID: tid, Status: "active"}}}, slog.Default())
	ctx := goalArrivalContext(context.Background(), id)
	q, ok := ctx.Value(goalRequestContextKey{}).(*GoalRequest)
	if !ok || q.ExpectedThreadID != tid || *q.ExpectedGeneration != 9 {
		t.Fatalf("missing arrival guard: %+v", q)
	}
	info, err := os.Stat(codexThreadRefPath(id, ""))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private ref: %v %v", info, err)
	}
	_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.DesiredPaused = true })
	if goalArrivalContext(context.Background(), id).Value(goalRequestContextKey{}) != nil {
		t.Fatal("paused goal resumed")
	}
}

func TestGoalStopFencesExactRunBeforeActivation(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	r := &codexGoalRuntime{agentID: id, key: "key", runID: "run-1", origin: "hub"}
	path := codexThreadRefPath(id, "key")
	codexGoalRuntimes.Store(path, r)
	defer codexGoalRuntimes.Delete(path)
	m := &Manager{}
	if err := m.FenceGoalRun(id, "key", "run-1", "wrong-peer"); err == nil {
		t.Fatal("foreign peer stopped run")
	}
	if r.stopRequested {
		t.Fatal("foreign peer changed fence")
	}
	if err := m.FenceGoalRun(id, "key", "run-1", "hub"); err != nil {
		t.Fatal(err)
	}
	if !r.stopRequested {
		t.Fatal("pre-activation stop lost")
	}
}

func TestFailedGoalControlDoesNotBecomeSuccessfulDedup(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	writeCodexThreadRef(id, "key", codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e"}, slog.Default())
	r := &codexGoalRuntime{agentID: id, key: "key", ready: true, write: func(string, any) (int64, error) { return 0, errors.New("broken pipe") }}
	_, err := r.control(context.Background(), &GoalRequest{Action: "pause", OperationID: "slack:1"})
	if err == nil {
		t.Fatal("expected RPC failure")
	}
	b, _ := goalBindingFor(id, "key")
	if goalOperationSeen(b, "slack:1") {
		t.Fatal("failed RPC recorded as successful")
	}
	if !b.DesiredPaused {
		t.Fatal("stop intent lost on failed RPC")
	}
}

func TestGoalShutdownDetectsCancelledLifecycleImmediately(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	m.SetNativeGoalLifecycle(ctx)
	cancel()
	if !m.NativeGoalsShuttingDown() {
		t.Fatal("shutdown raced transport cancellation")
	}
}
