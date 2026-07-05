package agent

import (
	"testing"
)

// TestIsBusyForStatusFlag verifies the busy accessor the dashboard's
// "N running" figure depends on (Manager.List sets Agent.Busy from it):
// an in-flight interactive/cron chat reads as busy, a background
// notification turn does not, and an idle agent is not busy.
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
}
