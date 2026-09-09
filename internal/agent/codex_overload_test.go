package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func overloadFailure(info any) string {
	return rpcLine("turn/completed", map[string]any{
		"turn": map[string]any{"status": "failed", "error": map[string]any{
			"message": "Selected model is at capacity. Please try a different model.", "codexErrorInfo": info,
		}},
	})
}

func overloadSuccess() []string {
	return []string{
		rpcLine("item/agentMessage/delta", map[string]any{"itemId": "final", "delta": "check-in complete"}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
	}
}

func runOverloadTest(ctx context.Context, lines []string, policy codexTurnRetryPolicy) (*codexStreamResult, []string) {
	var starts []string
	result := runCodexTurns(ctx, newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n")+"\n")), "check-in",
		policy, func(input string) (int64, error) { starts = append(starts, input); return int64(len(starts)), nil },
		nil, nil, testLogger(), func(ChatEvent) bool { return true })
	return result, starts
}

func TestCodexRetryPolicy(t *testing.T) {
	for _, opts := range []ChatOptions{{}, {AutomatedTrigger: true}, {OneShot: true}} {
		if p := codexRetryPolicy(opts); len(p.overloadDelays) != 0 || p.maxEmptyRetries != codexEmptyCompletionMaxRetries {
			t.Fatalf("ordinary turn policy = %+v", p)
		}
	}
	p := codexRetryPolicy(ChatOptions{RetryOverload: true})
	if len(p.overloadDelays) != 2 || p.overloadDelays[0] != 10*time.Second || p.overloadDelays[1] != 30*time.Second {
		t.Fatalf("check-in policy = %+v", p)
	}
}

func TestCodexOverloadRecovery(t *testing.T) {
	// A userMessage item is the input, not proof of model work.
	lines := []string{
		rpcLine("item/started", map[string]any{"item": map[string]any{"type": "userMessage"}}),
		overloadFailure("serverOverloaded"), overloadFailure("serverOverloaded"),
	}
	lines = append(lines, overloadSuccess()...)
	result, starts := runOverloadTest(context.Background(), lines, codexTurnRetryPolicy{overloadDelays: []time.Duration{0, 0}})
	if len(starts) != 3 || starts[0] != "check-in" || starts[1] != codexOverloadRetryPrompt || starts[2] != codexOverloadRetryPrompt {
		t.Fatalf("starts = %q", starts)
	}
	if result.processError != "" || result.processErrorCode != "" || result.cancelled || result.fullText.String() != "check-in complete" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCodexOverloadExhaustion(t *testing.T) {
	lines := []string{overloadFailure("serverOverloaded"), overloadFailure("serverOverloaded"), overloadFailure("serverOverloaded")}
	result, starts := runOverloadTest(context.Background(), lines, codexTurnRetryPolicy{overloadDelays: []time.Duration{0, 0}})
	if len(starts) != 3 || result.processErrorCode != "serverOverloaded" || !strings.Contains(result.processError, "at capacity") {
		t.Fatalf("starts=%q result=%+v", starts, result)
	}
}

func TestCodexOverloadFailsClosed(t *testing.T) {
	for _, info := range []any{nil, "usageLimitExceeded", "rateLimitExceeded", "unauthorized", "internalServerError",
		"server_overloaded", map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": 503}},
		map[string]any{"serverOverloaded": nil}} {
		t.Run(stringMustJSON(info), func(t *testing.T) {
			result, starts := runOverloadTest(context.Background(), []string{overloadFailure(info)}, codexTurnRetryPolicy{overloadDelays: []time.Duration{0, 0}})
			if len(starts) != 1 || result.processError == "" {
				t.Fatalf("starts=%q result=%+v", starts, result)
			}
		})
	}
	_, starts := runOverloadTest(context.Background(), []string{overloadFailure("serverOverloaded")}, codexTurnRetryPolicy{})
	if len(starts) != 1 {
		t.Fatalf("non-check-in retried: %q", starts)
	}
}

func stringMustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCodexOverloadNeverReplaysActivity(t *testing.T) {
	for name, activity := range map[string]string{
		"malformed event":   "{broken-json",
		"shell started":     rpcLine("item/started", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "shell"}}),
		"file change":       rpcLine("item/started", map[string]any{"item": map[string]any{"type": "fileChange"}}),
		"unknown tool":      rpcLine("item/completed", map[string]any{"item": map[string]any{"type": "futureTool"}}),
		"missing item type": rpcLine("item/started", map[string]any{}),
		"reasoning":         rpcLine("item/reasoning/textDelta", map[string]any{"delta": "thinking"}),
		"text":              rpcLine("item/agentMessage/delta", map[string]any{"delta": "partial"}),
		"other delta":       rpcLine("item/futureTool/progress", map[string]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			result, starts := runOverloadTest(context.Background(), []string{activity, overloadFailure("serverOverloaded")}, codexTurnRetryPolicy{overloadDelays: []time.Duration{0, 0}})
			if len(starts) != 1 || !result.activity || result.processErrorCode != "serverOverloaded" {
				t.Fatalf("starts=%q result=%+v", starts, result)
			}
		})
	}
}

func TestCodexOverloadAfterEmptyContinuationDoesNotReplayEarlierWork(t *testing.T) {
	lines := []string{
		rpcLine("item/started", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "cmd"}}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
		overloadFailure("serverOverloaded"),
	}
	result, starts := runOverloadTest(context.Background(), lines, codexTurnRetryPolicy{maxEmptyRetries: 2, overloadDelays: []time.Duration{0, 0}})
	if len(starts) != 2 || starts[1] != codexEmptyCompletionRetryPrompt || result.processErrorCode != "serverOverloaded" || len(result.toolUses) != 1 {
		t.Fatalf("starts=%q result=%+v", starts, result)
	}
}

func TestCodexOverloadDoesNotConsumeEmptyRetryBudget(t *testing.T) {
	lines := []string{overloadFailure("serverOverloaded"), rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})}
	lines = append(lines, overloadSuccess()...)
	result, starts := runOverloadTest(context.Background(), lines, codexTurnRetryPolicy{maxEmptyRetries: 1, overloadDelays: []time.Duration{0}})
	if len(starts) != 3 || starts[2] != codexEmptyCompletionRetryPrompt || result.processError != "" {
		t.Fatalf("starts=%q result=%+v", starts, result)
	}
}

func TestCodexOverloadDeadlineDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, starts := runOverloadTest(ctx, []string{overloadFailure("serverOverloaded")}, codexTurnRetryPolicy{overloadDelays: []time.Duration{time.Hour}})
	if len(starts) != 1 || !result.cancelled || result.turnCompleted || ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("starts=%q result=%+v err=%v", starts, result, ctx.Err())
	}
}

func TestWaitCodexRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- waitCodexRetry(ctx, time.Hour) }()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("cancelled wait succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("backoff did not stop on cancellation")
	}
}

func TestCodexTurnErrorObjectPreservesMessage(t *testing.T) {
	result, _ := runOverloadTest(context.Background(), []string{overloadFailure(map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 502}})}, codexTurnRetryPolicy{})
	if result.processErrorCode != "responseStreamDisconnected" || result.processError == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCodexOverloadServerRequestPreventsRetry(t *testing.T) {
	lines := []string{
		`{"jsonrpc":"2.0","id":"call-1","method":"item/tool/call","params":{"tool":"request_plugin_install","arguments":{}}}`,
		overloadFailure("serverOverloaded"),
	}
	starts, calls := 0, 0
	result := runCodexTurns(context.Background(), newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n")+"\n")), "check-in",
		codexTurnRetryPolicy{overloadDelays: []time.Duration{0}},
		func(string) (int64, error) { starts++; return int64(starts), nil }, nil,
		func(*rpcMessage) (string, error) { calls++; return "handled", nil },
		testLogger(), func(ChatEvent) bool { return true })
	if starts != 1 || calls != 1 || !result.activity || result.processErrorCode != "serverOverloaded" {
		t.Fatalf("starts=%d calls=%d result=%+v", starts, calls, result)
	}
}

func TestCodexOverloadInterruptedAndBrokenStreamsNeverRetry(t *testing.T) {
	for name, lines := range map[string][]string{
		"interrupted": {rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "interrupted", "error": map[string]any{"message": "capacity", "codexErrorInfo": "serverOverloaded"}}})},
		"broken":      {rpcLine("error", map[string]any{"error": map[string]any{"message": "capacity", "codexErrorInfo": "serverOverloaded"}})},
		"RPC error":   {rpcResponseLine(1, nil, &rpcError{Code: -1, Message: "Selected model is at capacity. Please try a different model."})},
	} {
		t.Run(name, func(t *testing.T) {
			_, starts := runOverloadTest(context.Background(), lines, codexTurnRetryPolicy{overloadDelays: []time.Duration{0, 0}})
			if len(starts) != 1 {
				t.Fatalf("starts = %q", starts)
			}
		})
	}
}
