package slackbot

import (
	"context"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// newTestBot creates a Bot pointing at a mock Slack API for unit testing.
func newTestBot(t *testing.T, cfg agent.SlackBotConfig) *Bot {
	t.Helper()
	srv := mockSlackServer(t)

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	sm := socketmode.New(api)
	ctx, cancel := context.WithCancel(context.Background())

	return &Bot{
		agentID:      "test-agent",
		agentDataDir: t.TempDir(),
		config:       cfg,
		api:          api,
		sm:           sm,
		mgr:          &mockMgr{},
		logger:       testLogger,
		botUserID:    "UBOTTEST",
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		threadLocks:  make(map[string]*threadLock),
		userCache:    make(map[string]string),
		sem:          make(chan struct{}, maxConcurrentChats),
	}
}

// --- Thread lock tests ---

func TestBotThreadLockRefCount(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Acquire lock for a thread.
	tl1 := bot.acquireThreadLock("C1", "1234.5678")
	tl1.mu.Lock()

	// Acquire again for the same thread — same lock, higher refcount.
	tl2 := bot.acquireThreadLock("C1", "1234.5678")
	if tl1 != tl2 {
		t.Fatal("expected same lock for same thread")
	}

	// Release once — should still exist (refcount > 0).
	bot.releaseThreadLock("C1", "1234.5678", tl1)
	bot.threadLocksMu.Lock()
	_, exists := bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if !exists {
		t.Fatal("lock should still exist after one release")
	}

	tl1.mu.Unlock()

	// Release again — refcount hits 0, entry removed.
	bot.releaseThreadLock("C1", "1234.5678", tl2)
	bot.threadLocksMu.Lock()
	_, exists = bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if exists {
		t.Fatal("lock should be removed when refcount reaches zero")
	}
}

func TestBotThreadLockIsolation(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	tl1 := bot.acquireThreadLock("C1", "ts1")
	tl2 := bot.acquireThreadLock("C1", "ts2")
	tl3 := bot.acquireThreadLock("C2", "ts1")

	if tl1 == tl2 || tl1 == tl3 || tl2 == tl3 {
		t.Fatal("different channel/thread combos should get different locks")
	}

	bot.releaseThreadLock("C1", "ts1", tl1)
	bot.releaseThreadLock("C1", "ts2", tl2)
	bot.releaseThreadLock("C2", "ts1", tl3)
}

// --- Semaphore tests ---

func TestBotSemaphoreCapacity(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	if cap(bot.sem) != maxConcurrentChats {
		t.Fatalf("semaphore capacity = %d, want %d", cap(bot.sem), maxConcurrentChats)
	}

	// Fill the semaphore.
	for i := 0; i < maxConcurrentChats; i++ {
		bot.sem <- struct{}{}
	}

	// Next send should block (non-blocking test via select).
	select {
	case bot.sem <- struct{}{}:
		t.Fatal("semaphore should be full")
	default:
		// expected
	}
}

// --- User cache tests ---

func TestBotResolveUserName(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns display_name "Alice" for U123.
	name := bot.resolveUserName("U123")
	if name != "Alice" {
		t.Fatalf("got %q, want %q", name, "Alice")
	}

	// Should be cached now.
	bot.userCacheMu.RLock()
	cached, ok := bot.userCache["U123"]
	bot.userCacheMu.RUnlock()
	if !ok || cached != "Alice" {
		t.Fatal("expected name to be cached")
	}
}

func TestBotResolveUserNameFallbackToRealName(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns empty display_name for U456, falls back to real_name "Bob Real".
	name := bot.resolveUserName("U456")
	if name != "Bob Real" {
		t.Fatalf("got %q, want %q", name, "Bob Real")
	}
}

func TestBotResolveUserNameFallbackToRawID(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	// Mock returns error for unknown users, falls back to raw ID.
	name := bot.resolveUserName("UUNKNOWN")
	if name != "UUNKNOWN" {
		t.Fatalf("got %q, want %q", name, "UUNKNOWN")
	}
}

// --- shouldAutoReply tests ---

func TestBotShouldAutoReply(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{Enabled: true, ThreadReplies: true})
	defer bot.cancel()

	ch, ts := "C1", "1234.5678"

	// No history → should not auto-reply.
	if bot.shouldAutoReply(ch, ts, "hello") {
		t.Fatal("should not auto-reply without history")
	}

	// Create history where bot sent the last message.
	path := chathistory.HistoryFilePath(bot.agentDataDir, platformSlack, ch, ts)
	msgs := []chathistory.HistoryMessage{
		{Platform: platformSlack, ChannelID: ch, ThreadID: ts, MessageID: "1", UserID: "U123", Text: "hi bot", IsBot: false},
		{Platform: platformSlack, ChannelID: ch, ThreadID: ts, MessageID: "2", UserID: "UBOTTEST", Text: "hello!", IsBot: true},
	}
	if err := chathistory.AppendMessages(path, msgs); err != nil {
		t.Fatal(err)
	}

	// Bot sent last message, no other mentions → auto-reply.
	if !bot.shouldAutoReply(ch, ts, "thanks") {
		t.Fatal("should auto-reply when last message was from bot")
	}

	// Message mentions another user → should not auto-reply.
	if bot.shouldAutoReply(ch, ts, "hey <@UOTHER>") {
		t.Fatal("should not auto-reply when mentioning another user")
	}

	// Mentioning the bot itself is OK → should auto-reply.
	if !bot.shouldAutoReply(ch, ts, "hey <@UBOTTEST> thanks") {
		t.Fatal("should auto-reply when only mentioning the bot itself")
	}
}

func TestBotShouldAutoReplyEmptyDataDir(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	bot.agentDataDir = ""
	defer bot.cancel()

	if bot.shouldAutoReply("C1", "ts1", "hello") {
		t.Fatal("should not auto-reply with empty agentDataDir")
	}
}
