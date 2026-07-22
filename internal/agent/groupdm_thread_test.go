package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// threadStub records one-shot invocations and drives the reply stream. It
// stands in for Manager.ChatOneShot via GroupDMManager.oneShot so thread-turn
// behaviour can be tested without the full chat pipeline.
type threadStub struct {
	mu            sync.Mutex
	calls         []OneShotOpts
	reply         string
	active        int32
	maxConcurrent int32
	blockUntilCtx bool
	turnDelay     time.Duration
}

func (s *threadStub) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }
func (s *threadStub) lastOpts() OneShotOpts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

func (s *threadStub) fn(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
	n := atomic.AddInt32(&s.active, 1)
	for {
		cur := atomic.LoadInt32(&s.maxConcurrent)
		if n <= cur || atomic.CompareAndSwapInt32(&s.maxConcurrent, cur, n) {
			break
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, opts)
	reply := s.reply
	s.mu.Unlock()

	if s.blockUntilCtx {
		<-ctx.Done()
	} else if s.turnDelay > 0 {
		time.Sleep(s.turnDelay)
	}
	atomic.AddInt32(&s.active, -1)

	ch := make(chan ChatEvent, 2)
	if reply != "" && ctx.Err() == nil {
		ch <- ChatEvent{Type: "text", Delta: reply}
	}
	close(ch)
	return ch, nil
}

func waitForMessage(t *testing.T, gdm *GroupDMManager, groupID, want string) *GroupMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _, _, err := gdm.Messages(groupID, 50, "")
		if err == nil {
			for _, msg := range msgs {
				if msg.Content == want {
					return msg
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("message %q never appeared in room %s", want, groupID)
	return nil
}

func TestThreadPost_RunsOneShotNotNotify(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}

	reply := waitForMessage(t, gdm, g.ID, "pong")
	if reply.AgentID != "ag_alice" {
		t.Errorf("reply author = %q, want ag_alice", reply.AgentID)
	}
	// Thread turn used ChatOneShot with the per-thread SessionKey.
	if got := stub.lastOpts().SessionKey; got != "groupdm:"+g.ID {
		t.Errorf("SessionKey = %q, want %q", got, "groupdm:"+g.ID)
	}
	// notify path must not have fired for the thread room.
	gdm.notifyMu.Lock()
	_, exists := gdm.notify[g.ID+":ag_alice"]
	gdm.notifyMu.Unlock()
	if exists {
		t.Errorf("thread post should not create notify state")
	}
}

func TestThreadPost_EmptyReplyPostsNothing(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: ""}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	msgs, _, _, err := gdm.Messages(g.ID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected only the user message, got %d messages", len(msgs))
	}
}

func TestGroupPost_UsesNotifyNotOneShot(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "hi"}
	gdm.oneShot = stub.fn

	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "hello team", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		gdm.notifyMu.Lock()
		_, a := gdm.notify[g.ID+":ag_alice"]
		_, b := gdm.notify[g.ID+":ag_bob"]
		gdm.notifyMu.Unlock()
		found = a || b
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("group post should use notify path (notify state expected)")
	}
	if stub.callCount() != 0 {
		t.Errorf("group post must not run a thread one-shot, got %d calls", stub.callCount())
	}
}

func TestTwoMemberDM_UsesNotifyNotThread(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "hi"}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice", "ag_bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "hey", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		gdm.notifyMu.Lock()
		_, a := gdm.notify[g.ID+":ag_alice"]
		_, b := gdm.notify[g.ID+":ag_bob"]
		gdm.notifyMu.Unlock()
		found = a || b
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("2-member dm should use notify path, not thread turn")
	}
	if stub.callCount() != 0 {
		t.Errorf("2-member dm must not run a thread one-shot, got %d calls", stub.callCount())
	}
}

func TestThreadTurn_ProceedsWhileMainChatBusy(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	stub := &threadStub{reply: "still-here"}
	gdm.oneShot = stub.fn

	// Mark the agent's main chat busy — the thread turn must not depend on it.
	mgr.busyMu.Lock()
	mgr.busy["ag_alice"] = busyEntry{startedAt: time.Now()}
	mgr.busyMu.Unlock()

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, g.ID, "still-here")
}

func TestThreadTurn_HasNoDeadline(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	hasDeadline := true
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		_, hasDeadline = ctx.Deadline()
		ch := make(chan ChatEvent)
		close(ch)
		return ch, nil
	}

	gdm.runThreadTurn("ag_alice", g.ID, "G", newGroupMessage(UserSenderID, UserSenderName, "ping", nil))
	if hasDeadline {
		t.Fatal("thread turn context has a deadline, want none")
	}
}

func TestThreadTurn_SerializesConcurrentPosts(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "ok", turnDelay: 80 * time.Millisecond}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "one", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "two", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.callCount() < 2 {
		t.Fatalf("expected 2 thread turns, got %d", stub.callCount())
	}
	if mc := atomic.LoadInt32(&stub.maxConcurrent); mc != 1 {
		t.Errorf("max concurrent thread turns = %d, want 1 (serialized)", mc)
	}
}

// TestArchiveThread_QueuedTurnBails verifies that a thread turn queued behind
// the per-room mutex during an archive does not run: no oneShot call for it,
// and no session JSONL re-created after archive removed it.
func TestArchiveThread_QueuedTurnBails(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{blockUntilCtx: true}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	sessionID := expectedClaudeSessionID("ag_alice", "groupdm:"+g.ID, false)
	absDir, err := filepath.Abs(agentDir("ag_alice"))
	if err != nil {
		t.Fatal(err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First post starts a blocking turn (holds threadMus until cancelled).
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "one", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.callCount() != 1 {
		t.Fatalf("first turn never started, calls=%d", stub.callCount())
	}

	// Second post queues a turn behind the mutex.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "two", nil, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Archive: cancels the first turn and removes the session. The queued
	// second turn then acquires the mutex, sees the room is gone, and bails.
	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	// Give the queued goroutine time to run (or, correctly, to bail).
	time.Sleep(200 * time.Millisecond)
	if got := stub.callCount(); got != 1 {
		t.Errorf("queued turn ran after archive: oneShot calls = %d, want 1", got)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("session file re-created (or not removed) after archive, stat err = %v", err)
	}
}

func TestArchiveThread_CancelsAndCleansSession(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{blockUntilCtx: true}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a fake session JSONL at the deterministic path so archive removes it.
	sessionID := expectedClaudeSessionID("ag_alice", "groupdm:"+g.ID, false)
	absDir, err := filepath.Abs(agentDir("ag_alice"))
	if err != nil {
		t.Fatal(err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start a turn that blocks until its context is cancelled.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gdm.threadCancelMu.Lock()
		_, running := gdm.threadCancels[g.ID]
		gdm.threadCancelMu.Unlock()
		if running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("expected session file removed, stat err = %v", err)
	}
	drained := false
	for i := 0; i < 200; i++ {
		gdm.threadCancelMu.Lock()
		_, running := gdm.threadCancels[g.ID]
		gdm.threadCancelMu.Unlock()
		if !running {
			drained = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !drained {
		t.Errorf("thread turn cancel entry did not drain after archive")
	}
}
