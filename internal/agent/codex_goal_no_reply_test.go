package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestNativeGoalNoReplyPerTurn(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		turns     []string
		want      string
	}{
		{"first middle last", "ag_codex_transfer:slack:C123:123.456", []string{SlackNoReplyToken, "first", SlackNoReplyToken, SlackNoReplyToken, "last", SlackNoReplyToken}, "first\n\nlast"},
		{"all silent", "ag_codex_transfer:slack:C123:123.456", []string{SlackNoReplyToken, SlackNoReplyToken}, ""},
		{"mentions preserved", "ag_codex_transfer:slack:C123:123.456", []string{"Use " + SlackNoReplyToken, SlackNoReplyToken, SlackNoReplyToken + " is a token"}, "Use " + SlackNoReplyToken + "\n\n" + SlackNoReplyToken + " is a token"},
		{"web unchanged", "thread:web", []string{"first", SlackNoReplyToken, "last"}, "first\n\n" + SlackNoReplyToken + "\n\nlast"},
	} {
		for _, completedOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/completedOnly=%v", tc.name, completedOnly), func(t *testing.T) {
				id, _ := setupCodexTransferTest(t)
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				writeCodexThreadRef(id, tc.key, codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e"}, logger)
				response := func(n int, status string) string {
					b, _ := json.Marshal(map[string]any{"id": n, "result": map[string]any{"goal": map[string]any{"status": status, "objective": "test", "tokensUsed": 42}}})
					return string(b)
				}
				lines := []string{response(1, "active")}
				for n, body := range tc.turns {
					lines = append(lines, rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": fmt.Sprint(n)}}))
					if !completedOnly {
						for _, c := range body {
							lines = append(lines, rpcLine("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": string(c)}))
						}
					}
					lines = append(lines, rpcLine("item/completed", map[string]any{"item": map[string]any{"id": "answer", "type": "agentMessage", "text": body}}))
					lines = append(lines, rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}))
					status := "active"
					if n == len(tc.turns)-1 {
						status = "blocked"
					}
					lines = append(lines, response(n+2, status))
				}
				var calls int64
				r := &codexGoalRuntime{agentID: id, key: tc.key, threadID: "019e7cc9-dd5e-7971-b654-7840c683879e", pending: map[int64]chan *rpcMessage{}, write: func(string, any) (int64, error) { calls++; return calls, nil }}
				var live strings.Builder
				result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "start", Objective: "test"}, r, nil, nil, logger, func(e ChatEvent) bool {
					if e.Type == "text" {
						live.WriteString(e.Delta)
					}
					return true
				})
				if live.String() != tc.want {
					t.Fatalf("live=%q want=%q", live.String(), tc.want)
				}
				summary := goalSummary(&CodexGoal{Status: "blocked", Objective: "test", TokensUsed: 42})
				if result.fullText.String() != tc.want+"\n\n"+summary {
					t.Fatalf("terminal=%q", result.fullText.String())
				}
				if result.processError != "" || !result.turnCompleted || result.cancelled {
					t.Fatalf("result=%+v", result)
				}
				binding, err := goalBindingFor(id, tc.key)
				if err != nil || binding.State.Status != "blocked" {
					t.Fatalf("binding=%+v err=%v", binding, err)
				}
			})
		}
	}
}

func TestNativeGoalNoReplyFailure(t *testing.T) {
	for _, ending := range []string{"eof", "failed", "interrupted"} {
		t.Run(ending, func(t *testing.T) {
			id, _ := setupCodexTransferTest(t)
			key := "ag_codex_transfer:slack:C123:123.456"
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			writeCodexThreadRef(id, key, codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e"}, logger)
			lines := []string{`{"id":1,"result":{"goal":{"status":"active"}}}`,
				rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "one"}}),
				rpcLine("item/agentMessage/delta", map[string]any{"delta": "[[NO_"}),
			}
			if ending != "eof" {
				lines = append(lines, rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": ending, "error": map[string]any{"message": "specific failure"}}}))
			}
			var calls int64
			r := &codexGoalRuntime{agentID: id, key: key, threadID: "019e7cc9-dd5e-7971-b654-7840c683879e", pending: map[int64]chan *rpcMessage{}, write: func(string, any) (int64, error) { calls++; return calls, nil }}
			var live strings.Builder
			result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "start"}, r, nil, nil, logger, func(e ChatEvent) bool {
				if e.Type == "text" {
					live.WriteString(e.Delta)
				}
				return true
			})
			if live.Len() != 0 || strings.Contains(result.fullText.String(), "[[NO_") || result.processError == "" {
				t.Fatalf("live=%q result=%+v terminal=%q", live.String(), result, result.fullText.String())
			}
		})
	}
}
