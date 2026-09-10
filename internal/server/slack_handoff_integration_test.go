package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/slackbot"
	"github.com/slack-go/slack"
)

type slackIntegrationTurn struct {
	opts       agent.OneShotOpts
	capability string
	finish     chan struct{}
}

// This connects the real Bot's Activate→synthetic turn to capability mint/bind,
// local HTTP finalize and durable receipts. Slack/model execution are mocked;
// router/stream cleanup and the full device-switch orchestrator are not run.
type slackIntegrationManager struct {
	server *Server
	turns  chan slackIntegrationTurn
	count  atomic.Int32
}

func (m *slackIntegrationManager) Chat(context.Context, string, string, string, []agent.MessageAttachment, ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	panic("Bot should use ChatOneShot, not Chat")
}
func (m *slackIntegrationManager) ChatOneShot(ctx context.Context, id, _ string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	turn := slackIntegrationTurn{opts: opts, capability: m.server.mintHandoffArrivalCapability(id, opts.SessionKey, opts.HandoffArrivalReservation), finish: make(chan struct{})}
	content := fmt.Sprintf("finished-%d", m.count.Add(1))
	m.turns <- turn
	events := make(chan agent.ChatEvent, 1)
	go func() {
		defer close(events)
		select {
		case <-ctx.Done():
		case <-turn.finish:
			events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: content}}
		}
	}()
	return events, nil
}

// dispatch logs this decision synchronously immediately before invoking its
// WebUI fallback. Observe that real branch, not the Bot's ChatManager stub.
type handoffFallbackLog struct {
	slog.Handler
	calls atomic.Int32
}

func (h *handoffFallbackLog) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "device-switch origin conversation unavailable; falling back to main WebUI arrival" {
		h.calls.Add(1)
	}
	return h.Handler.Handle(ctx, r)
}

func TestSlackSyntheticTurnFinalizesNextHandoffWithoutEOF(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	pending, db := newPendingSyncTestServer(t)
	srv.pendingSyncDB, srv.pendingSyncKEK = db, pending.pendingSyncKEK
	srv.peerID = &peer.Identity{DeviceID: "hub"}
	fallbackLog := &handoffFallbackLog{Handler: slog.NewTextHandler(io.Discard, nil)}
	srv.logger = slog.New(fallbackLog)
	id := group.Members[0].AgentID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := srv.agents.Store().AcquireAgentLock(ctx, id, "windows", 0, 60000); err != nil {
		t.Fatal(err)
	}
	sockets := make(chan *websocket.Conn, 1)
	var replies atomic.Int32
	var slackURL string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"api.slack.com"}})
			if err != nil {
				return
			}
			defer c.CloseNow()
			if err := wsjson.Write(ctx, c, map[string]any{"type": "hello", "num_connections": 1}); err != nil {
				return
			}
			select {
			case sockets <- c:
			case <-ctx.Done():
				return
			}
			for {
				if _, _, err := c.Read(ctx); err != nil {
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			io.WriteString(w, `{"ok":true,"user_id":"UBOT","team_id":"TEAM"}`)
		case "/apps.connections.open":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(slackURL, "http:", "ws:", 1) + "/socket"})
		case "/users.info":
			io.WriteString(w, `{"ok":true,"user":{"id":"U1","name":"Alice","profile":{"display_name":"Alice"}}}`)
		case "/conversations.replies":
			io.WriteString(w, `{"ok":true,"messages":[],"has_more":false}`)
		case "/chat.postMessage":
			r.ParseForm()
			if r.Form.Get("channel") != "D1" || r.Form.Get("thread_ts") != "1.0" {
				t.Errorf("reply escaped thread: %v", r.Form)
			}
			ts := fmt.Sprintf("%d.0", 100+replies.Add(1))
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts, "channel": "D1", "message": map[string]any{"text": r.Form.Get("text"), "ts": ts}})
		default:
			io.WriteString(w, `{"ok":true,"ts":"3.0","channel":"D1","message":{"text":"finished","ts":"3.0"}}`)
		}
	}))
	defer api.Close()
	slackURL = api.URL // published before Bot.Run starts any request
	mgr := &slackIntegrationManager{server: srv, turns: make(chan slackIntegrationTurn, 4)}
	historyDir := t.TempDir()
	bot := slackbot.NewBot(ctx, id, historyDir, agent.SlackBotConfig{Enabled: true, ThreadReplies: true}, "xapp-test", "xoxb-test", mgr, srv.logger, slack.OptionAPIURL(api.URL+"/"))
	go bot.Run()
	defer func() { bot.Stop(); cancel() }()
	var socket *websocket.Conn
	select {
	case socket = <-sockets:
	case <-time.After(3 * time.Second):
		t.Fatal("mock Slack socket did not connect")
	}
	defer socket.CloseNow()
	envelope := map[string]any{"envelope_id": "e1", "type": "events_api", "payload": map[string]any{"type": "event_callback", "event": map[string]any{"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "move", "ts": "1.0", "thread_ts": "1.0"}}}
	if err := wsjson.Write(ctx, socket, envelope); err != nil {
		t.Fatal(err)
	}
	nextTurn := func() slackIntegrationTurn {
		t.Helper()
		select {
		case turn := <-mgr.turns:
			if turn.capability == "" {
				t.Fatal("real Slack turn supplied no executable capability")
			}
			return turn
		case <-time.After(3 * time.Second):
			t.Fatal("Slack arrival did not resume")
			return slackIntegrationTurn{}
		}
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.handlePeerAgentSyncFinalize(w, authedRequest(r, auth.Principal{Role: auth.RolePeer, PeerID: "windows"}))
	}))
	defer httpServer.Close()
	first := nextTurn()
	turn := first
	for _, op := range []string{"arrival-1", "arrival-2"} {
		if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{AgentID: id, OpID: op, SessionKey: turn.opts.SessionKey, SourceDeviceID: "windows", TargetDeviceID: "hub", Capability: turn.capability}); err != nil {
			t.Fatal(err)
		}
		prepareFencedIncomingForTest(t, srv, id, op, "windows")
		if err := srv.recordPendingAgentSync(ctx, id, op, pendingSyncEntry{SourceDeviceID: "windows", IncomingFenced: true}); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(peerAgentSyncFinalizeRequest{AgentID: id, OpID: op, SourceDeviceID: "windows", Continuation: &handoffContinuation{OriginPeerID: "hub", SessionKey: turn.opts.SessionKey, Capability: turn.capability}})
		// Test an actual HTTP response, including the connection that previously
		// disappeared with EOF. A duplicate finalize must not queue another turn.
		for attempt := range 2 {
			resp, err := httpServer.Client().Post(httpServer.URL, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			want := 200
			if attempt == 1 {
				want = 404
			}
			if resp.StatusCode != want {
				t.Fatalf("%s finalize %d: %s", op, resp.StatusCode, data)
			}
		}
		receipt, err := srv.agents.Store().GetIncomingHandoff(ctx, id, op)
		if err != nil || string(receipt.Phase) != "done" {
			t.Fatalf("receipt %+v %v", receipt, err)
		}
		if _, ok, err := srv.consumePendingAgentSync(ctx, id, op); ok || err != nil {
			t.Fatalf("pending remains: %v %v", ok, err)
		}
		close(turn.finish)
		turn = nextTurn()
		if turn.opts.SessionKey != first.opts.SessionKey || !turn.opts.ForceFreshSession || turn.opts.GoalUserID != "U1" {
			t.Fatal("arrival escaped Slack conversation")
		}
		if op == "arrival-1" {
			// Model the intervening outbound ownership transfer; the arriving model
			// then immediately requests the next return in its synthetic Slack turn.
			if _, err := srv.agents.Store().CompleteHandoffSelectedBlobs(ctx, id, "windows", nil, 300000); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(turn.finish)
	barrier, ok := turn.opts.HandoffArrivalReservation.(interface{ WaitSourceComplete(context.Context) error })
	if !ok {
		t.Fatal("missing completion barrier")
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	defer waitCancel()
	if err := barrier.WaitSourceComplete(waitCtx); err != nil {
		t.Fatal(err)
	}
	if replies.Load() != 3 {
		t.Fatalf("Slack replies=%d, want one per turn", replies.Load())
	}
	history, err := chathistory.LoadHistory(chathistory.HistoryFilePath(historyDir, "slack", "D1", "1.0"))
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	for _, h := range history {
		if h.IsBot {
			found[h.Text] = true
		}
	}
	for i := 1; i <= 3; i++ {
		if !found[fmt.Sprintf("finished-%d", i)] {
			t.Fatalf("turn %d response missing from persisted Slack history: %+v", i, history)
		}
	}
	if fallbackLog.calls.Load() != 0 {
		t.Fatal("finalize entered the main WebUI fallback branch")
	}
	select {
	case <-mgr.turns:
		t.Fatal("duplicate arrival admitted")
	default:
	}
}
