package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestManager_Steer_NotBusy verifies Steer refuses when the agent has no
// running turn.
func TestManager_Steer_NotBusy(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Steer(context.Background(), "ag_nobody", "hello"); !errors.Is(err, ErrAgentNotBusy) {
		t.Errorf("err = %v, want ErrAgentNotBusy", err)
	}
}

// TestManager_Steer_UnsupportedBackend verifies that a busy turn whose
// backend never registered a steer handle (steer == nil, e.g. codex/grok/
// llamacpp) surfaces ErrSteerUnsupported rather than silently no-oping.
func TestManager_Steer_UnsupportedBackend(t *testing.T) {
	m := newTestManager(t)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{startedAt: time.Now(), cancel: func() {}}
	m.busyMu.Unlock()

	if _, err := m.Steer(context.Background(), "ag_test", "hello"); !errors.Is(err, ErrSteerUnsupported) {
		t.Errorf("err = %v, want ErrSteerUnsupported", err)
	}
}

// TestManager_Steer_InjectsAndPersists verifies that once a backend has
// registered a steer handle (mirroring ChatOptions.OnSteerReady), Steer
// forwards the text to it and appends it to the transcript as a plain user
// message.
func TestManager_Steer_InjectsAndPersists(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}

	var got string
	outCh := make(chan ChatEvent, 4)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     outCh,
		steer: func(text string) error {
			got = text
			return nil
		},
	}
	m.busyMu.Unlock()

	if _, err := m.Steer(context.Background(), "ag_test", "steer this turn"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got != "steer this turn" {
		t.Errorf("steer handle received %q, want %q", got, "steer this turn")
	}

	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mm := range msgs {
		if mm.Role == "user" && mm.Content == "steer this turn" {
			found = true
		}
	}
	if !found {
		t.Error("steered message not persisted to transcript")
	}

	select {
	case e := <-outCh:
		if e.Type != "message" || e.Message == nil || e.Message.Content != "steer this turn" {
			t.Errorf("unexpected outCh event: %+v", e)
		}
	default:
		t.Error("expected a live event pushed to outCh for the steered message")
	}
}

// TestManager_Steer_AppendFailureIsNotSwallowed verifies the durability
// invariant under the persist-first order: if the transcript append fails
// (here: the agent row is absent, as when it was evicted mid device-switch),
// Steer must NOT report success AND must NOT have injected the text into the
// live turn — persisting first means a failed reservation never reaches the
// model, so a client retry can't double-inject.
func TestManager_Steer_AppendFailureIsNotSwallowed(t *testing.T) {
	m := newTestManager(t)
	// Deliberately do NOT upsert "ag_gone" into the store, so appendMessage
	// fails with store.ErrNotFound.

	var injected string
	m.busyMu.Lock()
	m.busy["ag_gone"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     make(chan ChatEvent, 4),
		steer: func(text string) error {
			injected = text
			return nil
		},
	}
	m.busyMu.Unlock()

	_, err := m.Steer(context.Background(), "ag_gone", "steer that cannot persist")
	if err == nil {
		t.Fatal("Steer returned nil despite a failed transcript append (2xx would lose the message on reload)")
	}
	// Persist-first: the reservation failed, so the text must never have been
	// injected into the live turn.
	if injected != "" {
		t.Errorf("steer func was called (%q) despite the reservation failing; persist-first must not inject", injected)
	}
}

// TestManager_Steer_InjectionRaceStartsFallbackTurn verifies the fire-and-
// forget-window fix: when the row is reserved (persist succeeds) but injection
// then fails with ErrAgentNotBusy (the turn ended between the handle check and
// the write), Steer rolls the reserved row back, waits for the lingering busy
// slot to clear, and starts a normal follow-up turn EXACTLY ONCE with the same
// text — no duplicate persistence, no silent drop. Returns SteerModeFallbackTurn.
func TestManager_Steer_InjectionRaceStartsFallbackTurn(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}

	// Fake fallback turn: records the call and persists the message once,
	// exactly as the real Chat would. Returns an already-closed channel so
	// Steer's drain goroutine exits immediately.
	var fallbackCalls int32
	m.steerFallbackChat = func(ctx context.Context, agentID, text string) (<-chan ChatEvent, error) {
		atomic.AddInt32(&fallbackCalls, 1)
		if err := appendMessage(agentID, newUserMessage(text, nil)); err != nil {
			return nil, err
		}
		ch := make(chan ChatEvent)
		close(ch)
		return ch, nil
	}

	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     make(chan ChatEvent, 4),
		steer: func(text string) error {
			// The turn ended just before this write landed. Mirror the real
			// runtime: the turn goroutine clears the busy slot as it winds
			// down, so waitBusyClear returns promptly.
			m.clearBusy("ag_test")
			return ErrAgentNotBusy
		},
	}
	m.busyMu.Unlock()

	mode, err := m.Steer(context.Background(), "ag_test", "lands nowhere")
	if err != nil {
		t.Fatalf("Steer = %v, want nil (fallback turn)", err)
	}
	if mode != SteerModeFallbackTurn {
		t.Fatalf("mode = %q, want %q", mode, SteerModeFallbackTurn)
	}
	if n := atomic.LoadInt32(&fallbackCalls); n != 1 {
		t.Fatalf("fallback turn started %d times, want exactly 1", n)
	}
	// Exactly one copy of the text must be persisted (the fallback's), never
	// the rolled-back steer row on top of it.
	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, mm := range msgs {
		if mm.Role == "user" && mm.Content == "lands nowhere" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("persisted %d copies of the steer text, want exactly 1 (no duplicate)", count)
	}
}

// TestManager_Steer_Quiescing verifies Part C: a steer during a restart drain
// returns ErrQuiescing and persists nothing (no row reserved against a turn
// about to be aborted).
func TestManager_Steer_Quiescing(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}
	// A live steerable turn exists — Steer must still refuse while quiescing.
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     make(chan ChatEvent, 4),
		steer:     func(text string) error { return nil },
	}
	m.quiescing = true
	m.busyMu.Unlock()

	_, err := m.Steer(context.Background(), "ag_test", "during drain")
	if !errors.Is(err, ErrQuiescing) {
		t.Fatalf("Steer while quiescing = %v, want ErrQuiescing", err)
	}
	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range msgs {
		if mm.Content == "during drain" {
			t.Error("steer row was persisted despite quiescing")
		}
	}
}

// TestManager_AnswerQuestion_AppendFailureIsNotSwallowed mirrors the Steer
// invariant for the AskUserQuestion answer path.
func TestManager_AnswerQuestion_AppendFailureIsNotSwallowed(t *testing.T) {
	m := newTestManager(t)
	// "ag_gone" is intentionally absent from the store → append fails.

	m.busyMu.Lock()
	m.busy["ag_gone"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     make(chan ChatEvent, 4),
		answer: func(requestID string, answers map[string]any, deny bool, denyMessage string) error {
			return nil
		},
	}
	m.busyMu.Unlock()

	err := m.AnswerQuestion(context.Background(), "ag_gone", "req-1",
		map[string]any{"色選択": "青"}, false, "")
	if err == nil {
		t.Fatal("AnswerQuestion returned nil despite a failed transcript append")
	}
}

// TestManager_Steer_SteerFuncError propagates the backend's error (e.g.
// process already exited / stdin closed) rather than swallowing it.
func TestManager_Steer_SteerFuncError(t *testing.T) {
	m := newTestManager(t)
	// Upsert so the persist-first reservation succeeds and the steer func is
	// actually reached (otherwise the append failure would short-circuit).
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		steer: func(text string) error {
			return errors.New("turn is no longer accepting input")
		},
	}
	m.busyMu.Unlock()

	if _, err := m.Steer(context.Background(), "ag_test", "too late"); err == nil {
		t.Error("expected error from steer func to propagate")
	}
}
