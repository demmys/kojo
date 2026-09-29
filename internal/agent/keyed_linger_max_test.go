package agent

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// runPendingTurn runs one solicited keyed turn that ends with task t1 pending.
func runPendingTurn(t *testing.T, k *keyedTestSession, a *Agent, hold time.Duration) {
	t.Helper()
	sink, err := k.s.startTurn(context.Background(), a, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`+"\n")
	if hold > 0 {
		time.Sleep(hold)
	}
	io.WriteString(k.pw, `{"type":"result","subtype":"success","result":"ok"}`+"\n")
	if d := doneOf(t, collect(t, sink, 3*time.Second)); d.BackgroundTasksPending != 1 {
		t.Fatalf("pending = %d, want 1", d.BackgroundTasksPending)
	}
}

func assertAlive(t *testing.T, k *keyedTestSession, for_ time.Duration) {
	t.Helper()
	select {
	case <-k.killed:
		t.Fatal("keyed session closed before its linger cap")
	case <-time.After(for_):
	}
}

func TestKeyedLingerMaxRearmsAtTurnEnd(t *testing.T) {
	setKeyedTimers(t, time.Hour, 400*time.Millisecond, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	a := &Agent{ID: "test-agent"}
	runPendingTurn(t, k, a, 0)
	time.Sleep(250 * time.Millisecond)
	// A later turn on the same thread restarts the cap from its end.
	runPendingTurn(t, k, a, 0)
	assertAlive(t, k, 250*time.Millisecond) // ~500ms since the first turn
	waitKilled(t, k, 3*time.Second)
	k.s.mu.Lock()
	reason := k.s.closeReason
	k.s.mu.Unlock()
	if !strings.Contains(reason, "待機上限") {
		t.Fatalf("close reason = %q", reason)
	}
}

func TestKeyedLingerMaxDuringTurnDoesNotClose(t *testing.T) {
	setKeyedTimers(t, time.Hour, 150*time.Millisecond, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	a := &Agent{ID: "test-agent"}
	runPendingTurn(t, k, a, 0)
	// The cap elapses while this turn runs; its end re-arms instead.
	runPendingTurn(t, k, a, 300*time.Millisecond)
	assertAlive(t, k, 80*time.Millisecond)
	waitKilled(t, k, 3*time.Second)
}

func TestKeyedLingerMaxUsesAgentOverride(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	b.setKeyedLingerMax(&Agent{ID: "test-agent", BackgroundMaxMinutes: 5, UpdatedAt: "2026-09-29T10:00:00Z"})
	// A chat carrying an older (or same-second) row snapshot does not undo it.
	b.recordKeyedLingerMax(&Agent{ID: "test-agent", UpdatedAt: "2026-09-29T09:00:00Z"}, false)
	b.recordKeyedLingerMax(&Agent{ID: "test-agent", UpdatedAt: "2026-09-29T10:00:00Z"}, false)
	k.s.mu.Lock()
	got := k.s.effectiveLingerMaxLocked()
	k.s.mu.Unlock()
	if got != 5*time.Minute {
		t.Fatalf("linger max = %s, want 5m", got)
	}
	// Clearing the setting falls back to the default.
	b.setKeyedLingerMax(&Agent{ID: "test-agent", UpdatedAt: "2026-09-29T11:00:00Z"})
	k.s.mu.Lock()
	got = k.s.effectiveLingerMaxLocked()
	k.s.mu.Unlock()
	if got != time.Hour {
		t.Fatalf("linger max = %s, want default 1h", got)
	}
}

func TestBackgroundMaxValidation(t *testing.T) {
	for _, c := range []struct {
		min  int
		ok   bool
		want time.Duration
	}{{0, true, 0}, {30, true, 30 * time.Minute}, {24 * 60, true, 24 * time.Hour}, {24*60 + 1, false, 0}, {-1, false, 0}} {
		if got := ValidBackgroundMax(c.min); got != c.ok {
			t.Errorf("ValidBackgroundMax(%d) = %v", c.min, got)
		}
		if got := (&Agent{BackgroundMaxMinutes: c.min}).BackgroundLingerMax(); got != c.want {
			t.Errorf("BackgroundLingerMax(%d) = %s", c.min, got)
		}
	}
	if lingerLimitReason(2*time.Hour) != "待機上限(2時間)に到達" || lingerLimitReason(90*time.Minute) != "待機上限(90分)に到達" {
		t.Errorf("reason format: %q / %q", lingerLimitReason(2*time.Hour), lingerLimitReason(90*time.Minute))
	}
}

func TestKeyedLingerMaxRearmsForTasksReappearingAfterExpiredTurn(t *testing.T) {
	setKeyedTimers(t, time.Hour, 100*time.Millisecond, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	a := &Agent{ID: "test-agent"}
	runPendingTurn(t, k, a, 0)
	// The cap fires during this turn, which then ends with nothing pending.
	sink, err := k.s.startTurn(context.Background(), a, "go", false, false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`+"\n")
	io.WriteString(k.pw, `{"type":"result","subtype":"success","result":"ok"}`+"\n")
	doneOf(t, collect(t, sink, 3*time.Second))
	// Tasks reappearing while idle must still be bounded.
	io.WriteString(k.pw, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t2"}]}`+"\n")
	waitKilled(t, k, 3*time.Second)
}

func TestSetKeyedLingerMaxUpdatesLiveSessions(t *testing.T) {
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	b.setKeyedLingerMax(&Agent{ID: "test-agent", BackgroundMaxMinutes: 7})
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if got := k.s.effectiveLingerMaxLocked(); got != 7*time.Minute {
		t.Fatalf("linger max = %s", got)
	}
}

func TestSetKeyedLingerMaxReschedulesRunningCap(t *testing.T) {
	setKeyedTimers(t, time.Hour, time.Hour, 10*time.Millisecond)
	b := newKeyedTestBackend()
	k := newKeyedTestSession(t, b, "test-agent:slack:C1:1.0")
	runPendingTurn(t, k, &Agent{ID: "test-agent"}, 0)
	k.s.mu.Lock()
	k.s.lingerBase = time.Now().Add(-2 * time.Minute)
	k.s.mu.Unlock()
	// A 1-minute cap against a turn that ended 2 minutes ago is already over.
	b.setKeyedLingerMax(&Agent{ID: "test-agent", BackgroundMaxMinutes: 1})
	waitKilled(t, k, 3*time.Second)
}
