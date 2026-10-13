package agent

import (
	"testing"
	"time"
)

// A thread-initiated device switch must not close the keyed session whose
// pending tool call is the switch request itself; every other session of the
// agent is still closed so the transfer sees released session files.
func TestCloseSessionSyncExceptKeyedPreservesCaller(t *testing.T) {
	setKeyedTimers(t, time.Minute, time.Hour, 50*time.Millisecond)
	b := newKeyedTestBackend()
	caller := newKeyedTestSession(t, b, "slack:caller")
	other := newKeyedTestSession(t, b, "slack:other")

	b.CloseSessionSyncExceptKeyed("test-agent", "slack:caller")

	waitKilled(t, other, 2*time.Second)
	select {
	case <-caller.killed:
		t.Fatal("caller keyed session was closed")
	default:
	}
	if !b.HasKeyedSession("test-agent", "slack:caller") {
		t.Fatal("caller keyed session left the pool")
	}
	if b.HasKeyedSession("test-agent", "slack:other") {
		t.Fatal("other keyed session still pooled")
	}

	b.CloseKeyedSessionSync("test-agent", "slack:caller", "agent moved")
	waitKilled(t, caller, 2*time.Second)
}

func TestCloseSessionSyncExceptKeyedEmptyKeyClosesAll(t *testing.T) {
	setKeyedTimers(t, time.Minute, time.Hour, 50*time.Millisecond)
	b := newKeyedTestBackend()
	a := newKeyedTestSession(t, b, "slack:a")
	b.CloseSessionSyncExceptKeyed("test-agent", "")
	waitKilled(t, a, 2*time.Second)
}

func TestCloseClaudeKeyedSessionIfSameSkipsReplacement(t *testing.T) {
	setKeyedTimers(t, time.Minute, time.Hour, 50*time.Millisecond)
	b := newKeyedTestBackend()
	m := &Manager{backends: map[string]ChatBackend{"claude": b}}
	old := newKeyedTestSession(t, b, "slack:t")
	h := m.ClaudeKeyedSessionHandle("test-agent", "slack:t")
	if h == nil {
		t.Fatal("no handle")
	}
	b.CloseKeyedSessionSync("test-agent", "slack:t", "reclaim")
	waitKilled(t, old, 2*time.Second)
	repl := newKeyedTestSession(t, b, "slack:t")
	if m.CloseClaudeKeyedSessionIfSame("test-agent", "slack:t", h, "moved") {
		t.Fatal("closed a replacement session")
	}
	select {
	case <-repl.killed:
		t.Fatal("replacement killed")
	default:
	}
	h2 := m.ClaudeKeyedSessionHandle("test-agent", "slack:t")
	if !m.CloseClaudeKeyedSessionIfSame("test-agent", "slack:t", h2, "moved") {
		t.Fatal("did not close pinned session")
	}
	waitKilled(t, repl, 2*time.Second)
}
