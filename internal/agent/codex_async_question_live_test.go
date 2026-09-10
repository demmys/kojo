package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in native smoke: no repository/tool changes, one synthetic question.
func TestCodexAsyncQuestionLive(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_ASYNC_QUESTION") != "1" {
		t.Skip("authenticated CLI smoke")
	}
	for _, goal := range []bool{false, true} {
		name := "ordinary"
		if goal {
			name = "goal"
		}
		t.Run(name, func(t *testing.T) { testCodexAsyncQuestionLive(t, goal) })
	}
}
func testCodexAsyncQuestionLive(t *testing.T, goal bool) {
	home, _ := os.UserHomeDir()
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	backend := NewCodexBackend(slog.Default())
	ready := make(chan AnswerFunc, 1)
	prompt := "Use request_user_input_async to ask me which color to use, with Red and Blue as options. After asking, use clock.sleep to wait for my asynchronous answer. Do not finish until I answer. Then repeat the answer exactly."
	opts := ChatOptions{OnQuestionReady: func(fn AnswerFunc) { ready <- fn }}
	if goal {
		opts.Goal = &GoalRequest{Action: "start", Objective: prompt + " After receiving and repeating the answer, mark the goal complete."}
	}
	events, err := backend.Chat(ctx, &Agent{ID: "ag_async_question_smoke", Tool: ToolCodex, Model: "gpt-6-astra"},
		prompt, "This is a question UI smoke test. Do not access files, web, MCP or external services. Only ask the requested asynchronous question, wait for the answer, and mark the goal complete if active.", opts)
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
	delivered := make(chan error, 1)
	got := false
	complete := false
	for ev := range events {
		if ev.Goal != nil && ev.Goal.Status == "complete" {
			complete = true
		}
		if ev.Type == "user_question" {
			if asked {
				t.Fatal("duplicate question")
			}
			asked = true
			if ev.QuestionBlocking == nil || *ev.QuestionBlocking {
				t.Fatal("not async")
			}
			go func() { delivered <- answer(ev.RequestID, map[string]any{"question_0": "Blue"}, false, "") }()
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
