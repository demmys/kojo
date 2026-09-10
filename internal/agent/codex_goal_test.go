package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestParseGoalCommand(t *testing.T) {
	for _, tc := range []struct {
		text, action string
		bad          bool
	}{
		{"!goal fix it", "start", false}, {"!goal\n直して", "start", false}, {"!goal status", "status", false}, {"!goal pause", "pause", false}, {"!goal resume", "resume", false}, {"!goal clear", "clear", false}, {"!goal budget 20000", "budget", false}, {"!goal budget -1", "", true}, {"!goal budget 2 garbage", "", true}, {"!goal " + strings.Repeat("a", 4001), "", true}, {"please !goal fix it", "", false}, {"!goals fix it", "", false}, {"quoted\n!goal fix it", "", false},
	} {
		t.Run(tc.text[:min(len(tc.text), 40)], func(t *testing.T) {
			q, e := ParseGoalCommand(tc.text)
			if (e != nil) != tc.bad {
				t.Fatalf("err=%v", e)
			}
			if tc.bad {
				return
			}
			if tc.action == "" {
				if q != nil {
					t.Fatal(q)
				}
			} else if q == nil || q.Action != tc.action {
				t.Fatalf("got %+v", q)
			}
		})
	}
}

func TestNativeGoalStreamOwnsContinuationAndDrainsFinal(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: tid}, logger)
	goal := func(status string) map[string]any {
		return map[string]any{"goal": map[string]any{"threadId": tid, "status": status, "objective": "fix it", "tokensUsed": 33}}
	}
	resp := func(id int, v any) string {
		b, _ := json.Marshal(map[string]any{"id": id, "result": v})
		return string(b)
	}
	lines := []string{
		resp(1, goal("active")),
		rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "one"}}),
		rpcLine("item/agentMessage/delta", map[string]any{"delta": "first"}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
		resp(2, goal("active")),
		rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "two"}}),
		rpcLine("thread/goal/updated", goal("complete")),
		rpcLine("item/agentMessage/delta", map[string]any{"delta": "final"}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
		resp(3, goal("complete")),
	}
	var methods []string
	r := &codexGoalRuntime{agentID: id, threadID: tid, pending: map[int64]chan *rpcMessage{}, write: func(method string, _ any) (int64, error) {
		methods = append(methods, method)
		return int64(len(methods)), nil
	}}
	result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "start", Objective: "fix it"}, r, nil, nil, logger, func(ChatEvent) bool { return true })
	if result.processError != "" || !result.turnCompleted || !strings.Contains(result.fullText.String(), "final") {
		t.Fatalf("result: %+v %s", result, result.fullText.String())
	}
	for _, m := range methods {
		if m == "turn/start" {
			t.Fatal("host must not double-start native goal")
		}
	}
	b, err := goalBindingFor(id, "")
	if err != nil || b.State.Status != "complete" {
		t.Fatalf("binding=%+v err=%v", b, err)
	}
}

func TestGoalBindingKeepsPauseIntentOnLateNativeUpdate(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	writeCodexThreadRef(id, "key", codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e"}, slog.Default())
	if err := updateGoalBinding(id, "key", func(b *GoalBinding) { b.DesiredPaused = true }); err != nil {
		t.Fatal(err)
	}
	if err := updateGoalBinding(id, "key", func(b *GoalBinding) { b.State = &CodexGoal{Status: "active"} }); err != nil {
		t.Fatal(err)
	}
	b, _ := goalBindingFor(id, "key")
	if !b.DesiredPaused {
		t.Fatal("late update revived stopped goal")
	}
}

func TestGoalReplyAcceptedBeforeActivation(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			id, _ := setupCodexTransferTest(t)
			tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			writeCodexThreadRef(id, "", codexThreadRef{ThreadID: tid}, logger)
			lines := []string{`{"id":1,"result":{}}`, rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "reply"}}), `{"id":2,"result":{"goal":{"status":"active"}}}`, rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}), `{"id":3,"result":{"goal":{"status":"complete"}}}`}
			if reject {
				lines = []string{`{"id":1,"error":{"code":-1,"message":"rejected"}}`}
			}
			var methods []string
			r := &codexGoalRuntime{agentID: id, threadID: tid, pending: map[int64]chan *rpcMessage{}, write: func(method string, _ any) (int64, error) {
				methods = append(methods, method)
				return int64(len(methods)), nil
			}}
			result := runCodexGoalWithReply(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "resume"}, r, nil, nil, logger, func(ChatEvent) bool { return true }, func() (int64, error) { return r.write("turn/start", nil) })
			if reject {
				binding, err := goalBindingFor(id, "")
				if err != nil || binding == nil || !binding.DesiredPaused {
					t.Fatalf("rejected reply can recover: %+v %v", binding, err)
				}
				if len(methods) != 1 || !strings.Contains(result.processError, "rejected") {
					t.Fatalf("methods=%v error=%s", methods, result.processError)
				}
				return
			}
			if !result.turnCompleted || result.processError != "" || strings.Join(methods, ",") != "turn/start,thread/goal/set,thread/goal/get" {
				t.Fatalf("methods=%v result=%+v", methods, result)
			}
		})
	}
}

func TestGoalReplyActivationFailureRemainsPaused(t *testing.T) {
	for _, response := range []string{`{"id":2,"error":{"code":-1,"message":"activation rejected"}}`, "", "stop"} {
		t.Run(response, func(t *testing.T) {
			id, _ := setupCodexTransferTest(t)
			tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			writeCodexThreadRef(id, "", codexThreadRef{ThreadID: tid, Goal: &GoalBinding{State: &CodexGoal{Status: "active"}, RecoveryPending: true}}, logger)
			var calls int64
			r := &codexGoalRuntime{agentID: id, threadID: tid, pending: map[int64]chan *rpcMessage{}, write: func(string, any) (int64, error) { calls++; return calls, nil }}
			lines := `{"id":1,"result":{}}` + "\n" + response
			result := runCodexGoalWithReply(newCodexLineScanner(strings.NewReader(lines)), &GoalRequest{Action: "resume"}, r, nil, nil, logger, func(ChatEvent) bool { return true }, func() (int64, error) {
				if response == "stop" {
					r.stopRequested = true
				}
				return r.write("turn/start", nil)
			})
			binding, err := goalBindingFor(id, "")
			if result.processError == "" || err != nil || binding == nil || !binding.DesiredPaused || binding.RecoveryPending || binding.ActivationPending {
				t.Fatalf("result=%+v binding=%+v err=%v", result, binding, err)
			}
		})
	}
}
