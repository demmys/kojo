package agent

import (
	"testing"
	"time"
)

// TestIsBusyForStatusFlag verifies the busy accessor the dashboard's
// "N running" figure depends on (Manager.List sets Agent.Busy from it):
// an in-flight interactive/cron chat and a background turn continuing the
// agent's own work read as busy, a group-DM notification turn does not,
// and an idle agent is not busy.
func TestIsBusyForStatusFlag(t *testing.T) {
	m := &Manager{busy: make(map[string]busyEntry)}

	if m.IsBusyForStatus("a") {
		t.Fatal("idle agent should not be busy")
	}

	m.busy["a"] = busyEntry{source: BusySourceUser}
	if !m.IsBusyForStatus("a") {
		t.Fatal("agent with a user chat should be busy")
	}

	m.busy["c"] = busyEntry{source: BusySourceCron}
	if !m.IsBusyForStatus("c") {
		t.Fatal("agent with a cron chat should be busy")
	}

	m.busy["n"] = busyEntry{source: BusySourceNotification}
	if m.IsBusyForStatus("n") {
		t.Fatal("notification-only turns must not surface as busy")
	}

	m.busy["b"] = busyEntry{source: BusySourceBackground}
	if !m.IsBusyForStatus("b") {
		t.Fatal("background turns continuing the agent's own work should be busy")
	}
}

// TestHandleBackgroundTurnBusyForStatus exercises the real call site: an
// in-flight background task-notification turn (handleBackgroundTurn) must
// surface via IsBusyForStatus so the dashboard's "N running" counts it.
// Regression test for the "agent working on a task-notification wake shows
// 0 running" bug — handleBackgroundTurn used to tag the busy entry with
// BusySourceNotification, which IsBusyForStatus excludes.
func TestHandleBackgroundTurnBusyForStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	m := &Manager{
		agents:    make(map[string]*Agent),
		busy:      make(map[string]busyEntry),
		notifying: make(map[string]int),
		resetting: make(map[string]bool),
		logger:    testLogger(),
	}
	const id = "ag_bgturn"

	events := make(chan ChatEvent)
	done := make(chan struct{})
	go func() {
		m.handleBackgroundTurn(id, events, nil, nil)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for !m.IsBusyForStatus(id) {
		select {
		case <-deadline:
			t.Fatal("in-flight background turn never surfaced via IsBusyForStatus")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(events)
	select {
	case <-done:
	case <-deadline:
		t.Fatal("handleBackgroundTurn did not finish after events closed")
	}
	if m.IsBusyForStatus(id) {
		t.Fatal("agent should not be busy after the background turn ends")
	}
}
