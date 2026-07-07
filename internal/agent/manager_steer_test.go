package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestManager_Steer_NotBusy verifies Steer refuses when the agent has no
// running turn.
func TestManager_Steer_NotBusy(t *testing.T) {
	m := newTestManager(t)
	if err := m.Steer(context.Background(), "ag_nobody", "hello"); !errors.Is(err, ErrAgentNotBusy) {
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

	if err := m.Steer(context.Background(), "ag_test", "hello"); !errors.Is(err, ErrSteerUnsupported) {
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

	if err := m.Steer(context.Background(), "ag_test", "steer this turn"); err != nil {
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

	err := m.Steer(context.Background(), "ag_gone", "steer that cannot persist")
	if err == nil {
		t.Fatal("Steer returned nil despite a failed transcript append (2xx would lose the message on reload)")
	}
	// Persist-first: the reservation failed, so the text must never have been
	// injected into the live turn.
	if injected != "" {
		t.Errorf("steer func was called (%q) despite the reservation failing; persist-first must not inject", injected)
	}
}

// TestManager_Steer_InjectionFailureRollsBackRow verifies that when the row is
// reserved (persist succeeds) but injection then fails (turn ended between the
// handle check and the write), the reserved row is deleted so the caller's
// normal-send fallback doesn't leave a duplicate — and the original error
// (ErrAgentNotBusy) still propagates for that fallback.
func TestManager_Steer_InjectionFailureRollsBackRow(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     make(chan ChatEvent, 4),
		steer: func(text string) error {
			return ErrAgentNotBusy // turn ended before the write landed
		},
	}
	m.busyMu.Unlock()

	err := m.Steer(context.Background(), "ag_test", "lands nowhere")
	if !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("Steer = %v, want ErrAgentNotBusy to propagate for the not_busy fallback", err)
	}
	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range msgs {
		if mm.Role == "user" && mm.Content == "lands nowhere" {
			t.Error("reserved steer row was not rolled back after injection failure")
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

	if err := m.Steer(context.Background(), "ag_test", "too late"); err == nil {
		t.Error("expected error from steer func to propagate")
	}
}
