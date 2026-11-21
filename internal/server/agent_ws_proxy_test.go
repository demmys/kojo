package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAgentWSProxy_ConnectedHandshakeIsOrigins verifies that when the
// agent chat WS is proxied to a holder peer, the client sees exactly one
// "connected" handshake and it carries the PROXYING (origin) peer's
// version — the one that served the UI bundle — not the holder's.
// Forwarding the holder's version made the stale-frontend banner stick
// on every agent held by a peer running a different build.
func TestAgentWSProxy_ConnectedHandshakeIsOrigins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// Holder: sends its own handshake, then an ordinary event that
	// mentions "connected" in its payload (must pass through), then a
	// heartbeat.
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = writeJSON(ctx, c, agentConnectedMsg("holder-v9"))
		_ = writeJSON(ctx, c, map[string]any{"type": "text", "delta": `say "connected"`})
		_ = writeJSON(ctx, c, agentHeartbeatMsg())
		// Hold the socket open until the client goes away.
		_, _, _ = c.Read(ctx)
	}))
	t.Cleanup(holder.Close)

	const originVersion = "origin-v1"
	srv := &Server{logger: logger, version: originVersion}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.forwardWebSocketToPeer(w, r, srv.agentWSForward("ag_test", "holder-dev", holder.URL))
	}))
	t.Cleanup(origin.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(origin.URL, "http")+"/api/v1/agents/ag_test/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	var got []map[string]any
	for len(got) < 3 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read frame %d: %v (got %v)", len(got), err, got)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", data, err)
		}
		got = append(got, m)
	}

	if got[0]["type"] != "connected" || got[0]["version"] != originVersion {
		t.Fatalf("first frame = %v, want connected/%s", got[0], originVersion)
	}
	if got[1]["type"] != "text" {
		t.Fatalf("second frame = %v, want the holder's text event (holder handshake must be dropped)", got[1])
	}
	if got[2]["type"] != "ping" {
		t.Fatalf("third frame = %v, want ping", got[2])
	}
	conn.Close(websocket.StatusNormalClosure, "done")
}

func TestIsAgentConnectedFrame(t *testing.T) {
	hs, _ := json.Marshal(agentConnectedMsg("v0.149.0-9-g1effff94"))
	big := `{"type":"connected","version":"` + strings.Repeat("x", 4096) + `"}`
	cases := []struct {
		name string
		typ  websocket.MessageType
		data string
		want bool
	}{
		{"handshake", websocket.MessageText, string(hs), true},
		{"binary", websocket.MessageBinary, string(hs), false},
		{"long version", websocket.MessageText, big, true},
		{"mentions connected in payload", websocket.MessageText, `{"type":"text","delta":"\"connected\""}`, false},
		{"ping", websocket.MessageText, `{"type":"ping","t":1}`, false},
		{"not json", websocket.MessageText, `"connected"`, false},
	}
	for _, c := range cases {
		if got := isAgentConnectedFrame(c.typ, []byte(c.data)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
