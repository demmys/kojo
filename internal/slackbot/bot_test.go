package slackbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	r1 := bot.reserveThread("C1", "1234.5678")
	r1.Wait()

	// Acquire again for the same thread — same lock, higher refcount.
	r2 := bot.reserveThread("C1", "1234.5678")
	if r1.tl != r2.tl {
		t.Fatal("expected same lock for same thread")
	}

	// Release once — should still exist (refcount > 0).
	bot.releaseThreadReservation("C1", "1234.5678", r1)
	bot.threadLocksMu.Lock()
	_, exists := bot.threadLocks["C1:1234.5678"]
	bot.threadLocksMu.Unlock()
	if !exists {
		t.Fatal("lock should still exist after one release")
	}

	// Release again — refcount hits 0, entry removed.
	r2.Wait()
	bot.releaseThreadReservation("C1", "1234.5678", r2)
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

	r1 := bot.reserveThread("C1", "ts1")
	r2 := bot.reserveThread("C1", "ts2")
	r3 := bot.reserveThread("C2", "ts1")

	if r1.tl == r2.tl || r1.tl == r3.tl || r2.tl == r3.tl {
		t.Fatal("different channel/thread combos should get different locks")
	}

	bot.releaseThreadReservation("C1", "ts1", r1)
	bot.releaseThreadReservation("C1", "ts2", r2)
	bot.releaseThreadReservation("C2", "ts1", r3)
}

func TestBotThreadPairKeepsArrivalAheadOfNextTurn(t *testing.T) {
	bot := newTestBot(t, agent.SlackBotConfig{})
	defer bot.cancel()

	turn, arrival := bot.reserveThreadPair("C1", "T1")
	next := bot.reserveThread("C1", "T1")
	turn.Wait()
	bot.releaseThreadReservation("C1", "T1", turn)
	arrival.Wait()

	nextReady := make(chan struct{})
	go func() {
		next.Wait()
		close(nextReady)
	}()
	select {
	case <-nextReady:
		t.Fatal("next human turn overtook the reserved arrival slot")
	case <-time.After(20 * time.Millisecond):
	}
	bot.releaseThreadReservation("C1", "T1", arrival)
	select {
	case <-nextReady:
	case <-time.After(time.Second):
		t.Fatal("next turn did not unblock after arrival release")
	}
	bot.releaseThreadReservation("C1", "T1", next)
}

func TestSlackHistoryAtTurnStartExcludesLaterUserButKeepsPriorReply(t *testing.T) {
	history := []chathistory.HistoryMessage{
		{MessageID: incompleteMessageID, Text: "diagnostic"},
		{MessageID: "1786092800.000001", Text: "prior"},
		{MessageID: "1786092826.330419", Text: "trigger"},
		{MessageID: "1786092827.000001", Text: "future"},
		{MessageID: "1786092828.000001", UserID: "B1", Text: "reply to prior", IsBot: true},
		{MessageID: "1786092829.000001", UserID: "B2", Text: "future external bot", IsBot: true},
	}
	got := slackHistoryAtTurnStart(history, "1786092826.330419", "B1")
	if len(got) != 4 || got[0].Text != "diagnostic" || got[1].Text != "prior" || got[2].Text != "trigger" || got[3].Text != "reply to prior" {
		t.Fatalf("history = %+v, want diagnostic, prior, trigger, and prior reply only", got)
	}
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

// --- postMessage / chat.update markdown_text tests ---

// TestBotPostMessageSendsMarkdownTextOnly verifies that postMessage emits
// only the markdown_text form field. Pairing it with text triggers Slack's
// markdown_text_conflict error and the call silently fails — observed in
// production (2026-05-19) and the cause of stream finalize truncation in
// multi-chunk replies. Regression guard: if a future change re-adds
// MsgOptionText to postMessage, this test must fail.
func TestBotPostMessageSendsMarkdownTextOnly(t *testing.T) {
	type captured struct {
		text         string
		markdownText string
		called       int
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			_ = r.ParseForm()
			got.text = r.FormValue("text")
			got.markdownText = r.FormValue("markdown_text")
			got.called++
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"123.456"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
	}

	if !bot.postMessage(context.Background(), "C1", "", "hello world") {
		t.Fatal("postMessage should succeed against a mock returning ok")
	}
	if got.called != 1 {
		t.Fatalf("chat.postMessage called %d times, want 1", got.called)
	}
	if got.markdownText != "hello world" {
		t.Errorf("markdown_text = %q, want %q", got.markdownText, "hello world")
	}
	if got.text != "" {
		t.Errorf("text must be empty to avoid markdown_text_conflict, got %q", got.text)
	}
}

// TestFinalizeUpdateOptsSendsMarkdownTextOnly verifies that the stream-finalize
// chat.update call wires markdown_text alone, with no text field. Pairing
// MsgOptionText with MsgOptionMarkdownText was the root cause of the
// "{accumulated stream buffer} + {final body}" double-render bug (see the
// IMPORTANT comment in sendToAgent). Mirrors TestBotPostMessageSendsMarkdownTextOnly
// but covers the chat.update path — the postMessage test alone does not
// guard against re-adding MsgOptionText to the finalize update slice.
func TestFinalizeUpdateOptsSendsMarkdownTextOnly(t *testing.T) {
	type captured struct {
		text         string
		markdownText string
		threadTS     string
		called       int
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.update":
			_ = r.ParseForm()
			got.text = r.FormValue("text")
			got.markdownText = r.FormValue("markdown_text")
			got.threadTS = r.FormValue("thread_ts")
			got.called++
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"123.456"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	opts := finalizeUpdateOpts("hello body", "thread.999")
	if _, _, _, err := api.UpdateMessageContext(context.Background(), "C1", "stream.111", opts...); err != nil {
		t.Fatalf("UpdateMessageContext failed: %v", err)
	}

	if got.called != 1 {
		t.Fatalf("chat.update called %d times, want 1", got.called)
	}
	if got.markdownText != "hello body" {
		t.Errorf("markdown_text = %q, want %q", got.markdownText, "hello body")
	}
	if got.text != "" {
		t.Errorf("text must be empty to avoid double-render with the streamed buffer, got %q", got.text)
	}
	if got.threadTS != "thread.999" {
		t.Errorf("thread_ts = %q, want %q", got.threadTS, "thread.999")
	}
}

// TestFinalizeUpdateOptsOmitsThreadTSWhenEmpty guards the conditional MsgOptionTS
// append — passing an empty threadTS would otherwise leak `thread_ts=` to the
// wire and chat.update would reject it.
func TestFinalizeUpdateOptsOmitsThreadTSWhenEmpty(t *testing.T) {
	var sawTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			sawTS = r.FormValue("thread_ts")
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	opts := finalizeUpdateOpts("body", "")
	if _, _, _, err := api.UpdateMessageContext(context.Background(), "C1", "stream.222", opts...); err != nil {
		t.Fatalf("UpdateMessageContext failed: %v", err)
	}
	if sawTS != "" {
		t.Errorf("thread_ts must be empty when input threadTS is empty, got %q", sawTS)
	}
}

// TestDeliveryFailureNoticeDoesNotAttributeCause guards against regressing
// the notice wording back to a cause-specific phrasing like "too long".
// The notice is posted from any deliveredAll=false path in sendToAgent
// (stream-finalize and batch-fallback), and those failures may come from
// chunkPostTimeout expiry, transient Slack API errors, or context cancel
// — not just oversized replies. The text must stay cause-neutral so users
// aren't misled into thinking they hit a length limit when they didn't.
func TestDeliveryFailureNoticeDoesNotAttributeCause(t *testing.T) {
	forbidden := []string{"too long", "too large", "exceeded", "limit"}
	lower := strings.ToLower(deliveryFailureNotice)
	for _, sub := range forbidden {
		if strings.Contains(lower, sub) {
			t.Errorf("deliveryFailureNotice = %q must not imply specific cause %q", deliveryFailureNotice, sub)
		}
	}
	if !strings.Contains(deliveryFailureNotice, "could not be delivered") {
		t.Errorf("deliveryFailureNotice = %q should describe a generic delivery failure", deliveryFailureNotice)
	}
}

// TestPostMessageRateLimitNoExtraSleepOnFinalAttempt guards against the
// rate-limit retry loop sleeping after the last permitted attempt fails.
// Before the fix, an attempt == maxRateLimitRetry that hit a 429 still
// spent attempt+1 seconds in time.After before exiting the loop, eating
// chunkPostTimeout budget for no subsequent retry. After the fix the loop
// must:
//   - perform exactly maxRateLimitRetry+1 PostMessage calls
//   - sleep at most maxRateLimitRetry times (one per gap between attempts)
//   - return false promptly when the final attempt is also rate-limited
func TestPostMessageRateLimitNoExtraSleepOnFinalAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleeps atomic.Int32
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
		// Use a synchronous "instant fire" sleeper so the test does not
		// depend on real wall-clock waits while still counting how many
		// times the loop tried to sleep.
		rateLimitSleep: func(d time.Duration) <-chan time.Time {
			sleeps.Add(1)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}

	if bot.postMessage(context.Background(), "C1", "", "hello") {
		t.Fatal("postMessage should return false when all attempts are rate limited")
	}

	if got := calls.Load(); got != int32(maxRateLimitRetry+1) {
		t.Errorf("PostMessage call count = %d, want %d (maxRateLimitRetry+1)", got, maxRateLimitRetry+1)
	}
	if got := sleeps.Load(); got != int32(maxRateLimitRetry) {
		t.Errorf("rateLimitSleep invocations = %d, want %d (one per inter-attempt gap; no sleep after final attempt)", got, maxRateLimitRetry)
	}
}

// scriptedMgr returns a pre-canned event stream from ChatOneShot so
// sendToAgent's event loop can be driven deterministically in tests.
type scriptedMgr struct {
	events          []agent.ChatEvent
	lastOneShotOpts agent.OneShotOpts
}

func (m *scriptedMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (m *scriptedMgr) ChatOneShot(_ context.Context, _, _ string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	m.lastOneShotOpts = opts
	ch := make(chan agent.ChatEvent, len(m.events)+1)
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// streamScript drives a mockSlackServer for sendToAgent-level tests of
// the stream-restart behavior. It hands out fresh stream timestamps from
// streamTSs in order, fails the appendStream call at the indexes in
// killAt with message_not_in_streaming_state, and counts every relevant
// API call so the test can assert on the resulting sequence. Counters
// and the issued/stopped lists are mutated from the HTTP handler
// goroutine; the test reads them only after sendToAgent returns.
type streamScript struct {
	streamTSs  []string // returned by chat.startStream, in order
	killAt     []int    // 0-based indexes of appendStream calls that should fail with message_not_in_streaming_state
	failUpdate bool     // chat.update returns an error (simulate finalize replacement failure)
	failPost   bool     // chat.postMessage returns an error (simulate batch/fallback delivery failure)
	// 0-based chat.delete call indexes that return HTTP 429. Tests use this
	// to verify Retry-After recovery and failed eager-cleanup handoff.
	rateLimitDeleteAt []int
	repliesJSON       string // optional conversations.replies response body

	startCalls   int
	appendCalls  int
	stopCalls    int
	updateCalls  int
	postCalls    int
	deleteCalls  int
	issuedTS     []string // ts values returned by chat.startStream
	stoppedTS    []string // ts values seen by chat.stopStream
	deletedTS    []string // ts values seen by chat.delete (dead-stream cleanup)
	appendOnTS   []string // ts the bot tried to append to (in order)
	lastUpdateTS string
	lastUpdateMD string
	lastPostMD   string
}

// newStreamServer returns a mock Slack server that delegates streaming
// calls to script and otherwise mimics mockSlackServer.
func newStreamServer(t *testing.T, script *streamScript) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/chat.startStream":
			idx := script.startCalls
			script.startCalls++
			if idx >= len(script.streamTSs) {
				fmt.Fprintf(w, `{"ok":false,"error":"too_many_streams"}`)
				return
			}
			ts := script.streamTSs[idx]
			script.issuedTS = append(script.issuedTS, ts)
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, ts)
		case "/chat.appendStream":
			idx := script.appendCalls
			script.appendCalls++
			script.appendOnTS = append(script.appendOnTS, r.FormValue("ts"))
			for _, k := range script.killAt {
				if k == idx {
					fmt.Fprintf(w, `{"ok":false,"error":"message_not_in_streaming_state"}`)
					return
				}
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/chat.stopStream":
			script.stopCalls++
			script.stoppedTS = append(script.stoppedTS, r.FormValue("ts"))
			fmt.Fprintf(w, `{"ok":true}`)
		case "/chat.update":
			script.updateCalls++
			script.lastUpdateTS = r.FormValue("ts")
			script.lastUpdateMD = r.FormValue("markdown_text")
			if script.failUpdate {
				fmt.Fprintf(w, `{"ok":false,"error":"message_not_found"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/chat.postMessage":
			script.postCalls++
			script.lastPostMD = r.FormValue("markdown_text")
			if script.failPost {
				fmt.Fprintf(w, `{"ok":false,"error":"channel_not_found"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"post.999"}`)
		case "/chat.delete":
			idx := script.deleteCalls
			script.deleteCalls++
			script.deletedTS = append(script.deletedTS, r.FormValue("ts"))
			if slices.Contains(script.rateLimitDeleteAt, idx) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, r.FormValue("ts"))
		case "/conversations.replies":
			if script.repliesJSON != "" {
				fmt.Fprint(w, script.repliesJSON)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"messages":[]}`)
		case "/conversations.history":
			fmt.Fprintf(w, `{"ok":true,"messages":[]}`)
		case "/assistant.threads.setStatus":
			fmt.Fprintf(w, `{"ok":true}`)
		case "/auth.test":
			fmt.Fprintf(w, `{"ok":true,"user_id":"UBOTTEST","user":"testbot","team":"T1","team_id":"T1"}`)
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBotWithStream(t *testing.T, mgr ChatManager, srv *httptest.Server) *Bot {
	t.Helper()
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	sm := socketmode.New(api)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Bot{
		agentID:      "test-agent",
		agentDataDir: "", // empty → skip chathistory writes
		config:       agent.SlackBotConfig{Enabled: true},
		api:          api,
		sm:           sm,
		mgr:          mgr,
		logger:       testLogger,
		botUserID:    "UBOTTEST",
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		threadLocks:  make(map[string]*threadLock),
		userCache:    make(map[string]string),
		sem:          make(chan struct{}, maxConcurrentChats),
		// Run the dead-stream cleanup goroutine synchronously so the
		// test can observe its API calls after sendToAgent returns.
		runAsync: func(fn func()) { fn() },
	}
}

func TestSendToAgentSuppressesNoReplyToken(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "[[NO_"},
		{Type: "text", Delta: "REPLY]]"},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if !strings.Contains(mgr.lastOneShotOpts.SystemPromptExtra, noReplyToken) {
		t.Fatalf("Slack system prompt does not advertise no-reply token: %q", mgr.lastOneShotOpts.SystemPromptExtra)
	}
	if script.startCalls != 0 || script.appendCalls != 0 || script.stopCalls != 0 ||
		script.updateCalls != 0 || script.postCalls != 0 || script.deleteCalls != 0 {
		t.Fatalf("no-reply emitted Slack message calls: start=%d append=%d stop=%d update=%d post=%d delete=%d",
			script.startCalls, script.appendCalls, script.stopCalls, script.updateCalls, script.postCalls, script.deleteCalls)
	}
}

func TestSendToAgentNoReplyTerminalMessageWithoutTextEvent(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "done", Message: &agent.Message{Content: " \n" + noReplyToken + "\t"}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 0 || script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("terminal-only no-reply emitted Slack message calls: start=%d update=%d post=%d",
			script.startCalls, script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentUsesTerminalContentForNoReplyDecision(t *testing.T) {
	t.Run("terminal token overrides incomplete ordinary delta", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.1"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: "partial ordinary text"},
			{Type: "done", Message: &agent.Message{Content: noReplyToken}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if script.postCalls != 0 || script.updateCalls != 0 {
			t.Fatalf("authoritative terminal token delivered content: post=%d update=%d", script.postCalls, script.updateCalls)
		}
		if !containsString(script.deletedTS, "stream.1") {
			t.Fatalf("stream containing incomplete delta was not deleted: %v", script.deletedTS)
		}
	})

	t.Run("terminal ordinary text overrides streamed token", func(t *testing.T) {
		script := &streamScript{streamTSs: []string{"stream.unused"}}
		srv := newStreamServer(t, script)
		mgr := &scriptedMgr{events: []agent.ChatEvent{
			{Type: "text", Delta: noReplyToken},
			{Type: "done", Message: &agent.Message{Content: "ordinary final response"}},
		}}
		bot := newBotWithStream(t, mgr, srv)

		bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

		if script.postCalls != 1 || script.lastPostMD != "ordinary final response" {
			t.Fatalf("authoritative ordinary response not delivered: posts=%d body=%q", script.postCalls, script.lastPostMD)
		}
	})
}

func TestSendToAgentNeverLeaksNoReplyTokenOnError(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.unused"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}, ErrorMessage: "backend failed"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.postCalls != 1 {
		t.Fatalf("error after no-reply token should post one generic failure, got %d", script.postCalls)
	}
	if strings.Contains(script.lastPostMD, noReplyToken) {
		t.Fatalf("no-reply token leaked in failure message: %q", script.lastPostMD)
	}
}

func TestSendToAgentNeverLeaksIncompleteNoReplyPrefix(t *testing.T) {
	tests := []struct {
		name   string
		events []agent.ChatEvent
	}{
		{
			name: "event stream closes without done",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "[[NO_"},
			},
		},
		{
			name: "done reports backend error",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "[[NO_"},
				{Type: "done", Message: &agent.Message{Content: "[[NO_"}, ErrorMessage: "backend failed"},
			},
		},
		{
			name: "authoritative terminal prefix replaces ordinary delta",
			events: []agent.ChatEvent{
				{Type: "text", Delta: "ordinary streamed text"},
				{Type: "done", Message: &agent.Message{Content: "[[NO_"}, ErrorMessage: "backend failed"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := &streamScript{streamTSs: []string{"stream.unused"}}
			srv := newStreamServer(t, script)
			bot := newBotWithStream(t, &scriptedMgr{events: tc.events}, srv)

			bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

			if script.postCalls != 1 {
				t.Fatalf("incomplete control prefix should post one generic failure, got %d", script.postCalls)
			}
			if strings.Contains(script.lastPostMD, "[[NO_") {
				t.Fatalf("incomplete no-reply prefix leaked in failure message: %q", script.lastPostMD)
			}
		})
	}
}

func TestSendToAgentNoReplyDeletesEarlierToolStream(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 1 {
		t.Fatalf("chat.startStream calls = %d, want 1 for the earlier tool indicator", script.startCalls)
	}
	if !containsString(script.stoppedTS, "stream.1") || !containsString(script.deletedTS, "stream.1") {
		t.Fatalf("suppressed tool stream was not stopped and deleted; stopped=%v deleted=%v", script.stoppedTS, script.deletedTS)
	}
	if script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("suppressed tool stream delivered content: update=%d post=%d", script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentNoReplyDeleteRecoversFromRateLimit(t *testing.T) {
	script := &streamScript{
		streamTSs:         []string{"stream.1"},
		rateLimitDeleteAt: []int{0},
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)
	var sleeps atomic.Int32
	bot.rateLimitSleep = func(time.Duration) <-chan time.Time {
		sleeps.Add(1)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.deleteCalls != 2 {
		t.Fatalf("chat.delete calls = %d, want one rate-limited attempt plus one retry", script.deleteCalls)
	}
	if sleeps.Load() != 1 {
		t.Fatalf("rate-limit sleeps = %d, want 1", sleeps.Load())
	}
	if countString(script.deletedTS, "stream.1") != 2 {
		t.Fatalf("stream.1 delete attempts = %d, want 2; deletedTS=%v", countString(script.deletedTS, "stream.1"), script.deletedTS)
	}
}

func TestSendToAgentNoReplyRetriesFailedSupersededStreamDeletion(t *testing.T) {
	rotations := maxRetainedDeadStreams + 1
	streams := make([]string, rotations)
	killRotations := make([]int, rotations)
	events := make([]agent.ChatEvent, rotations)
	for i := range rotations {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
		killRotations[i] = i
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events,
		agent.ChatEvent{Type: "text", Delta: noReplyToken},
		agent.ChatEvent{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	)
	// Exhaust every rate-limit attempt made by the eager cleanup of the
	// first superseded stream. Suppression must retain that TS and make a
	// fifth, successful delete attempt before returning.
	rateLimitedDeletes := make([]int, maxRateLimitRetry+1)
	for i := range rateLimitedDeletes {
		rateLimitedDeletes[i] = i
	}
	script := &streamScript{
		streamTSs:         streams,
		killAt:            killRotations,
		rateLimitDeleteAt: rateLimitedDeletes,
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	bot.rateLimitSleep = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if got := countString(script.deletedTS, "stream.1"); got != maxRateLimitRetry+2 {
		t.Fatalf("stream.1 delete attempts = %d, want %d (exhausted eager cleanup + suppression retry); deletedTS=%v",
			got, maxRateLimitRetry+2, script.deletedTS)
	}
	for _, ts := range streams {
		if !containsString(script.deletedTS, ts) {
			t.Errorf("suppressed stream %q was never deleted; deletedTS=%v", ts, script.deletedTS)
		}
	}
	if script.updateCalls != 0 || script.postCalls != 0 {
		t.Fatalf("no-reply delivered content after cleanup retry: update=%d post=%d", script.updateCalls, script.postCalls)
	}
}

func TestSendToAgentNoReplyLeavesUserAsLastThreadHistoryEntry(t *testing.T) {
	const (
		channel   = "C1"
		threadTS  = "1700000000.000100"
		messageTS = "1700000001.000200"
	)
	script := &streamScript{
		streamTSs: []string{"stream.unused"},
		// Simulate conversations.replies succeeding before Slack has made the
		// triggering event visible: the response contains only the root.
		repliesJSON: `{"ok":true,"messages":[` +
			`{"type":"message","user":"UROOT","text":"root","ts":"` + threadTS + `","thread_ts":"` + threadTS + `"}` +
			`]}`,
	}
	srv := newStreamServer(t, script)
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: noReplyToken},
		{Type: "done", Message: &agent.Message{Content: noReplyToken}},
	}}
	bot := newBotWithStream(t, mgr, srv)
	bot.agentDataDir = t.TempDir()

	path := chathistory.HistoryFilePath(bot.agentDataDir, platformSlack, channel, threadTS)
	if err := chathistory.AppendMessages(path, []chathistory.HistoryMessage{
		{Platform: platformSlack, ChannelID: channel, ThreadID: threadTS, MessageID: threadTS, UserID: "UROOT", Text: "root"},
		{Platform: platformSlack, ChannelID: channel, ThreadID: threadTS, MessageID: "1700000000.bot", UserID: bot.botUserID, Text: "prior answer", IsBot: true},
	}); err != nil {
		t.Fatal(err)
	}

	bot.sendToAgent(context.Background(), channel, threadTS, threadTS, messageTS, "ping", "alice", "U123")
	if got := mgr.lastOneShotOpts.History; len(got) != 2 || got[0].Text != "root" || got[1].Text != "prior answer" {
		t.Fatalf("ChatOneShot history = %+v, want canonical prior Slack transcript", got)
	}
	if mgr.lastOneShotOpts.HistorySelfUserID != bot.botUserID {
		t.Fatalf("HistorySelfUserID = %q, want %q", mgr.lastOneShotOpts.HistorySelfUserID, bot.botUserID)
	}

	history, err := chathistory.LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[len(history)-1].MessageID != messageTS || history[len(history)-1].IsBot {
		t.Fatalf("last history entry after no-reply is not the triggering user message: %+v", history)
	}
	for _, msg := range history {
		if strings.Contains(msg.Text, noReplyToken) {
			t.Fatalf("no-reply token was persisted to Slack history: %+v", msg)
		}
	}
	if bot.shouldAutoReply(channel, threadTS, "another unmentioned message") {
		t.Fatal("no-reply should leave the triggering user as last history entry and stop auto-reply chaining")
	}
}

func TestSendToAgentFlushesNoReplyPrefixWhenResponseDiverges(t *testing.T) {
	script := &streamScript{streamTSs: []string{"stream.1"}}
	srv := newStreamServer(t, script)
	text := noReplyToken + " is the control token"
	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "text", Delta: "[[NO_"},
		{Type: "text", Delta: "REPLY]] is the control token"},
		{Type: "done", Message: &agent.Message{Content: text}},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != 1 || script.updateCalls != 1 {
		t.Fatalf("ordinary response after token prefix was not delivered: start=%d update=%d", script.startCalls, script.updateCalls)
	}
	if script.lastUpdateMD != text {
		t.Fatalf("final response = %q, want %q", script.lastUpdateMD, text)
	}
}

// TestSendToAgentRestartsOnDeadStream drives sendToAgent end-to-end with
// a tool_use event that triggers stream open + a doomed append, then a
// text event whose first append hits the stream-closed error. The bot
// must:
//   - open ts-1 on the tool_use,
//   - drop ts-1 after appendStream returns message_not_in_streaming_state,
//   - open ts-2 on the next tool_use (since text events are throttled
//     and won't open a new stream until the throttle clears, we use a
//     second tool_use which bypasses the throttle),
//   - flush the carried-over delta + new indicator on ts-2,
//   - chat.update ts-2 with the full response text,
//   - chat.delete the dead ts-1 during finalize cleanup (the full reply
//     landed on ts-2, so the frozen ts-1 partial is a duplicate).
//
// This is the integration guard for the silent-truncation bug that was
// the original motivation for these changes.
func TestSendToAgentRestartsOnDeadStream(t *testing.T) {
	script := &streamScript{
		streamTSs: []string{"stream.1", "stream.2"},
		// Kill the very first appendStream — that's the indicator
		// append on the first tool_use event. The bot should drop
		// ts-1, then the next tool_use opens ts-2 and lands the
		// indicator there.
		killAt: []int{0},
	}
	srv := newStreamServer(t, script)

	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Delta: "hello world"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if !mgr.lastOneShotOpts.DisableKojoAttachmentInstructions {
		t.Error("Slack ChatOneShot must disable Kojo response-attachment instructions")
	}

	if script.startCalls != 2 {
		t.Errorf("chat.startStream calls = %d, want 2 (initial + 1 restart)", script.startCalls)
	}
	if len(script.issuedTS) >= 2 && script.issuedTS[0] != "stream.1" {
		t.Errorf("first startStream returned %q, want %q", script.issuedTS[0], "stream.1")
	}
	if !containsString(script.deletedTS, "stream.1") {
		t.Errorf("chat.delete not called on dead stream.1; deletedTS=%v", script.deletedTS)
	}
	if containsString(script.deletedTS, "stream.2") {
		t.Errorf("chat.delete must NOT touch the live final stream.2; deletedTS=%v", script.deletedTS)
	}
	if script.lastUpdateTS != "stream.2" {
		t.Errorf("final chat.update targeted %q, want %q", script.lastUpdateTS, "stream.2")
	}
	if !strings.Contains(script.lastUpdateMD, "hello world") {
		t.Errorf("final chat.update markdown_text = %q, want it to contain %q", script.lastUpdateMD, "hello world")
	}
}

// TestSendToAgentRestartBurstCapFallsBackToBatchPost forces every
// appendStream to die in a tight burst so the bot exhausts its
// maxStreamRestarts budget inside streamRestartWindow. After the circuit
// breaker trips, the remaining text must reach the user via
// chat.postMessage (batch fallback) rather than being silently dropped.
// Also asserts that every dead streamTS gets chat.delete'd during finalize
// cleanup (the full reply landed via batch post, so the N frozen partials
// are duplicates) so the channel doesn't render N stuck streaming messages.
func TestSendToAgentRestartBurstCapFallsBackToBatchPost(t *testing.T) {
	// Generate maxStreamRestarts+2 stream TSs so the bot has more
	// inventory than it can possibly use. The cap check must trip
	// before the inventory runs out.
	streams := make([]string, maxStreamRestarts+2)
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	// Kill every appendStream call.
	killAll := make([]int, 200)
	for i := range killAll {
		killAll[i] = i
	}
	script := &streamScript{streamTSs: streams, killAt: killAll}
	srv := newStreamServer(t, script)

	// Use tool_use events (bypass the 1s throttle on text) so each
	// event reliably triggers a fresh appendStream attempt.
	events := make([]agent.ChatEvent, maxStreamRestarts+3)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	// End with a text event so response.Builder is non-empty and the
	// finalize path actually emits a chunk (no text → "something went
	// wrong" branch instead).
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"})

	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	// startStream must be called exactly maxStreamRestarts+1 times
	// (initial open + maxStreamRestarts restarts before the cap trips).
	// Equality matters here: the previous `>=` cap condition allowed
	// only maxStreamRestarts opens (off-by-one), and a `> +1` assertion
	// would happily pass that regression. Asserting equality keeps the
	// cap semantics pinned down.
	if script.startCalls != maxStreamRestarts+1 {
		t.Errorf("chat.startStream called %d times, want %d (initial + maxStreamRestarts)",
			script.startCalls, maxStreamRestarts+1)
	}
	// All dead streams should be chat.delete'd during finalize: every
	// issued stream died, the batch post carried the full reply, so each
	// frozen partial is a duplicate that must be removed.
	if len(script.deletedTS) != script.startCalls {
		t.Errorf("chat.delete called on %d streams, want %d (every issued stream)",
			len(script.deletedTS), script.startCalls)
	}
	// Fallback path: at least one chat.postMessage for the final text.
	if script.postCalls == 0 {
		t.Error("expected at least one chat.postMessage as batch-fallback after cap, got 0")
	}
}

func TestTrimStreamDeathsOutsideWindowBoundariesAndRollback(t *testing.T) {
	now := time.Unix(1_000, 0)
	cutoff := now.Add(-streamRestartWindow)
	cases := []struct {
		name   string
		deaths []time.Time
		want   []time.Time
	}{
		{
			name: "before cutoff is expired",
			deaths: []time.Time{
				cutoff.Add(-time.Nanosecond),
			},
			want: nil,
		},
		{
			name: "cutoff is inclusive",
			deaths: []time.Time{
				cutoff,
			},
			want: []time.Time{cutoff},
		},
		{
			name: "after cutoff is recent",
			deaths: []time.Time{
				cutoff.Add(time.Nanosecond),
			},
			want: []time.Time{cutoff.Add(time.Nanosecond)},
		},
		{
			name: "clock rollback retains future deaths conservatively",
			deaths: []time.Time{
				now.Add(time.Second),
				now.Add(-2 * streamRestartWindow),
			},
			want: []time.Time{now.Add(time.Second)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimStreamDeathsOutsideWindow(append([]time.Time(nil), tc.deaths...), now)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("trimStreamDeathsOutsideWindow() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSendToAgentAllowsSlowStreamRotation verifies that the restart cap is
// a burst circuit breaker, not a lifetime limit. Slack regularly expires an
// otherwise healthy stream after roughly five minutes. A long-running turn
// may therefore need more than maxStreamRestarts replacements, provided the
// deaths are spaced outside streamRestartWindow. The final live stream must
// still receive chat.update rather than forcing a batch post after ~30 min.
func TestSendToAgentAllowsSlowStreamRotation(t *testing.T) {
	const extraRotations = 2
	rotations := maxStreamRestarts + extraRotations
	streams := make([]string, rotations+1) // one live stream after all rotations
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	killRotations := make([]int, rotations)
	for i := range killRotations {
		killRotations[i] = i
	}
	script := &streamScript{streamTSs: streams, killAt: killRotations}
	srv := newStreamServer(t, script)

	events := make([]agent.ChatEvent, rotations)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"})

	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		// startStream and dropStream both consult the clock. Advancing by
		// more than the whole window on each observation guarantees that
		// the preceding death is outside the next start's rolling window.
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	if script.startCalls != rotations+1 {
		t.Errorf("chat.startStream called %d times, want %d slow rotations plus final live stream",
			script.startCalls, rotations+1)
	}
	if script.postCalls != 0 {
		t.Errorf("chat.postMessage calls = %d, want 0; slow rotation must not trip batch fallback", script.postCalls)
	}
	if script.lastUpdateTS != streams[len(streams)-1] {
		t.Errorf("final chat.update targeted %q, want final live stream %q",
			script.lastUpdateTS, streams[len(streams)-1])
	}
	if len(script.deletedTS) != rotations {
		t.Errorf("chat.delete called on %d streams, want %d dead rotations", len(script.deletedTS), rotations)
	}
}

func TestSendToAgentBoundsDeadStreamArtifactsDuringSlowRotation(t *testing.T) {
	rotations := maxRetainedDeadStreams + 2
	streams := make([]string, rotations+1)
	for i := range streams {
		streams[i] = fmt.Sprintf("stream.%d", i+1)
	}
	killRotations := make([]int, rotations)
	for i := range killRotations {
		killRotations[i] = i
	}
	script := &streamScript{
		streamTSs:  streams,
		killAt:     killRotations,
		failUpdate: true,
		failPost:   true,
	}
	srv := newStreamServer(t, script)

	events := make([]agent.ChatEvent, rotations)
	for i := range events {
		events[i] = agent.ChatEvent{Type: "tool_use", ToolName: "Bash"}
	}
	events = append(events, agent.ChatEvent{Type: "text", Delta: "final words"})
	mgr := &scriptedMgr{events: events}
	bot := newBotWithStream(t, mgr, srv)
	var clock atomic.Int64
	bot.streamNow = func() time.Time {
		ns := clock.Add(int64(streamRestartWindow + time.Second))
		return time.Unix(0, ns)
	}

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	eagerDeletes := rotations - maxRetainedDeadStreams
	if len(script.deletedTS) != eagerDeletes {
		t.Fatalf("chat.delete called on %d streams, want %d superseded artifacts", len(script.deletedTS), eagerDeletes)
	}
	for _, ts := range streams[:eagerDeletes] {
		if !containsString(script.deletedTS, ts) {
			t.Errorf("superseded stream %q was not deleted; deletedTS=%v", ts, script.deletedTS)
		}
	}
	for _, ts := range streams[eagerDeletes:rotations] {
		if !containsString(script.stoppedTS, ts) {
			t.Errorf("retained failure artifact %q was not stopped; stoppedTS=%v", ts, script.stoppedTS)
		}
	}
}

// TestSendToAgentKeepsDeadStreamWhenDeliveryFails verifies the safety net:
// if the final reply could NOT be delivered (chat.update fails AND the
// chat.postMessage fallback fails), the dead partial is preserved as a
// debugging/retry artifact — StopStream'd, not chat.delete'd. Deleting it
// there would leave the user with no content at all.
func TestSendToAgentKeepsDeadStreamWhenDeliveryFails(t *testing.T) {
	script := &streamScript{
		streamTSs:  []string{"stream.1", "stream.2"},
		killAt:     []int{0}, // kill the first append → drop stream.1, restart to stream.2
		failUpdate: true,     // finalize chat.update on stream.2 fails
		failPost:   true,     // batch/fallback postMessage also fails
	}
	srv := newStreamServer(t, script)

	mgr := &scriptedMgr{events: []agent.ChatEvent{
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Delta: "hello world"},
	}}
	bot := newBotWithStream(t, mgr, srv)

	bot.sendToAgent(context.Background(), "C1", "thread.123", "thread.123", "msg.456", "ping", "alice", "U123")

	// Delivery failed, so the dead stream.1 must be kept (StopStream),
	// not deleted.
	if containsString(script.deletedTS, "stream.1") {
		t.Errorf("dead stream.1 was deleted despite delivery failure; deletedTS=%v", script.deletedTS)
	}
	if !containsString(script.stoppedTS, "stream.1") {
		t.Errorf("dead stream.1 should be StopStream'd as a retry artifact; stoppedTS=%v", script.stoppedTS)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func countString(haystack []string, needle string) int {
	count := 0
	for _, s := range haystack {
		if s == needle {
			count++
		}
	}
	return count
}

// TestIsStreamClosedErr verifies the typed-error detection used by
// appendStream to decide whether to abandon a dead stream. The detection
// goes through errors.As + slack.SlackErrorResponse so a future slack-go
// change that wraps the error still trips the same code path.
func TestIsStreamClosedErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated error", errors.New("boom"), false},
		{"wrong slack error code", slack.SlackErrorResponse{Err: "channel_not_found"}, false},
		{"raw stream-closed", slack.SlackErrorResponse{Err: slackErrNotStreaming}, true},
		{"wrapped stream-closed", fmt.Errorf("append failed: %w", slack.SlackErrorResponse{Err: slackErrNotStreaming}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamClosedErr(tc.err); got != tc.want {
				t.Fatalf("isStreamClosedErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAppendStreamReturnsFalseOnStreamClosed verifies the critical bug-fix
// signal: when Slack rejects chat.appendStream with
// message_not_in_streaming_state, appendStream returns false so the
// caller can drop the dead streamTS and restart a fresh stream. Before
// this fix the error was silently logged at Debug and the caller kept
// pushing into the dead stream, producing the 30+ identical failures
// observed in production (2026-06-03 12:34-12:41).
func TestAppendStreamReturnsFalseOnStreamClosed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "appendStream") {
			calls.Add(1)
			fmt.Fprintf(w, `{"ok":false,"error":"message_not_in_streaming_state"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{api: api, logger: testLogger}

	if bot.appendStream(context.Background(), "C1", "stream-ts", "delta") {
		t.Fatal("appendStream must return false on message_not_in_streaming_state")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("AppendStream call count = %d, want 1 (no retry on stream-closed)", got)
	}
}

// TestAppendStreamReturnsTrueOnUnknownError covers the conservative
// fallback for non-rate-limit, non-stream-closed errors. A transient
// Slack 5xx must NOT abandon the stream — the next call might succeed
// and prematurely churning the streamTS leaves an extra orphaned
// message in the channel. Caller keeps the same streamTS; if the error
// WAS terminal, the next append will return the typed stream-closed
// signal and we'll restart then.
func TestAppendStreamReturnsTrueOnUnknownError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "appendStream") {
			fmt.Fprintf(w, `{"ok":false,"error":"internal_error"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{api: api, logger: testLogger}

	if !bot.appendStream(context.Background(), "C1", "stream-ts", "delta") {
		t.Fatal("appendStream must return true on unknown errors (don't churn the stream)")
	}
}

// TestAppendStreamRateLimitNoExtraSleepOnFinalAttempt mirrors the
// postMessage guard for appendStream's identical retry loop. Both sites
// must stop sleeping once attempt == maxRateLimitRetry — a stray sleep
// there delays the entire stream-finalize path with no follow-up
// AppendStream call to justify the wait.
func TestAppendStreamRateLimitNoExtraSleepOnFinalAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slack streaming sends chat.appendStream — return 429 on any
		// path that isn't an irrelevant info call so the test stays
		// resilient to URL routing changes in slack-go.
		if strings.Contains(r.URL.Path, "appendStream") {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sleeps atomic.Int32
	bot := &Bot{
		api:    api,
		logger: testLogger,
		ctx:    ctx,
		rateLimitSleep: func(d time.Duration) <-chan time.Time {
			sleeps.Add(1)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}

	bot.appendStream(context.Background(), "C1", "stream-ts", "delta")

	if got := calls.Load(); got != int32(maxRateLimitRetry+1) {
		t.Errorf("AppendStream call count = %d, want %d (maxRateLimitRetry+1)", got, maxRateLimitRetry+1)
	}
	if got := sleeps.Load(); got != int32(maxRateLimitRetry) {
		t.Errorf("rateLimitSleep invocations = %d, want %d (one per inter-attempt gap; no sleep after final attempt)", got, maxRateLimitRetry)
	}
}
