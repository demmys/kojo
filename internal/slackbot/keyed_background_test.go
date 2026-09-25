package slackbot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/slack-go/slack"
)

type slackCall struct{ path, thread, body string }

func newRecordingBot(t *testing.T) (*Bot, func() []slackCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []slackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, slackCall{path: r.URL.Path, thread: r.FormValue("thread_ts"), body: r.Form.Encode()})
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"post.1","messages":[]}`)
	}))
	t.Cleanup(srv.Close)
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bot := &Bot{
		agentID: "test-agent", agentDataDir: t.TempDir(),
		config: agent.SlackBotConfig{Enabled: true, ThreadReplies: true},
		api:    api, mgr: &mockMgr{}, logger: testLogger, botUserID: "UBOTTEST",
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		threadLocks: make(map[string]*threadLock), activeTurns: make(map[string][]*activeTurn),
		stoppingTurns: make(map[string]*activeTurn),
		userCache:     make(map[string]string), sem: make(chan struct{}, maxConcurrentChats),
	}
	return bot, func() []slackCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]slackCall(nil), calls...)
	}
}

func postedText(calls []slackCall) string {
	var sb strings.Builder
	for _, c := range calls {
		if strings.Contains(c.path, "chat.") {
			sb.WriteString(c.body)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func TestHandleKeyedBackgroundTurnPostsIntoThread(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 4)
	events <- agent.ChatEvent{Type: "text", Delta: "background job finished"}
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "background job finished"}}
	close(events)

	// A user turn holds the thread FIFO first; the synthetic admission must
	// wait for it rather than interleave.
	held := bot.reserveThread("C1", "1700.1")
	held.Wait()
	finished := make(chan struct{})
	go func() {
		bot.HandleKeyedBackgroundTurn("test-agent", slackSessionKey("test-agent", "C1", "1700.1"), events, func() {})
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("background turn bypassed the thread FIFO")
	case <-time.After(100 * time.Millisecond):
	}
	bot.releaseThreadReservation("C1", "1700.1", held)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("background delivery did not finish")
	}
	got := calls()
	var threaded bool
	for _, c := range got {
		if strings.Contains(c.path, "chat.") && c.thread == "1700.1" {
			threaded = true
		}
		if strings.Contains(c.path, "chat.") && c.thread != "" && c.thread != "1700.1" {
			t.Fatalf("posted into wrong thread: %+v", c)
		}
	}
	if !threaded || !strings.Contains(postedText(got), "background+job+finished") {
		t.Fatalf("background reply not posted into thread: %+v", got)
	}
	if bot.hasActiveTurn("C1", "1700.1") {
		t.Fatal("synthetic active turn leaked")
	}
}

func TestHandleKeyedBackgroundTurnIgnoresForeignKey(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 1)
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "x"}}
	close(events)
	cancelled := false
	bot.HandleKeyedBackgroundTurn("test-agent", "other-agent:slack:C1:1.0", events, func() { cancelled = true })
	if !cancelled || len(calls()) != 0 {
		t.Fatalf("foreign key: cancelled=%v calls=%v", cancelled, calls())
	}
}

func TestBackgroundPendingNoteAppendedToFinalReply(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 4)
	events <- agent.ChatEvent{Type: "text", Delta: "started it"}
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "started it"}, BackgroundTasksPending: 2}
	close(events)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	active := bot.registerActiveTurn("C1", "1700.2", turnCancel)
	bot.deliverAgentTurn(turnCtx, slackTurnDelivery{channel: "C1", threadTS: "1700.2", sessionKey: slackSessionKey("test-agent", "C1", "1700.2"), turnCtx: turnCtx, turnCancel: turnCancel, active: active}, events)
	want := "バックグラウンド処理 2件 実行中"
	text := postedText(calls())
	// Form bodies are URL-encoded; decode loosely by checking the helper text.
	if !strings.Contains(text, urlEncode(want)) {
		t.Fatalf("pending note missing; calls=%s", text)
	}
}

func TestBackgroundPendingNoteNotAppendedToNoReply(t *testing.T) {
	bot, calls := newRecordingBot(t)
	events := make(chan agent.ChatEvent, 2)
	events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: agent.SlackNoReplyToken}, BackgroundTasksPending: 1}
	close(events)
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	active := bot.registerActiveTurn("C1", "1700.3", turnCancel)
	bot.deliverAgentTurn(turnCtx, slackTurnDelivery{channel: "C1", threadTS: "1700.3", sessionKey: slackSessionKey("test-agent", "C1", "1700.3"), turnCtx: turnCtx, turnCancel: turnCancel, active: active}, events)
	if strings.Contains(postedText(calls()), urlEncode("バックグラウンド処理")) {
		t.Fatal("pending note must not be posted for NO_REPLY")
	}
}

func urlEncode(s string) string { return url.QueryEscape(s) }
