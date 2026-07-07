package agent

import (
	"testing"
	"time"
)

// TestPendingQuestionTracking verifies the pending-AskUserQuestion
// bookkeeping backing Agent.AwaitingAnswer: set on raise, reflected by
// List(), cleared on answer (clearQuestion, wired as
// ChatOptions.OnQuestionResolved), and cleared on process/turn exit
// (clearAllQuestionsForAgent, processChatEvents' safety-net defer) even if
// no answer ever arrives.
func TestPendingQuestionTracking(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.agents["ag_alice"] = &Agent{ID: "ag_alice", Name: "Alice", Tool: "claude"}
	m.mu.Unlock()
	if err := m.store.Upsert(m.agents["ag_alice"]); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if m.HasPendingQuestion("ag_alice") {
		t.Fatal("no question raised yet: should not be pending")
	}

	// markQuestionRaised invokes OnQuestionRaised in its own goroutine (so a
	// slow web-push send can't delay forwarding the user_question ChatEvent)
	// — collect firings on a channel instead of a bare counter so the test
	// doesn't race the callback.
	raised := make(chan string, 8)
	m.OnQuestionRaised = func(agentID string) { raised <- agentID }
	awaitRaised := func(want int) {
		t.Helper()
		for i := 0; i < want; i++ {
			select {
			case agentID := <-raised:
				if agentID != "ag_alice" {
					t.Errorf("OnQuestionRaised agentID = %q, want ag_alice", agentID)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for OnQuestionRaised firing %d/%d", i+1, want)
			}
		}
		select {
		case agentID := <-raised:
			t.Fatalf("unexpected extra OnQuestionRaised firing for %q", agentID)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Raising a question marks it pending and fires OnQuestionRaised once.
	m.markQuestionRaised("ag_alice", "req1")
	if !m.HasPendingQuestion("ag_alice") {
		t.Fatal("expected pending question after markQuestionRaised")
	}
	awaitRaised(1)

	// List() folds the pending state into Agent.AwaitingAnswer.
	list := m.List()
	if len(list) != 1 || !list[0].AwaitingAnswer {
		t.Fatalf("List() did not surface AwaitingAnswer=true: %+v", list)
	}

	// A second, DISTINCT question raised on the same agent while the first
	// is still outstanding is its own raise the user hasn't been told
	// about — it must fire OnQuestionRaised again.
	m.markQuestionRaised("ag_alice", "req2")
	awaitRaised(1)

	// Re-observing the SAME requestID as already-pending (e.g. a duplicate
	// user_question event) must not re-fire.
	m.markQuestionRaised("ag_alice", "req2")
	awaitRaised(0)

	// Answering one of the two pending questions leaves the agent still
	// awaiting (the other question is still open).
	m.clearQuestion("ag_alice", "req1")
	if !m.HasPendingQuestion("ag_alice") {
		t.Fatal("expected still-pending question req2 after clearing req1")
	}

	// clearQuestion is idempotent — clearing an already-cleared or unknown
	// requestID must not panic or affect other agents' state.
	m.clearQuestion("ag_alice", "req1")
	m.clearQuestion("ag_bob", "req-unknown")

	// Answering the last pending question clears the agent entirely.
	m.clearQuestion("ag_alice", "req2")
	if m.HasPendingQuestion("ag_alice") {
		t.Fatal("expected no pending question after clearing req2")
	}
	list = m.List()
	if len(list) != 1 || list[0].AwaitingAnswer {
		t.Fatalf("List() still reports AwaitingAnswer=true after all questions cleared: %+v", list)
	}

	// Safety net: a question that never gets an explicit resolution (e.g.
	// the backend process died) must still be dropped when the turn ends
	// — mirroring processChatEvents' deferred clearAllQuestionsForAgent.
	m.markQuestionRaised("ag_alice", "req3")
	awaitRaised(1)
	if !m.HasPendingQuestion("ag_alice") {
		t.Fatal("expected pending question after markQuestionRaised")
	}
	m.clearAllQuestionsForAgent("ag_alice")
	if m.HasPendingQuestion("ag_alice") {
		t.Fatal("clearAllQuestionsForAgent did not clear the leaked pending question")
	}
	list = m.List()
	if len(list) != 1 || list[0].AwaitingAnswer {
		t.Fatalf("List() still reports AwaitingAnswer=true after clearAllQuestionsForAgent: %+v", list)
	}
}
