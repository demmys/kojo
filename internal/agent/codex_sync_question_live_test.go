package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in native smoke: no repository/tool changes, one synthetic question.
func TestCodexSyncQuestionLive(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_SYNC_QUESTION") != "1" {
		t.Skip("authenticated CLI smoke")
	}
	for _, goal := range []bool{false, true} {
		name := "ordinary"
		if goal {
			name = "goal"
		}
		t.Run(name, func(t *testing.T) { testCodexSyncQuestionLive(t, goal) })
	}
}
func testCodexSyncQuestionLive(t *testing.T, goal bool) {
	home, _ := os.UserHomeDir()
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("APPDATA", configHome)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	backend := NewCodexBackend(slog.Default())
	ready := make(chan AnswerFunc, 1)
	prompt := "Use request_user_input to ask me which color to use, with Red and Blue as options. Use id color for the question. Wait for my answer. Do not finish until I answer. Then repeat the answer exactly."
	opts := ChatOptions{OnQuestionReady: func(fn AnswerFunc) { ready <- fn }}
	if goal {
		opts.Goal = &GoalRequest{Action: "start", Objective: prompt + " After receiving and repeating the answer, mark the goal complete."}
	}
	events, err := backend.Chat(ctx, &Agent{ID: "ag_sync_question_smoke", Tool: ToolCodex, Model: "gpt-6-astra"},
		prompt, "This is a question UI smoke test. Do not access files, web, MCP or external services. Only ask the requested synchronous question, wait for the answer, and mark the goal complete if active.", opts)
	if err != nil {
		t.Fatal(err)
	}
	var answer AnswerFunc
	select {
	case answer = <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	asked := false
	var submitted atomic.Bool
	delivered := make(chan error, 1)
	got := false
	complete := false
	for ev := range events {
		if asked && !submitted.Load() && (ev.Type == "done" || ev.Type == "question_resolved" || (ev.Goal != nil && ev.Goal.Status == "complete")) {
			t.Fatal("question resolved or run completed before answer")
		}
		if ev.Goal != nil && ev.Goal.Status == "complete" {
			complete = true
		}
		if ev.Type == "user_question" {
			if asked {
				t.Fatal("duplicate question")
			}
			asked = true
			if ev.QuestionBlocking == nil || !*ev.QuestionBlocking {
				t.Fatal("RPC question must wait for answer")
			}
			go func() {
				select {
				case <-time.After(15 * time.Second):
				case <-ctx.Done():
					delivered <- ctx.Err()
					return
				}
				submitted.Store(true)
				delivered <- answer(ev.RequestID, map[string]any{"color": "Blue"}, false, "")
			}()
		}
		if ev.Type == "error" {
			t.Errorf("backend: %s", ev.ErrorMessage)
		}
		if ev.Type == "done" && ev.Message != nil {
			got = strings.HasPrefix(strings.TrimSpace(ev.Message.Content), "Blue")
			t.Log(ev.Message.Content)
		}
	}
	if !asked || !got || (goal && !complete) {
		t.Fatalf("asked=%v answer in final=%v goalComplete=%v ctx=%v", asked, got, complete, ctx.Err())
	}
	select {
	case err := <-delivered:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
