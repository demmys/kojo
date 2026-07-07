package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDrainBlockers_ReportsEachBlockingItem: DrainBlockers must return one
// human-readable entry per blocking counter/map, tagged with kind and
// agentID, so a stuck restart drain is diagnosable.
func TestDrainBlockers_ReportsEachBlockingItem(t *testing.T) {
	m := &Manager{
		busy: map[string]busyEntry{
			"ag_busy": {cancel: func() {}, source: BusySourceNotification, startedAt: time.Now().Add(-4 * time.Minute)},
		},
		preparing:       map[string]int{"ag_prep": 2},
		notifying:       map[string]int{"ag_notif": 1},
		mutating:        map[string]int{"ag_mut": 1},
		editing:         map[string]bool{"ag_edit": true},
		resetting:       map[string]bool{"ag_reset": true},
		switching:       map[string]bool{"ag_sw": true},
		profileGen:      map[string]bool{"ag_prof": true},
		oneShotCancels:  map[string]map[int64]context.CancelFunc{"ag_os": {7: func() {}}},
		oneShotSessions: map[string]map[int64]string{"ag_os": {7: "groupdm:g_1"}},
		oneShotArmed:    map[string]map[int64]time.Time{"ag_os": {7: time.Now().Add(-32 * time.Minute)}},
	}
	m.summarizing = 1

	got := strings.Join(m.DrainBlockers(), "\n")
	wants := []string{
		"busy:ag_busy(source=notification,age=4m)",
		"preparing:ag_prep(n=2)",
		"notifying:ag_notif(n=1)",
		"mutating:ag_mut(n=1)",
		"editing:ag_edit",
		"resetting:ag_reset",
		"switching:ag_sw",
		"summarizing(n=1)",
		"profileGen:ag_prof",
		"oneShot:ag_os(id=7,key=groupdm:g_1,age=32m)",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("DrainBlockers missing %q; got:\n%s", w, got)
		}
	}

	// Fully idle → empty list.
	idle := &Manager{}
	if b := idle.DrainBlockers(); len(b) != 0 {
		t.Errorf("idle DrainBlockers = %v, want empty", b)
	}
}

// TestWaitAllChatsIdle_TimeoutErrorIncludesBlockers: on ctx timeout the
// returned error must wrap ctx.Err() AND embed the blocker list so the
// aborted restart is diagnosable from the error alone.
func TestWaitAllChatsIdle_TimeoutErrorIncludesBlockers(t *testing.T) {
	m := &Manager{
		busy: map[string]busyEntry{"ag_stuck": {cancel: func() {}, source: BusySourceUser}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := m.WaitAllChatsIdle(ctx)
	if err == nil {
		t.Fatal("WaitAllChatsIdle returned nil while busy")
	}
	if !strings.Contains(err.Error(), "busy:ag_stuck") {
		t.Errorf("timeout error missing blocker list: %v", err)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("timeout error does not wrap ctx.Err(): %v", err)
	}
}

// TestQuiescingRefusesNewChats: SetQuiescing(true) must make
// acquirePreparing (the shared Chat / ChatOneShot entry gate) refuse
// with ErrAgentBusy; SetQuiescing(false) restores it.
func TestQuiescingRefusesNewChats(t *testing.T) {
	m := &Manager{}
	m.SetQuiescing(true)
	if err := m.acquirePreparing("ag_x"); err == nil {
		t.Fatal("acquirePreparing succeeded while quiescing")
	}
	m.SetQuiescing(false)
	if err := m.acquirePreparing("ag_x"); err != nil {
		t.Fatalf("acquirePreparing after quiesce lift: %v", err)
	}
	m.releasePreparing("ag_x")
}

// TestWaitAllChatsIdle_DrainsBusyAndSummarizing: the daemon-wide idle
// wait must block while any agent is busy or a post-turn summarizer is
// in flight, and return once both clear.
func TestWaitAllChatsIdle_DrainsBusyAndSummarizing(t *testing.T) {
	m := &Manager{
		busy: map[string]busyEntry{
			"ag_a": {cancel: func() {}},
		},
	}
	m.summarizing = 1

	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.WaitAllChatsIdle(short); err == nil {
		t.Fatal("WaitAllChatsIdle returned while busy + summarizing")
	}

	done := make(chan error, 1)
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	go func() { done <- m.WaitAllChatsIdle(ctx) }()

	time.Sleep(150 * time.Millisecond)
	m.clearBusy("ag_a")
	m.busyMu.Lock()
	m.summarizing = 0
	m.busyMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitAllChatsIdle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitAllChatsIdle did not return after drain")
	}
}
