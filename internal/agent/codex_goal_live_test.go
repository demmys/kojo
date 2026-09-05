package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in smoke against the installed authenticated CLI. It creates only a
// disposable native thread and never edits a user's repository.
func TestCodexGoalLive(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_GOAL") != "1" {
		t.Skip("set KOJO_TEST_CODEX_GOAL=1 for authenticated native goal smoke")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &Agent{ID: "ag_native_goal_smoke", Tool: ToolCodex}
	b := NewCodexBackend(slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	events, err := b.Chat(ctx, a, "Return 2.", "This is a smoke test. Do not use filesystem, web, or external tools. Answer the arithmetic request and use native goal tools to mark completion.", ChatOptions{Goal: &GoalRequest{Action: "start", Objective: "Compute 1+1, respond with 2, and mark this goal complete."}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		events, err := b.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "clear"}})
		if err == nil {
			for range events {
			}
		}
	}()
	done := false
	complete := false
	for ev := range events {
		if ev.ErrorMessage != "" {
			t.Errorf("event error: %s", ev.ErrorMessage)
		}
		if ev.Goal != nil {
			t.Logf("goal %s tokens=%d", ev.Goal.Status, ev.Goal.TokensUsed)
			complete = ev.Goal.Status == "complete"
		}
		if ev.Type == "done" {
			done = true
			if ev.Message == nil || !strings.Contains(ev.Message.Content, "2") {
				t.Errorf("missing final arithmetic response: %+v", ev.Message)
			}
		}
	}
	if !done || !complete {
		t.Fatalf("done=%v complete=%v ctx=%v", done, complete, ctx.Err())
	}
}

func TestCodexGoalLivePauseResume(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_GOAL") != "1" {
		t.Skip("authenticated native smoke")
	}
	home, _ := os.UserHomeDir()
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &Agent{ID: "ag_native_goal_pause_smoke", Tool: ToolCodex}
	b := NewCodexBackend(slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	events, err := b.Chat(ctx, a, "Return 2.", "Smoke test: no filesystem or web tools; compute arithmetic and mark native goal complete.", ChatOptions{Goal: &GoalRequest{Action: "start", Objective: "Compute 1+1, respond 2, and mark the goal complete."}})
	if err != nil {
		t.Fatal(err)
	}
	paused := make(chan error, 1)
	requested := false
	for ev := range events {
		if ev.Goal != nil && ev.Goal.Status == "active" && !requested {
			requested = true
			go func() {
				reply, err := b.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "pause"}})
				if err == nil {
					for e := range reply {
						if e.ErrorMessage != "" {
							err = errors.New(e.ErrorMessage)
						}
					}
				}
				paused <- err
			}()
		}
	}
	if !requested {
		t.Fatal("no active state")
	}
	if err = <-paused; err != nil {
		t.Fatal(err)
	}
	binding, err := goalBindingFor(a.ID, "")
	if err != nil || binding == nil || !binding.DesiredPaused {
		t.Fatalf("pause not persisted: %+v %v", binding, err)
	}
	// Original process is gone; resume must use its persisted native thread.
	events, err = b.Chat(ctx, a, "", "Smoke test; no external tools.", ChatOptions{Goal: &GoalRequest{Action: "resume"}})
	if err != nil {
		t.Fatal(err)
	}
	complete := false
	for ev := range events {
		if ev.ErrorMessage != "" {
			t.Error(ev.ErrorMessage)
		}
		if ev.Goal != nil {
			complete = ev.Goal.Status == "complete"
		}
	}
	if !complete {
		t.Fatal("resume did not complete")
	}
	events, err = b.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "clear"}})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func TestCodexGoalLiveCancellationReasons(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_GOAL") != "1" {
		t.Skip("authenticated native smoke")
	}
	home, _ := os.UserHomeDir()
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	for _, tc := range []struct {
		name                   string
		remote, shutdown, stop bool
	}{
		{name: "user-stop", stop: true}, {name: "daemon-shutdown", shutdown: true},
		{name: "origin-disconnect", remote: true}, {name: "remote-user-stop", remote: true, stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			a := &Agent{ID: "ag_goal_cancel_smoke", Tool: ToolCodex}
			b := NewCodexBackend(slog.Default())
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			opts := ChatOptions{Goal: &GoalRequest{Action: "start", Objective: "Compute 1+1, respond 2, and mark the native goal complete."}, PreserveGoalOnCancel: func() bool { return tc.shutdown }}
			if tc.remote {
				opts.GoalRunID = "smoke-run"
				opts.OriginPeerID = "hub"
			}
			events, err := b.Chat(ctx, a, "Return 2.", "Smoke test. No filesystem, web or external tools. Arithmetic only.", opts)
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			for ev := range events {
				if ev.Goal != nil && ev.Goal.Status == "active" && !stopped {
					stopped = true
					if tc.remote && tc.stop {
						if err = (&Manager{}).FenceGoalRun(a.ID, "", opts.GoalRunID, "hub"); err != nil {
							t.Error(err)
						}
					}
					cancel()
				}
			}
			binding, err := goalBindingFor(a.ID, "")
			if !stopped || err != nil || binding == nil {
				t.Fatalf("no active checkpoint: %+v %v", binding, err)
			}
			wantPause := tc.stop
			if binding.DesiredPaused != wantPause {
				t.Errorf("pause=%v want=%v", binding.DesiredPaused, wantPause)
			}
			if !wantPause && !binding.RecoveryPending {
				t.Error("disconnect/shutdown lost recovery intent")
			}
			cleanupCtx, cleanup := context.WithTimeout(context.Background(), 20*time.Second)
			defer cleanup()
			events, err = b.Chat(cleanupCtx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "clear"}})
			if err != nil {
				t.Fatal(err)
			}
			for ev := range events {
				if ev.ErrorMessage != "" {
					t.Error(ev.ErrorMessage)
				}
			}
		})
	}
}

func TestCodexGoalLiveReplyResume(t *testing.T) {
	if os.Getenv("KOJO_TEST_CODEX_GOAL") != "1" {
		t.Skip("authenticated native smoke")
	}
	home, _ := os.UserHomeDir()
	if os.Getenv("CODEX_HOME") == "" {
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &Agent{ID: "ag_native_goal_reply_smoke", Tool: ToolCodex}
	b := NewCodexBackend(slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	events, err := b.Chat(ctx, a, "Return 2.", "Smoke test: no filesystem or web tools; compute arithmetic and mark native goal complete.", ChatOptions{Goal: &GoalRequest{Action: "start", Objective: "Compute 1+1, respond 2, and mark the goal complete."}})
	if err != nil {
		t.Fatal(err)
	}
	paused := make(chan error, 1)
	requested := false
	for ev := range events {
		if ev.Goal != nil && ev.Goal.Status == "active" && !requested {
			requested = true
			go func() {
				reply, err := b.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "pause"}})
				if err == nil {
					for e := range reply {
						if e.ErrorMessage != "" {
							err = errors.New(e.ErrorMessage)
						}
					}
				}
				paused <- err
			}()
		}
	}
	if !requested {
		t.Fatal("no active state")
	}
	if err = <-paused; err != nil {
		t.Fatal(err)
	}
	binding, err := goalBindingFor(a.ID, "")
	if err != nil || binding == nil || !binding.DesiredPaused {
		t.Fatalf("pause not persisted: %+v %v", binding, err)
	}
	// Original process is gone; resume must use its persisted native thread.
	events, err = b.Chat(ctx, a, "The answer has changed: respond with REPLY_DELIVERED_739 instead of 2, then mark the goal complete.", "Smoke test; no external tools.", ChatOptions{ResumeGoalOnReply: true})
	if err != nil {
		t.Fatal(err)
	}
	complete := false
	receivedReply := false
	for ev := range events {
		t.Logf("event type=%s goal=%+v message=%+v delta=%s error=%s", ev.Type, ev.Goal, ev.Message, ev.Delta, ev.ErrorMessage)
		if ev.ErrorMessage != "" {
			t.Error(ev.ErrorMessage)
		}
		if ev.Message != nil && strings.Contains(ev.Message.Content+ev.Message.Thinking, "REPLY_DELIVERED_739") {
			receivedReply = true
		}
		if ev.Goal != nil {
			complete = ev.Goal.Status == "complete"
		}
	}
	if !complete || !receivedReply {
		t.Fatalf("resume complete=%v receivedReply=%v", complete, receivedReply)
	}
	events, err = b.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "clear"}})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}
