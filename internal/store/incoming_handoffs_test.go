package store

import (
	"context"
	"errors"
	"testing"
)

func prepareIncomingTest(t *testing.T, s *Store, id, op, source, target string) error {
	t.Helper()
	ctx := context.Background()
	v, err := s.GetAgentLockVersion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := prepareIncomingHandoffTx(ctx, tx, &IncomingHandoff{AgentID: id, OpID: op, SourcePeer: source, TargetPeer: target, Expected: v}); err != nil {
		return err
	}
	return tx.Commit()
}

func TestIncomingHandoffQuickRoundTrip(t *testing.T) {
	ctx := context.Background()
	windows, hub := openTestStore(t), openTestStore(t)
	id := agentLockTestSetup(t, windows)
	if agentLockTestSetup(t, hub) != id {
		t.Fatal("different ids")
	}
	const lease = int64(300000)
	initial, err := windows.AcquireAgentLock(ctx, id, "windows", NowMillis(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, hub, id, "out", "windows", "hub"); err != nil {
		t.Fatal(err)
	}
	out, err := windows.CompleteHandoffSelectedBlobs(ctx, id, "hub", nil, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.ReleaseAgentLock(ctx, id, "windows", initial.FencingToken); !errors.Is(err, ErrFencingMismatch) {
		t.Fatalf("old release: %v", err)
	}
	accepted, err := hub.AcceptIncomingHandoff(ctx, id, "out", "windows", "hub", "hub", NowMillis(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.ActivateIncomingHandoff(ctx, id, "out"); err != nil {
		t.Fatal(err)
	}
	if err := hub.FinishIncomingHandoff(ctx, id, "out"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, windows, id, "back", "hub", "windows"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.CompleteHandoffSelectedBlobs(ctx, id, "windows", nil, lease); err != nil {
		t.Fatal(err)
	}
	stale, err := windows.GetAgentLock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stale.HolderPeer != "hub" || stale.LeaseExpiresAt <= NowMillis() {
		t.Fatalf("no live outgoing shadow: %+v", stale)
	}
	if _, err := windows.AcquireAgentLock(ctx, id, "windows", stale.LeaseExpiresAt+1, lease); !errors.Is(err, ErrIncomingHandoffPending) {
		t.Fatalf("guard bypassed prepared handoff: %v", err)
	}
	back, err := windows.AcceptIncomingHandoff(ctx, id, "back", "hub", "windows", "hub", NowMillis(), lease)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := windows.GetAgentLock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if lock.HolderPeer != "windows" || lock.AllowedProxyPeer != "hub" || lock.FencingToken <= out.Lock.FencingToken || back.AcceptedToken != lock.FencingToken {
		t.Fatalf("bad accepted lock: %+v", lock)
	}
	if _, err := windows.AcquireAgentLock(ctx, id, "windows", NowMillis(), lease); err != nil {
		t.Fatal(err)
	}
	retry, err := windows.AcceptIncomingHandoff(ctx, id, "back", "hub", "windows", "hub", NowMillis(), lease)
	if err != nil || retry.AcceptedToken != back.AcceptedToken {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if err := windows.ActivateIncomingHandoff(ctx, id, "back"); err != nil {
		t.Fatal(err)
	}
	if err := windows.FinishIncomingHandoff(ctx, id, "back"); err != nil {
		t.Fatal(err)
	}
	t.Logf("immediate round trip: Hub local token=%d, Windows local token=%d; proxy=hub", accepted.AcceptedToken, lock.FencingToken)
}

func TestIncomingHandoffRejectsReclaimReplay(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "source", NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, s, id, "old", "source", "target"); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ForceReclaimAgentToLocal(ctx, id, "target", NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptIncomingHandoff(ctx, id, "old", "source", "target", "source", NowMillis(), 300000); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("late finalize: %v", err)
	}
	if err := prepareIncomingTest(t, s, id, "old", "source", "target"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("old phase-1: %v", err)
	}
	lock, err := s.GetAgentLock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if lock.FencingToken != reclaimed.FencingToken || lock.AllowedProxyPeer != "target" {
		t.Fatalf("reclaim clobbered: %+v", lock)
	}
}

func TestIncomingHandoffBindsSourceAndResumesAcceptedGap(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	id := agentLockTestSetup(t, s)
	if err := prepareIncomingTest(t, s, id, "op", "source", "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptIncomingHandoff(ctx, id, "op", "stranger", "target", "stranger", NowMillis(), 300000); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("wrong source: %v", err)
	}
	a, err := s.AcceptIncomingHandoff(ctx, id, "op", "source", "target", "hub", NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAgentLock(ctx, id, "target", a.AcceptedToken); err != nil {
		t.Fatal(err)
	}
	incomplete, err := s.IncompleteIncomingHandoffs(ctx)
	if err != nil || !incomplete[id] {
		t.Fatalf("restart lost pending fence: %v %v", incomplete, err)
	}
	b, err := s.AcceptIncomingHandoff(ctx, id, "op", "source", "target", "hub", NowMillis(), 300000)
	if err != nil || b.AcceptedToken <= a.AcceptedToken {
		t.Fatalf("resume gap: %+v %v", b, err)
	}
	if _, err := s.AcceptIncomingHandoff(ctx, id, "op", "source", "target", "other", NowMillis(), 300000); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("proxy changed on retry: %v", err)
	}
	if err := s.ActivateIncomingHandoff(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishIncomingHandoff(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, s, id, "op", "source", "target"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("terminal op resurrected: %v", err)
	}
}

func TestIncomingHandoffEarlyDropAndUnknownOldPhase1(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	// Cancellation must work even before the agent row exists.
	const unknown = "ag_not_arrived"
	if err := s.AbortIncomingHandoff(ctx, unknown, "cancelled", "source"); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortIncomingHandoff(ctx, unknown, "cancelled", "other"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("wrong cancel source: %v", err)
	}
	if err := s.ValidateIncomingHandoff(ctx, &IncomingHandoff{AgentID: unknown, OpID: "cancelled", SourcePeer: "source", TargetPeer: "target"}); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("cancelled phase1: %v", err)
	}
	id := agentLockTestSetup(t, s)
	if err := prepareIncomingTest(t, s, id, "newer", "source", "target"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, s, id, "delayed-unknown-old", "source", "target"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("unknown op superseded pending: %v", err)
	}
	if err := s.AbortIncomingHandoff(ctx, id, "newer", "source"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, s, id, "next", "source", "target"); err != nil {
		t.Fatal(err)
	}
	if err := prepareIncomingTest(t, s, id, "newer", "source", "target"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("cancelled op resurrected: %v", err)
	}
	if _, err := s.AcceptIncomingHandoff(ctx, id, "next", "source", "target", "hub", NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortIncomingHandoff(ctx, id, "next", "source"); !errors.Is(err, ErrStaleHandoff) {
		t.Fatalf("accepted op dropped: %v", err)
	}
}

func TestIncomingHandoffActivatedRestartPreservesProxy(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	id := agentLockTestSetup(t, s)
	if err := prepareIncomingTest(t, s, id, "op", "source", "target"); err != nil {
		t.Fatal(err)
	}
	a, err := s.AcceptIncomingHandoff(ctx, id, "op", "source", "target", "hub", NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateIncomingHandoff(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAgentLock(ctx, id, "target", a.AcceptedToken); err != nil {
		t.Fatal(err)
	}
	b, err := s.AcquireAgentLock(ctx, id, "target", NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedProxyPeer != "hub" || b.FencingToken <= a.AcceptedToken {
		t.Fatalf("bad restart lock: %+v", b)
	}
	retry, err := s.AcceptIncomingHandoff(ctx, id, "op", "source", "target", "hub", NowMillis(), 300000)
	if err != nil || retry.AcceptedToken != b.FencingToken {
		t.Fatalf("restart receipt: %+v %v", retry, err)
	}
	if err := s.FinishIncomingHandoff(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
}
