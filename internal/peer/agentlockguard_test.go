package peer

import (
	"context"
	"errors"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

func TestAgentLockGuardIgnoresStaleAcquireAfterRemove(t *testing.T) {
	ctx := context.Background()
	st := openRegistrarTestStore(t)
	const id = "ag_guard_remove"
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: id, Name: id}, store.AgentInsertOptions{}); err != nil {
		t.Fatal(err)
	}
	g := NewAgentLockGuard(st, &Identity{DeviceID: "self"}, nil)
	g.AddAgent(ctx, id)
	g.RemoveAgent(ctx, id)
	// A refreshAll snapshot may have selected this id before RemoveAgent.
	g.acquire(ctx, id)
	if _, err := st.GetAgentLock(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale acquire created lock: %v", err)
	}
}

func TestAgentLockGuardAdoptsReclaimedTokenAndIgnoresOldRefresh(t *testing.T) {
	ctx := context.Background()
	st := openRegistrarTestStore(t)
	const id = "ag_guard_reclaim"
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: id, Name: id}, store.AgentInsertOptions{}); err != nil {
		t.Fatal(err)
	}
	g := NewAgentLockGuard(st, &Identity{DeviceID: "self"}, nil)
	g.AddAgent(ctx, id)
	old := g.tokens[id]
	lock, err := st.ForceReclaimAgentToLocal(ctx, id, "self", store.NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	g.AddAgent(ctx, id)
	g.refreshOne(id, old)
	if got := g.tokens[id]; got != lock.FencingToken {
		t.Fatalf("new token lost: %d want %d", got, lock.FencingToken)
	}
	g.RemoveAgent(ctx, id)
	if _, err := st.GetAgentLock(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove did not release new token: %v", err)
	}
}
