package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Deterministic app-server peer; no API account, native installation or network.
func TestGoalRPCProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "goal-rpc-fixture" {
		return
	}
	statePath, logPath := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
	emit := func(method string, params any) { _ = enc.Encode(map[string]any{"method": method, "params": params}) }
	goal := func(status string) any {
		return map[string]any{"goal": map[string]any{"threadId": tid, "status": status, "objective": "test", "tokensUsed": 42, "timeUsedSeconds": 5}}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &q) != nil {
			os.Exit(3)
		}
		_, _ = f.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
		if len(q.ID) == 0 {
			continue
		}
		data, _ := os.ReadFile(statePath)
		status := string(data)
		var result any = map[string]any{}
		switch q.Method {
		case "thread/goal/get":
			result = goal(status)
		case "thread/goal/set":
			if next, ok := q.Params["status"].(string); ok {
				status = next
				_ = os.WriteFile(statePath, []byte(status), 0600)
			}
			result = goal(status)
		case "thread/resume", "thread/start":
			result = map[string]any{"thread": map[string]any{"id": tid}}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "turn"}}
		}
		_ = enc.Encode(map[string]any{"id": q.ID, "result": result})
		if q.Method == "turn/start" || (q.Method == "thread/goal/set" && status == "active") {
			emit("turn/started", map[string]any{"turn": map[string]any{"id": "turn"}})
			emit("item/agentMessage/delta", map[string]any{"delta": "ordinary reply"})
			if status == "active" {
				_ = os.WriteFile(statePath, []byte("complete"), 0600)
				emit("thread/goal/updated", goal("complete"))
			}
			emit("turn/completed", map[string]any{"turn": map[string]any{"id": "turn", "status": "completed"}})
		}
	}
	_ = f.Close()
	os.Exit(0)
}

func TestGoalPauseThenOrdinaryReplyDoesNotReactivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture launcher uses POSIX shell")
	}
	for _, initial := range []string{"pause-command", "stop-stale-active", "native-paused", "blocked"} {
		t.Run(initial, func(t *testing.T) {
			id, _ := setupCodexTransferTest(t)
			key := id + ":slack:C:T"
			tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			calls := filepath.Join(dir, "calls")
			status := "active"
			if initial == "native-paused" {
				status = "paused"
			}
			if initial == "blocked" {
				status = "blocked"
			}
			if err := os.WriteFile(state, []byte(status), 0600); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestGoalRPCProcess$ -- goal-rpc-fixture %q %q\n", exe, state, calls)
			if err = os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			writeCodexThreadRef(id, key, codexThreadRef{ThreadID: tid, Goal: &GoalBinding{UserID: "UOWNER", DesiredPaused: initial == "stop-stale-active", State: &CodexGoal{ThreadID: tid, Status: status}}}, slog.Default())
			b := NewCodexBackend(slog.Default())
			a := &Agent{ID: id, Tool: ToolCodex}
			run := func(q *GoalRequest, reply bool) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ev, err := b.Chat(ctx, a, "move to Hub", "", ChatOptions{SessionKey: key, GoalUserID: "UOWNER", Goal: q, ResumeGoalOnReply: reply})
				if err != nil {
					t.Fatal(err)
				}
				done := false
				for e := range ev {
					if e.ErrorMessage != "" {
						t.Fatalf("backend error: %s", e.ErrorMessage)
					}
					if e.Type == "status" && q == nil && initial != "blocked" && NativeGoalRunning(id, key) {
						t.Fatal("ordinary paused reply hit self-handoff guard")
					}
					done = done || e.Type == "done"
				}
				if !done || ctx.Err() != nil {
					t.Fatalf("no completion: %v", ctx.Err())
				}
			}
			if initial == "pause-command" {
				run(&GoalRequest{Action: "pause"}, false)
			}
			run(nil, true)
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			activated := strings.Contains(string(data), `"status":"active"`)
			if activated != (initial == "blocked") {
				t.Fatalf("unexpected activation (%s):\n%s", initial, data)
			}
			if initial != "blocked" {
				run(&GoalRequest{Action: "resume"}, false)
				data, _ = os.ReadFile(calls)
				if !strings.Contains(string(data), `"status":"active"`) {
					t.Fatal("explicit resume did not activate")
				}
			}
		})
	}
}
