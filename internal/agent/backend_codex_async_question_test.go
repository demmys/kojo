package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

const asyncQuestionNotification = `{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"call_async","text":"Which period?","delivery":"async","questions":[{"title":"Which period?","options":["Before","After"]},{"title":"Any constraints?"}]}}}`

func TestCodexAsyncQuestionStreamAndAnswer(t *testing.T) {
	events := make(chan ChatEvent, 16)
	writes := 0
	var input string
	q := newCodexQuestionState(func(any) error { writes++; return nil }, func(e ChatEvent) bool { events <- e; return true }, nil)
	q.steer = func(s string) error { input = s; return nil }
	defer q.close()
	stream := asyncQuestionNotification + "\n" + asyncQuestionNotification + "\n" + `{"method":"turn/completed","params":{"turn":{"id":"t","status":"completed"}}}` + "\n"
	result := parseCodexStream(newCodexLineScanner(strings.NewReader(stream)), 1, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ChatEvent) bool { return true }, q)
	if !result.hasFinalResponse() || !result.turnCompleted {
		t.Fatalf("lost final response: %+v", result)
	}
	if len(events) != 1 {
		t.Fatalf("want one card, got %d", len(events))
	}
	ev := <-events
	q.resolveRPC(nil) // unrelated RPC resolution must not consume a message question
	if ev.Type != "user_question" || ev.QuestionBlocking == nil || *ev.QuestionBlocking {
		t.Fatal(ev)
	}
	var questions []UserQuestion
	if err := json.Unmarshal(ev.Questions, &questions); err != nil {
		t.Fatal(err)
	}
	if len(questions) != 2 || !questions[0].AllowsFreeText() || questions[0].Options[0].Label != "Before" {
		t.Fatal(questions)
	}
	if err := q.answer(ev.RequestID, map[string]any{"question_0": "Custom period", "question_1": "Low cost"}, false, ""); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || !strings.Contains(input, "Custom period") || !strings.Contains(input, "Which period?") || !strings.Contains(input, "Low cost") {
		t.Fatalf("writes=%d input=%s", writes, input)
	}
	if err := q.answer(ev.RequestID, nil, true, ""); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatal(err)
	}
}

func TestCodexAsyncQuestionFailureCloseAndDeny(t *testing.T) {
	for _, mode := range []string{"deny", "close", "uncertain", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			events := make(chan ChatEvent, 16)
			q := newCodexQuestionState(func(any) error { t.Error("unexpected RPC response"); return nil }, func(e ChatEvent) bool { events <- e; return true }, nil)
			calls := 0
			q.steer = func(string) error {
				calls++
				if mode == "deny" {
					return nil
				}
				if mode == "uncertain" {
					return ErrSteerDeliveryUncertain
				}
				return ErrAgentNotBusy
			}
			defer q.close()
			var msg rpcMessage
			_ = json.Unmarshal([]byte(asyncQuestionNotification), &msg)
			if err := q.registerAsync(&msg); err != nil {
				t.Fatal(err)
			}
			ev := <-events
			if mode == "close" {
				q.close()
			}
			err := q.answer(ev.RequestID, map[string]any{"question_0": "Before", "question_1": "None"}, mode == "deny", "")
			switch mode {
			case "deny":
				if err != nil || calls != 1 {
					t.Fatal(err, calls)
				}
			case "close":
				if !errors.Is(err, ErrQuestionNotFound) || calls != 0 {
					t.Fatal(err, calls)
				}
			case "uncertain":
				if !errors.Is(err, ErrSteerDeliveryUncertain) || calls != 1 {
					t.Fatal(err, calls)
				}
			case "rejected":
				if !errors.Is(err, ErrAgentNotBusy) || calls != 1 {
					t.Fatal(err, calls)
				}
			}
			if err := q.answer(ev.RequestID, nil, true, ""); !errors.Is(err, ErrQuestionNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexAsyncQuestionOneShotOriginAndStop(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := ChatOptions{}
	qt := m.setupOneShotQuestions(ctx, "agent", 1, OneShotOpts{SessionKey: "slack:thread", OriginPeerID: "hub", InteractiveQuestions: true}, &opts)
	backend := make(chan ChatEvent, 16)
	q := newCodexQuestionState(func(any) error { return nil }, func(e ChatEvent) bool { backend <- e; return true }, opts.OnQuestionResolved)
	q.steer = func(string) error { return nil }
	opts.OnQuestionReady(q.answer)
	defer q.close()
	out := m.oneShotQuestionEvents(ctx, backend, qt)
	var msg rpcMessage
	_ = json.Unmarshal([]byte(asyncQuestionNotification), &msg)
	if err := q.registerAsync(&msg); err != nil {
		t.Fatal(err)
	}
	ev := <-out
	answers := map[string]any{"question_0": "Before", "question_1": "None"}
	if err := m.AnswerOneShotQuestion("agent", "slack:thread", "other-hub", ev.RequestID, answers, false, ""); !errors.Is(err, ErrSteerOriginForbidden) {
		t.Fatal(err)
	}
	if err := m.AnswerOneShotQuestion("agent", "slack:thread", "hub", ev.RequestID, answers, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.AnswerOneShotQuestion("agent", "slack:thread", "hub", ev.RequestID, answers, false, ""); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatal(err)
	}
	q.close()
	close(backend)
	for range out {
	}
}

func TestCodexAsyncQuestionNativeGoalContinuation(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	tid := "019e7cc9-dd5e-7971-b654-7840c683879e"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: tid}, logger)
	goal := func(status string) map[string]any {
		return map[string]any{"goal": map[string]any{"threadId": tid, "status": status, "objective": "test"}}
	}
	resp := func(id int, v any) string {
		b, _ := json.Marshal(map[string]any{"id": id, "result": v})
		return string(b)
	}
	lines := []string{
		resp(1, goal("active")), rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "one"}}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}), resp(2, goal("active")),
		rpcLine("turn/started", map[string]any{"turn": map[string]any{"id": "two"}}),
		asyncQuestionNotification,
		rpcLine("thread/goal/updated", goal("complete")),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}), resp(3, goal("complete")),
	}
	events := make(chan ChatEvent, 16)
	q := newCodexQuestionState(func(any) error { return nil }, func(e ChatEvent) bool { events <- e; return true }, nil)
	defer q.close()
	n := int64(0)
	r := &codexGoalRuntime{agentID: id, threadID: tid, pending: map[int64]chan *rpcMessage{}, write: func(string, any) (int64, error) { n++; return n, nil }}
	result := runCodexGoal(newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n"))), &GoalRequest{Action: "start", Objective: "test"}, r, nil, nil, logger, func(ChatEvent) bool { return true }, q)
	if result.processError != "" || !result.turnCompleted {
		t.Fatal(result.processError)
	}
	if len(events) != 1 || (<-events).Type != "user_question" {
		t.Fatal("lost question after native continuation")
	}
}

func TestAsyncQuestionUncertainAnswerKeepsHistory(t *testing.T) {
	m := newTestManager(t)
	a := &Agent{ID: "ag_test", Name: "Test", Tool: ToolCodex}
	m.agents[a.ID] = a
	if err := m.store.Upsert(a); err != nil {
		t.Fatal(err)
	}
	out := make(chan ChatEvent, 1)
	m.busy[a.ID] = busyEntry{outCh: out, cancel: func() {}, answer: func(string, map[string]any, bool, string) error { return ErrSteerDeliveryUncertain }}
	if err := m.AnswerQuestion(context.Background(), a.ID, "request", map[string]any{"color": "Blue"}, false, ""); !errors.Is(err, ErrSteerDeliveryUncertain) {
		t.Fatal(err)
	}
	msgs, err := m.Messages(a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range msgs {
		if msg.Role == "user" && strings.Contains(msg.Content, "Blue") {
			found = true
		}
	}
	if !found {
		t.Fatal("lost possibly delivered answer")
	}
	select {
	case ev := <-out:
		if ev.Type != "message" {
			t.Fatal(ev)
		}
	default:
		t.Fatal("retained answer not published")
	}
}

func TestAsyncQuestionDoesNotPolluteFinalText(t *testing.T) {
	q := newCodexQuestionState(func(any) error { return nil }, func(ChatEvent) bool { return true }, nil)
	defer q.close()
	lines := asyncQuestionNotification + "\n" + rpcLine("item/agentMessage/delta", map[string]any{"delta": "Blue"}) + "\n" + rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
	result := parseCodexStream(newCodexLineScanner(strings.NewReader(lines)), 1, nil, nil, slog.Default(), func(ChatEvent) bool { return true }, q)
	if got := result.buildMessage().Content; got != "Blue" {
		t.Fatalf("final=%q", got)
	}
	// Ending immediately after a question is not an empty-completion retry.
	result = &codexStreamResult{questionText: "Which period?"}
	if !result.hasFinalResponse() || result.buildMessage().Content != "Which period?" {
		t.Fatal("lost question fallback")
	}
}

func TestAsyncQuestionNonInteractiveFallback(t *testing.T) {
	lines := asyncQuestionNotification + "\n" + rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
	var text strings.Builder
	result := parseCodexStream(newCodexLineScanner(strings.NewReader(lines)), 1, nil, nil, slog.Default(), func(ev ChatEvent) bool {
		if ev.Type == "text" {
			text.WriteString(ev.Delta)
		}
		return true
	})
	if text.String() != "Which period?" || !result.hasFinalResponse() {
		t.Fatal("missing plain-text fallback")
	}
}

func TestAsyncQuestionDenyNotifiesButExpiryDoesNot(t *testing.T) {
	var msg rpcMessage
	_ = json.Unmarshal([]byte(asyncQuestionNotification), &msg)
	events := make(chan ChatEvent, 4)
	var input string
	q := newCodexQuestionState(func(any) error { t.Fatal("RPC reply"); return nil }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
	q.steer = func(s string) error { input = s; return nil }
	defer q.close()
	if err := q.registerAsync(&msg); err != nil {
		t.Fatal(err)
	}
	ev := <-events
	if err := q.answer(ev.RequestID, nil, true, "Slack form delivery failed"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "Slack form delivery failed") {
		t.Fatal(input)
	}
	q2 := newCodexQuestionState(func(any) error { return nil }, func(ChatEvent) bool { return false }, nil)
	q2.steer = func(string) error { t.Fatal("stream-reader expiry must not wait for ACK"); return nil }
	defer q2.close()
	if err := q2.registerAsync(&msg); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncQuestionFallbackKeepsMultiplePrompts(t *testing.T) {
	q := newCodexQuestionState(func(any) error { return nil }, func(ChatEvent) bool { return true }, nil)
	defer q.close()
	second := strings.ReplaceAll(strings.ReplaceAll(asyncQuestionNotification, "call_async", "call_second"), "Which period?", "Second question?")
	lines := asyncQuestionNotification + "\n" + asyncQuestionNotification + "\n" + second + "\n" + rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
	result := parseCodexStream(newCodexLineScanner(strings.NewReader(lines)), 1, nil, nil, slog.Default(), func(ChatEvent) bool { return true }, q)
	want := "Which period?\n\nSecond question?"
	if got := result.buildMessage().Content; got != want {
		t.Fatalf("fallback=%q", got)
	}
	combined := &codexStreamResult{questionText: "Earlier"}
	combined.absorb(result)
	if got := combined.buildMessage().Content; got != "Earlier\n\n"+want {
		t.Fatal(got)
	}
}
