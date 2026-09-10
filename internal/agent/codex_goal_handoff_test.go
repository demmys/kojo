package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const handoffTestID = "019e7cc9-dd5e-7971-b654-7840c683879e"
const handoffTestOp = "019e7cc9-dd5e-7971-b654-7840c683879f"

func handoffFixture(t *testing.T) (string, string, *codexGoalRuntime, *Manager) {
	t.Helper()
	id, root := setupCodexTransferTest(t)
	binding := &GoalBinding{State: &CodexGoal{ThreadID: handoffTestID, Status: "active", Objective: "test"}, Generation: 4}
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: handoffTestID, Goal: binding}, slog.Default())
	r := &codexGoalRuntime{agentID: id, threadID: handoffTestID, isGoal: true, ready: true, inNativeTurn: true, turnSequence: 1, pending: map[int64]chan *rpcMessage{}}
	codexGoalRuntimes.Store(codexThreadRefPath(id, ""), r)
	t.Cleanup(func() { codexGoalRuntimes.Delete(codexThreadRefPath(id, "")); r.close() })
	return id, root, r, &Manager{}
}
func seedHandoffDB(t *testing.T, root, status string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "goals_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(testGoalDDL); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO thread_goals VALUES (?,?,?,?,?,?,?,?,?)", handoffTestID, "goal-1", "test", status, 10000, 450, 7, 1, 2); err != nil {
		t.Fatal(err)
	}
}
func TestGoalHandoffRequiresCleanNativeCheckpoint(t *testing.T) {
	for _, tc := range []string{"clean", "kill", "uncheckpointed", "stop", "missing-db", "active-db"} {
		t.Run(tc, func(t *testing.T) {
			id, root, r, m := handoffFixture(t)
			done, err := m.QueueGoalHandoff(id, "", GoalHandoff{ID: handoffTestOp, SourcePeerID: "source", TargetPeerID: "target"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.QueueGoalHandoff(id, "", GoalHandoff{ID: "duplicate"}); err == nil {
				t.Fatal("duplicate queue")
			}
			if err := checkGoalHandoffAdmission(id, "", nil); err == nil {
				t.Fatal("ordinary reply accepted")
			}
			if err := checkGoalHandoffAdmission(id, "", &GoalRequest{Action: "status"}); err != nil {
				t.Fatal(err)
			}
			if tc != "missing-db" {
				status := "paused"
				if tc == "active-db" {
					status = "active"
				}
				seedHandoffDB(t, root, status)
			}
			if tc != "uncheckpointed" {
				r.checkpointHandoff()
			}
			if err := updateGoalBinding(id, "", func(b *GoalBinding) {
				b.State.Status = "paused"
				if tc == "stop" {
					cancelGoalHandoff(b, "operator stop")
					b.Generation++
				}
			}); err != nil {
				t.Fatal(err)
			}
			var exitErr error
			if tc == "kill" {
				exitErr = errors.New("killed")
			}
			r.finishHandoff(exitErr)
			err = <-done
			if (err == nil) != (tc == "clean") {
				t.Fatalf("checkpoint err=%v", err)
			}
			b, _ := goalBindingFor(id, "")
			if !b.DesiredPaused {
				t.Fatal("recovery fence lost")
			}
			if tc != "clean" {
				if _, err = m.GoalHandoffCheckpoint(id, "", handoffTestOp); err == nil {
					t.Fatal("unclean checkpoint allowed")
				}
				return
			}
			if b.State.TokensUsed != 450 || b.State.TimeUsedSeconds != 7 {
				t.Fatal("final database accounting lost")
			}
			accepted, err := m.AcceptGoalHandoff(id, "", handoffTestOp, "source", "target")
			if err != nil {
				t.Fatal(err)
			}
			gen := accepted.Generation
			q := &GoalRequest{Action: "resume", ExpectedThreadID: handoffTestID, ExpectedGeneration: &gen, ExpectedHandoffID: handoffTestOp}
			ref, _ := readCodexThreadRef(id, "")
			if !goalHandoffResumeAllowed(ref, q) {
				t.Fatal("finalized checkpoint not resumable")
			}
			if _, err = m.AcceptGoalHandoff(id, "", handoffTestOp, "source", "wrong"); err == nil {
				t.Fatal("wrong target allowed")
			}
			if err := updateGoalBinding(id, "", func(b *GoalBinding) { cancelGoalHandoff(b, "stop"); b.Generation++ }); err != nil {
				t.Fatal(err)
			}
			ref, _ = readCodexThreadRef(id, "")
			if goalHandoffResumeAllowed(ref, q) {
				t.Fatal("stop was undone")
			}
			if _, err = m.AcceptGoalHandoff(id, "", handoffTestOp, "source", "target"); err == nil {
				t.Fatal("duplicate finalize undid stop")
			}
		})
	}
}
func TestGoalHandoffStreamParksAfterTurnAndDrainsRace(t *testing.T) {
	for _, mode := range []string{"clean", "late-turn", "eager", "after-pause-ack", "blocked", "complete", "pause-error"} {
		t.Run(mode, func(t *testing.T) {
			id, _, r, m := handoffFixture(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			goal := func(status string) map[string]any {
				return map[string]any{"goal": map[string]any{"threadId": handoffTestID, "status": status, "objective": "test"}}
			}
			resp := func(n int, v any) string {
				b, _ := json.Marshal(map[string]any{"id": n, "result": v})
				return string(b)
			}
			turn := func(method string) string {
				return rpcLine(method, map[string]any{"turn": map[string]any{"id": "one", "status": "completed"}})
			}
			lines := []string{resp(1, goal("active")), turn("turn/started"), rpcLine("item/agentMessage/delta", map[string]any{"delta": "queue"})}
			status := "active"
			if mode == "blocked" || mode == "complete" {
				status = mode
				lines = append(lines, rpcLine("thread/goal/updated", goal(status)))
			}
			lines = append(lines, turn("turn/completed"))
			if mode == "eager" {
				lines = append(lines, turn("turn/started"))
			}
			lines = append(lines, resp(2, goal(status)))
			if mode == "clean" {
				lines = append(lines, resp(3, goal("paused")), resp(4, goal("paused")))
			}
			if mode == "late-turn" {
				lines = append(lines, turn("turn/started"))
			}
			if mode == "late-turn" || mode == "eager" {
				lines = append(lines, resp(3, goal("paused")), rpcLine("turn/completed", map[string]any{"turn": map[string]any{"id": "one", "status": "interrupted"}}), resp(5, goal("paused")))
			}
			if mode == "after-pause-ack" {
				lines = append(lines, resp(3, goal("paused")), turn("turn/started"), resp(4, goal("paused")), rpcLine("turn/completed", map[string]any{"turn": map[string]any{"id": "one", "status": "interrupted"}}), resp(6, goal("paused")))
			}
			if mode == "pause-error" {
				lines = append(lines, `{"id":3,"error":{"code":-1,"message":"failed"}}`)
			}
			var methods []string
			r.write = func(method string, _ any) (int64, error) {
				methods = append(methods, method)
				return int64(len(methods)), nil
			}
			queued := false
			result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "resume"}, r, nil, nil, logger, func(evt ChatEvent) bool {
				if evt.Type == "text" && strings.Contains(evt.Delta, "queue") && !queued {
					queued = true
					if _, err := m.QueueGoalHandoff(id, "", GoalHandoff{ID: handoffTestOp, SourcePeerID: "source", TargetPeerID: "target"}); err != nil {
						t.Fatal(err)
					}
					if len(methods) != 1 {
						t.Fatalf("queue interrupted current tool/turn: %v", methods)
					}
				}
				return true
			})
			if !queued {
				t.Fatal("fixture did not queue")
			}
			if mode == "clean" || mode == "late-turn" || mode == "eager" || mode == "after-pause-ack" {
				if result.processError != "" || r.handoff == nil || !r.handoff.checkpointed {
					t.Fatalf("result=%+v methods=%v", result, methods)
				}
			} else {
				if r.handoff != nil && r.handoff.checkpointed {
					t.Fatal("invalid checkpoint")
				}
				if mode != "pause-error" {
					for _, v := range methods[1:] {
						if v == "thread/goal/set" {
							t.Fatalf("reactivated %s: %v", mode, methods)
						}
					}
				}
			}
		})
	}
}
func TestParseHandoffResumeIdentity(t *testing.T) {
	q, err := ParseGoalCommand(fmt.Sprintf("!goal resume-if %s 5 - %s", handoffTestID, handoffTestOp))
	if err != nil || q.ExpectedHandoffID != handoffTestOp || q.ExpectedRunID != "" {
		t.Fatalf("q=%+v err=%v", q, err)
	}
	if _, err := ParseGoalCommand(fmt.Sprintf("!goal resume-if %s 5 - invalid", handoffTestID)); err == nil {
		t.Fatal("invalid operation accepted")
	}
}

func TestGoalHandoffRealRPCProcessExitsBeforeReadyAndResumesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture launcher")
	}
	id, root := setupCodexTransferTest(t)
	seedHandoffDB(t, root, "paused")
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	calls := filepath.Join(dir, "calls")
	if err := os.WriteFile(state, []byte("paused"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state+".handoff", nil, 0600); err != nil {
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
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: handoffTestID, Goal: &GoalBinding{DesiredPaused: true, State: &CodexGoal{ThreadID: handoffTestID, Status: "paused", Objective: "test"}}}, slog.Default())
	backend := NewCodexBackend(slog.Default())
	a := &Agent{ID: id, Tool: ToolCodex}
	m := &Manager{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	events, err := backend.Chat(ctx, a, "", "", ChatOptions{Goal: &GoalRequest{Action: "resume"}})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint <-chan error
	for e := range events {
		if e.Type == "text" && checkpoint == nil {
			checkpoint, err = m.QueueGoalHandoff(id, "", GoalHandoff{ID: handoffTestOp, SourcePeerID: "source", TargetPeerID: "target"})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(state+".queued", nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if e.ErrorMessage != "" {
			t.Fatal(e.ErrorMessage)
		}
	}
	if checkpoint == nil || ctx.Err() != nil {
		t.Fatalf("missing checkpoint: %v", ctx.Err())
	}
	if err = <-checkpoint; err != nil {
		t.Fatal(err)
	}
	binding, err := m.GoalHandoffCheckpoint(id, "", handoffTestOp)
	if err != nil {
		t.Fatal(err)
	}
	if binding.State.TokensUsed != 451 {
		t.Fatal("did not read final committed native DB")
	}
	// Source runtime is closed and no longer registered before any move.
	if _, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, "")); ok {
		t.Fatal("runner still registered")
	}
	if err = os.Remove(state + ".handoff"); err != nil {
		t.Fatal(err)
	}
	accepted, err := m.AcceptGoalHandoff(id, "", handoffTestOp, "source", "target")
	if err != nil {
		t.Fatal(err)
	}
	q := &GoalRequest{Action: "resume", ExpectedThreadID: handoffTestID, ExpectedGeneration: &accepted.Generation, ExpectedHandoffID: handoffTestOp}
	events, err = backend.Chat(ctx, a, "", "", ChatOptions{Goal: q})
	if err != nil {
		t.Fatal(err)
	}
	resumed := false
	for e := range events {
		if e.ErrorMessage != "" {
			t.Fatal(e.ErrorMessage)
		}
		if e.Type == "text" && strings.Contains(e.Delta, "resumed after device transfer") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("no native resume ACK notice")
	}
	// A duplicated command may not activate this now-completed binding.
	events, err = backend.Chat(ctx, a, "", "", ChatOptions{Goal: q})
	if err == nil {
		rejected := false
		for e := range events {
			rejected = rejected || e.ErrorMessage != ""
		}
		if !rejected {
			t.Fatal("duplicate resume admitted")
		}
	}
}

func TestGoalHandoffIgnoresPreviousTurnsDelayedGet(t *testing.T) {
	id, _, r, m := handoffFixture(t)
	goal := func(status string) any {
		return map[string]any{"goal": map[string]any{"threadId": handoffTestID, "status": status, "objective": "test"}}
	}
	resp := func(n int, status string) string {
		b, _ := json.Marshal(map[string]any{"id": n, "result": goal(status)})
		return string(b)
	}
	turn := func(method, tid string) string {
		return rpcLine(method, map[string]any{"turn": map[string]any{"id": tid, "status": "completed"}})
	}
	lines := []string{
		resp(1, "active"), turn("turn/started", "A"), turn("turn/completed", "A"),
		turn("turn/started", "B"), rpcLine("item/agentMessage/delta", map[string]any{"delta": "queue"}),
		resp(2, "active"), rpcLine("item/agentMessage/delta", map[string]any{"delta": "still B"}),
		turn("turn/completed", "B"), resp(3, "active"), resp(4, "paused"), resp(5, "paused"),
	}
	var methods []string
	r.write = func(method string, _ any) (int64, error) {
		methods = append(methods, method)
		return int64(len(methods)), nil
	}
	queued := false
	result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "resume"}, r, nil, nil, slog.Default(), func(e ChatEvent) bool {
		if e.Type == "text" && strings.Contains(e.Delta, "queue") && !queued {
			queued = true
			if _, err := m.QueueGoalHandoff(id, "", GoalHandoff{ID: handoffTestOp, SourcePeerID: "source", TargetPeerID: "target"}); err != nil {
				t.Fatal(err)
			}
		}
		if e.Type == "text" && strings.Contains(e.Delta, "still B") && len(methods) != 2 {
			t.Fatalf("previous turn response paused requesting turn: %v", methods)
		}
		return true
	})
	if result.processError != "" || r.handoff == nil || !r.handoff.checkpointed {
		t.Fatalf("result=%+v calls=%v", result, methods)
	}
}
func TestOldHandoffStopCannotCancelReplacementRuntime(t *testing.T) {
	id, _, r, m := handoffFixture(t)
	// New explicit goal has registered its runtime but not finished startup.
	r.mu.Lock()
	r.ready = false
	r.inNativeTurn = false
	r.mu.Unlock()
	if err := updateGoalBinding(id, "", func(b *GoalBinding) {
		b.Handoff = &GoalHandoff{ID: handoffTestOp, SourcePeerID: "old", TargetPeerID: "here", Phase: "resumed"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelGoalHandoff(id, "", handoffTestOp); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	stopped := r.stopRequested
	r.mu.Unlock()
	if stopped {
		t.Fatal("old operation cancelled replacement startup")
	}
}

// Pin the admission mutex and observe the contender blocked on it, then
// install the reservation before releasing it. This reproduces check-before-
// lock ordering without depending on arbitrary sleeps to win the race.
func TestGoalSteerRechecksReservationInsideAdmission(t *testing.T) {
	for _, surface := range []string{"slack", "main"} {
		t.Run(surface, func(t *testing.T) {
			m := newTestManager(t)
			id := "ag_steer_handoff"
			m.agents[id] = &Agent{ID: id, Tool: ToolCodex}
			key := ""
			if surface == "slack" {
				key = id + ":slack:C:T"
			}
			writeCodexThreadRef(id, key, codexThreadRef{ThreadID: handoffTestID, Goal: &GoalBinding{SessionKey: key, UserID: "UOWNER", State: &CodexGoal{ThreadID: handoffTestID, Status: "active"}}}, slog.Default())
			var invoked atomic.Int32
			fn := func(string) error { invoked.Add(1); return nil }
			if surface == "slack" {
				m.oneShotCancels = make(map[string]map[int64]context.CancelFunc)
				m.oneShotSessions = make(map[string]map[int64]string)
				m.oneShotArmed = make(map[string]map[int64]time.Time)
				run := m.trackOneShot(id, func() {}, key, "hub", "")
				defer m.untrackOneShot(id, run)
				m.oneShotSteers = map[string]SteerFunc{key: fn}
			} else {
				m.busy[id] = busyEntry{source: BusySourceUser, steer: fn, cancel: func() {}, outCh: make(chan ChatEvent, 4)}
				defer delete(m.busy, id)
			}
			release := sync.OnceFunc(goalAdmissions.Lock(codexThreadRefPath(id, key)))
			defer release()
			done := make(chan error, 1)
			go func() {
				if surface == "slack" {
					done <- m.SteerOneShotAsUser(id, key, "hub", "reply", "UOWNER")
				} else {
					_, err := m.Steer(context.Background(), id, "reply")
					done <- err
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			waiting := false
			for time.Now().Before(deadline) {
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
					if strings.Contains(stack, "keyedMutex).Lock") && (strings.Contains(stack, "SteerOneShotAsUser") || strings.Contains(stack, "Manager).Steer")) {
						waiting = true
						break
					}
				}
				if waiting {
					break
				}
				runtime.Gosched()
			}
			if !waiting {
				t.Fatal("steer never waited for admission")
			}
			if err := updateGoalBinding(id, key, func(b *GoalBinding) {
				b.Handoff = &GoalHandoff{ID: handoffTestOp, Phase: "queued"}
				b.DesiredPaused = true
			}); err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "handoff pending") {
					t.Fatalf("err=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("steer blocked")
			}
			if invoked.Load() != 0 {
				t.Fatal("steer invoked after reservation committed")
			}
		})
	}
}
