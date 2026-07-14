package agent

import (
	"context"
	"strings"
	"testing"
)

// seedPreviewTestAgent registers an agent in both the in-memory map and
// the DB (updateLastMessagePreview mutates the map; appendMessage needs
// the agents row for the FK on agent_messages).
func seedPreviewTestAgent(t *testing.T, m *Manager, id string) *Agent {
	t.Helper()
	a := &Agent{ID: id, Name: "Preview", Tool: "claude"}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	if err := m.store.Upsert(a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return a
}

// TestProcessChatEvents_ErrorUpdatesLastMessagePreview locks the
// dashboard-visibility contract for failed turns: a terminal "error"
// event must not only persist the "⚠️ Error: …" system message to the
// transcript (the historical behavior) but also refresh the in-memory
// LastMessage preview that Manager.List serves. Before this contract,
// the list kept showing the stale pre-error preview, so an agent whose
// cron turns failed for hours (e.g. a 402 balance-exhausted loop) was
// indistinguishable from a healthy idle one.
func TestProcessChatEvents_ErrorUpdatesLastMessagePreview(t *testing.T) {
	m := newTestManager(t)
	a := seedPreviewTestAgent(t, m, "ag_errprev")

	backendCh := make(chan ChatEvent, 1)
	backendCh <- ChatEvent{Type: "error", ErrorMessage: "API error (status 402 Payment Required): balance exhausted"}
	close(backendCh)
	outCh := make(chan ChatEvent, 4)
	m.processChatEvents(context.Background(), a.ID, backendCh, outCh)

	// In-memory preview (what Manager.List copies out).
	m.mu.Lock()
	lm := a.LastMessage
	lmAt := a.LastMessageAt
	m.mu.Unlock()
	if lm == nil {
		t.Fatal("LastMessage preview not updated after error turn")
	}
	if lm.Role != "system" || !strings.HasPrefix(lm.Content, "⚠️ Error: ") {
		t.Errorf("preview = role %q content %q, want system message with \"⚠️ Error: \" prefix", lm.Role, lm.Content)
	}
	if !strings.Contains(lm.Content, "402") {
		t.Errorf("preview content %q lost the error detail", lm.Content)
	}
	if lmAt == 0 {
		t.Error("LastMessageAt not stamped — list ordering would not float the error")
	}

	// Persisted transcript row (restart / GetRemote / ListRemote reads).
	msgs, err := loadMessages(a.ID, 1)
	if err != nil {
		t.Fatalf("loadMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Role != "system" || !strings.HasPrefix(msgs[0].Content, "⚠️ Error: ") {
		t.Errorf("persisted message = role %q content %q, want system ⚠️ Error", msgs[0].Role, msgs[0].Content)
	}
}

// TestProcessChatEvents_ErrorSkipsEvictedAgent guards the §3.7 release
// path: an error event for an agent that a device-switch already evicted
// from m.agents must not write to this peer's transcript (target owns
// the canonical state) — and must not resurrect a preview either.
func TestProcessChatEvents_ErrorSkipsEvictedAgent(t *testing.T) {
	m := newTestManager(t)
	a := seedPreviewTestAgent(t, m, "ag_errgone")
	m.mu.Lock()
	delete(m.agents, a.ID)
	m.mu.Unlock()

	backendCh := make(chan ChatEvent, 1)
	backendCh <- ChatEvent{Type: "error", ErrorMessage: "boom"}
	close(backendCh)
	outCh := make(chan ChatEvent, 4)
	m.processChatEvents(context.Background(), a.ID, backendCh, outCh)

	msgs, err := loadMessages(a.ID, 10)
	if err != nil {
		t.Fatalf("loadMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("evicted agent got %d transcript writes, want 0", len(msgs))
	}
}
