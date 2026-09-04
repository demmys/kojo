package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

func msg(role, content string) *Message {
	return &Message{Role: role, Content: content}
}

func TestBuildHistoryTurns_ShapesTheReplay(t *testing.T) {
	volatile := "<context>\n" + volatileContextSentinel + "\nnow: yesterday\n</context>\nreal question"

	turns := buildHistoryTurns([]*Message{
		msg("assistant", "orphan opener"),
		msg("user", volatile),
		msg("assistant", "an answer"),
		msg("system", "[System] cron fired"),
		msg("system", "\u26a0\ufe0f Error: dial tcp 127.0.0.1:8080: connection refused"),
		msg("user", "and a follow-up"),
		msg("assistant", ""),
		msg("tool", "ignored role"),
		msg("assistant", "  final  "),
	})

	want := []HistoryTurn{
		{Role: "user", Content: "real question"},
		{Role: "assistant", Content: "an answer"},
		{Role: "user", Content: "[System] cron fired\n\nand a follow-up"},
		{Role: "assistant", Content: "final"},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, turns[i], want[i])
		}
	}
}

func TestBuildHistoryTurns_EmptyAndUnusable(t *testing.T) {
	if got := buildHistoryTurns(nil); got != nil {
		t.Errorf("nil input = %+v, want nil", got)
	}
	if got := buildHistoryTurns([]*Message{nil, msg("user", "   ")}); got != nil {
		t.Errorf("blank input = %+v, want nil", got)
	}
	// Assistant-only history has nothing to answer; it must not lead.
	if got := buildHistoryTurns([]*Message{msg("assistant", "hi")}); got != nil {
		t.Errorf("assistant-only = %+v, want nil", got)
	}
}

func TestBuildHistoryTurns_CapsTurnsAndBudget(t *testing.T) {
	var msgs []*Message
	for i := 0; i < bareHistoryMaxTurns*2; i++ {
		msgs = append(msgs, msg("user", "q"), msg("assistant", "a"))
	}
	turns := buildHistoryTurns(msgs)
	if len(turns) > bareHistoryMaxTurns {
		t.Fatalf("kept %d turns, want <= %d", len(turns), bareHistoryMaxTurns)
	}
	if turns[0].Role != "user" {
		t.Errorf("replay opens with %q, want user", turns[0].Role)
	}

	// Per-turn truncation, then whole-turn drops until the total fits.
	long := strings.Repeat("あ", bareHistoryMaxRunesPerTurn*3)
	msgs = nil
	for i := 0; i < bareHistoryMaxTurns; i++ {
		msgs = append(msgs, msg("user", long), msg("assistant", long))
	}
	turns = buildHistoryTurns(msgs)
	total := 0
	for _, tn := range turns {
		n := len([]rune(tn.Content))
		if n > bareHistoryMaxRunesPerTurn {
			t.Errorf("turn kept %d runes, want <= %d", n, bareHistoryMaxRunesPerTurn)
		}
		total += n
	}
	if total > bareHistoryMaxRunesTotal {
		t.Errorf("replay is %d runes, want <= %d", total, bareHistoryMaxRunesTotal)
	}
	if len(turns) == 0 {
		t.Fatal("budget trimming dropped everything")
	}
}

func TestBackendReplaysHistory(t *testing.T) {
	if !backendReplaysHistory(&CustomBareBackend{}) {
		t.Error("custom-bare should get the replay")
	}
	if backendReplaysHistory(nil) {
		t.Error("nil backend should not")
	}
	// Session-owning backends must not: they would see the transcript twice.
	if backendReplaysHistory(&ClaudeBackend{}) {
		t.Error("claude should not get the replay")
	}
}

// The replay only matters if it reaches the wire, between the system
// prompt and the current turn.
func TestCustomBareChat_SendsHistoryInOrder(t *testing.T) {
	var got llamaCppRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := NewCustomBareBackend(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ch, err := b.Chat(context.Background(),
		&Agent{ID: "ag_hist", Tool: ToolCustomBare, CustomBaseURL: srv.URL, Model: "m"},
		"current turn", "SYSTEM",
		ChatOptions{
			// ChatOneShot derives this from the same canonical history for
			// sessionful backends. custom-bare must prefer its structured replay
			// rather than inject the formatted copy into the current turn too.
			FreshSessionContext: "formatted duplicate of older question and answer",
			History: []HistoryTurn{
				{Role: "user", Content: "older question"},
				{Role: "assistant", Content: "older answer"},
			},
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range ch {
	}

	want := []llamaCppMessage{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "older question"},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "current turn"},
	}
	if len(got.Messages) != len(want) {
		t.Fatalf("sent %d messages, want %d: %+v", len(got.Messages), len(want), got.Messages)
	}
	for i := range want {
		if got.Messages[i] != want[i] {
			t.Errorf("message %d = %+v, want %+v", i, got.Messages[i], want[i])
		}
	}
}

// An unanswered question at the tail must not become a second user
// message next to the current turn.
func TestCustomBareChat_FoldsTrailingUserTurn(t *testing.T) {
	var got llamaCppRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// A real per-turn message: volatile context, then the persona
	// anchor, then what the user actually typed.
	current := "<context>\n" + volatileContextSentinel + "\nnow: today\n</context>\n" +
		"<persona-anchor>\n" + personaAnchorHeader + "\nterse\n</persona-anchor>\n\n" +
		"current turn"

	b := NewCustomBareBackend(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ch, err := b.Chat(context.Background(),
		&Agent{ID: "ag_hist", Tool: ToolCustomBare, CustomBaseURL: srv.URL, Model: "m"},
		current, "",
		ChatOptions{History: []HistoryTurn{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "the turn that errored"},
		}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range ch {
	}

	folded := strings.Replace(current, "current turn", "the turn that errored\n\ncurrent turn", 1)
	want := []llamaCppMessage{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: folded},
	}
	if len(got.Messages) != len(want) {
		t.Fatalf("sent %d messages, want %d: %+v", len(got.Messages), len(want), got.Messages)
	}
	for i := range want {
		if got.Messages[i] != want[i] {
			t.Errorf("message %d = %+v, want %+v", i, got.Messages[i], want[i])
		}
	}
}

func TestTruncateHistoryTurn_RespectsCapWithMarker(t *testing.T) {
	out := truncateHistoryTurn(strings.Repeat("x", bareHistoryMaxRunesPerTurn+1))
	if n := len([]rune(out)); n != bareHistoryMaxRunesPerTurn {
		t.Errorf("truncated to %d runes, want exactly %d", n, bareHistoryMaxRunesPerTurn)
	}
	if !strings.Contains(out, historyTruncationMarker) {
		t.Error("truncated turn lost its marker")
	}
	short := strings.Repeat("x", bareHistoryMaxRunesPerTurn)
	if truncateHistoryTurn(short) != short {
		t.Error("a turn that exactly fits must pass through untouched")
	}
}

// The regenerate path replays only what precedes the message being
// re-run. Store-backed because the cursor is a DB concern.
func TestBuildHistoryTurnsBefore_StopsAtCursor(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))

	st, err := store.Open(context.Background(), store.Options{ConfigDir: configdir.Path()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		setGlobalStore(nil)
	})
	setGlobalStore(st)

	const agentID = "ag_replay"
	if _, err := st.InsertAgent(context.Background(), &store.AgentRecord{
		ID:       agentID,
		Name:     "bare",
		Settings: map[string]any{"tool": ToolCustomBare},
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	first := newUserMessage("first question", nil)
	answer := assembleAssistantMessage("first answer", "", nil, nil)
	pivot := newUserMessage("the message being regenerated", nil)
	stale := assembleAssistantMessage("answer about to be tombstoned", "", nil, nil)
	for _, m := range []*Message{first, answer, pivot, stale} {
		if err := appendMessage(agentID, m); err != nil {
			t.Fatalf("appendMessage: %v", err)
		}
	}

	var mgr *Manager
	full := mgr.BuildHistoryTurns(context.Background(), agentID)
	if len(full) != 4 {
		t.Fatalf("full replay = %d turns, want 4: %+v", len(full), full)
	}

	cut := mgr.BuildHistoryTurnsBefore(context.Background(), agentID, pivot.ID)
	want := []HistoryTurn{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	if len(cut) != len(want) {
		t.Fatalf("cursor replay = %d turns, want %d: %+v", len(cut), len(want), cut)
	}
	for i := range want {
		if cut[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, cut[i], want[i])
		}
	}
}

func TestFoldIntoUserMessage_KeepsPreambleFirst(t *testing.T) {
	ctxBlock := "<context>\n" + volatileContextSentinel + "\nnow: today\n</context>\n"
	anchor := "<persona-anchor>\n" + personaAnchorHeader + "\nterse\n</persona-anchor>\n\n"

	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain message", "hello", "older\n\nhello"},
		{"context only", ctxBlock + "hello", ctxBlock + "older\n\nhello"},
		{"context and anchor", ctxBlock + anchor + "hello", ctxBlock + anchor + "older\n\nhello"},
		{
			// A user message that merely opens with the same tag is
			// not kojo's preamble: no sentinel, so nothing is skipped.
			"lookalike tag", "<context>mine</context>\nhello",
			"older\n\n<context>mine</context>\nhello",
		},
	} {
		if got := foldIntoUserMessage(tc.in, "older"); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestIsInternalNotice(t *testing.T) {
	for _, in := range []string{
		NoticeErrorPrefix + "dial tcp: refused",
		NoticeTimeoutText,
		NoticeSwitchAbortedPrefix + "peer offline",
	} {
		if !isInternalNotice(in) {
			t.Errorf("%q should be an internal notice", in)
		}
	}
	for _, in := range []string{
		"[System] cron fired",
		"\u26a0\ufe0f 気をつけて",
		"",
	} {
		if isInternalNotice(in) {
			t.Errorf("%q should NOT be an internal notice", in)
		}
	}
}

// Rate-limit replies are re-roled from assistant to system by the
// manager; they are UI notices too and must not come back as user turns.
func TestBuildHistoryTurns_DropsRateLimitRow(t *testing.T) {
	turns := buildHistoryTurns([]*Message{
		msg("user", "q"),
		msg("system", "You've hit your limit. Try again later."),
		msg("user", "still there?"),
	})
	want := []HistoryTurn{{Role: "user", Content: "q\n\nstill there?"}}
	if len(turns) != 1 || turns[0] != want[0] {
		t.Errorf("got %+v, want %+v", turns, want)
	}
}
