package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func codexQuestionRequest(id string) *rpcMessage {
	raw := json.RawMessage(id)
	params := json.RawMessage(`{"questions":[{"id":"color","header":"Color","question":"Color?","options":[{"label":"Blue"}]}]}`)
	return &rpcMessage{ID: &raw, Method: "item/tool/requestUserInput", Params: &params}
}
func TestCodexQuestionsTypedAnswerAndDuplicate(t *testing.T) {
	for _, id := range []string{`42`, `"rpc-id"`} {
		t.Run(id, func(t *testing.T) {
			var response []byte
			events := make(chan ChatEvent, 10)
			q := newCodexQuestionState(func(v any) error { response, _ = json.Marshal(v); return nil }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
			defer q.close()
			if _, err := q.register(codexQuestionRequest(id), false); err != nil {
				t.Fatal(err)
			}
			ev := <-events
			if ev.Type != "user_question" || ev.RequestID == id {
				t.Fatalf("event: %+v", ev)
			}
			if response != nil {
				t.Fatal("request must remain pending")
			}
			if err := q.answer(ev.RequestID, map[string]any{"wrong": "Blue"}, false, ""); !errors.Is(err, ErrInvalidQuestionAnswer) {
				t.Fatal(err)
			}
			if err := q.answer(ev.RequestID, map[string]any{"color": "Blue"}, false, ""); err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID     json.RawMessage
				Result struct {
					Answers map[string]struct{ Answers []string }
				}
			}
			if err := json.Unmarshal(response, &got); err != nil {
				t.Fatal(err)
			}
			if string(got.ID) != id || len(got.Result.Answers["color"].Answers) != 1 || got.Result.Answers["color"].Answers[0] != "Blue" {
				t.Fatalf("response: %s", response)
			}
			if (<-events).Type != "question_resolved" {
				t.Fatal("missing resolved")
			}
			if err := q.answer(ev.RequestID, nil, true, ""); !errors.Is(err, ErrQuestionNotFound) {
				t.Fatal(err)
			}
		})
	}
}
func TestCodexQuestionTimeoutResolutionAndClose(t *testing.T) {
	events := make(chan ChatEvent, 10)
	writes := make(chan any, 10)
	q := newCodexQuestionState(func(v any) error { writes <- v; return nil }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
	req := codexQuestionRequest(`1`)
	raw := json.RawMessage(`{"autoResolutionMs":1,"questions":[{"id":"x","question":"X?"}]}`)
	req.Params = &raw
	if _, err := q.register(req, true); err != nil {
		t.Fatal(err)
	}
	raised := <-events
	q.mu.Lock()
	q.pending[raised.RequestID].timer.Reset(time.Millisecond)
	q.mu.Unlock()
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("timeout didn't resolve")
	}
	select {
	case ev := <-events:
		if ev.Type != "question_resolved" {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing resolution")
	}
	if err := q.answer(raised.RequestID, nil, true, ""); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatal(err)
	}
	if _, err := q.register(codexQuestionRequest(`2`), false); err != nil {
		t.Fatal(err)
	}
	<-events
	q.resolveRPC(json.RawMessage(`2`))
	<-events
	if len(writes) != 0 {
		t.Fatal("server-resolved request must not be answered again")
	}
	q.close()
	if _, err := q.register(codexQuestionRequest(`3`), false); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatal(err)
	}
}
func TestCodexQuestionConcurrentCloseAnswer(t *testing.T) {
	for i := 0; i < 50; i++ {
		events := make(chan ChatEvent, 4)
		q := newCodexQuestionState(func(any) error { return nil }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
		_, _ = q.register(codexQuestionRequest(`1`), false)
		ev := <-events
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = q.answer(ev.RequestID, map[string]any{"color": "Blue"}, false, "") }()
		q.close()
		close(events)
		wg.Wait()
	}
}
func TestOneShotQuestionsFencedAndReliable(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := ChatOptions{}
	q := m.setupOneShotQuestions(ctx, "a", 1, OneShotOpts{SessionKey: "s", OriginPeerID: "hub", InteractiveQuestions: true}, &opts)
	calls := 0
	opts.OnQuestionReady(func(id string, a map[string]any, d bool, msg string) error {
		calls++
		opts.OnQuestionResolved(id)
		return nil
	})
	backend := make(chan ChatEvent, 3)
	out := m.oneShotQuestionEvents(ctx, backend, q)
	backend <- ChatEvent{Type: "user_question", RequestID: "r", Questions: json.RawMessage(`[{"question":"Q?"}]`)}
	if ev := <-out; ev.Type != "user_question" {
		t.Fatal(ev)
	}
	if !m.HasPendingQuestion("a") {
		t.Fatal("not awaiting answer")
	}
	for _, tc := range []struct {
		a, s, o string
		want    error
	}{{"b", "s", "hub", ErrAgentNotBusy}, {"a", "other", "hub", ErrAgentNotBusy}, {"a", "s", "other", ErrSteerOriginForbidden}} {
		if err := m.AnswerOneShotQuestion(tc.a, tc.s, tc.o, "r", map[string]any{"Q?": "A"}, false, ""); !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
	}
	if err := m.AnswerOneShotQuestion("a", "s", "hub", "r", map[string]any{"Q?": "A"}, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.AnswerOneShotQuestion("a", "s", "hub", "r", map[string]any{"Q?": "A"}, false, ""); !errors.Is(err, ErrQuestionNotFound) {
		t.Fatal(err)
	}
	if calls != 1 || m.HasPendingQuestion("a") {
		t.Fatalf("calls=%d", calls)
	}
	if ev := <-out; ev.Type != "question_resolved" {
		t.Fatal(ev)
	}
	close(backend)
	for range out {
	}
}

func TestOneShotQuestionCancelledTerminalAndEarlyResolution(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	opts := ChatOptions{}
	q := m.setupOneShotQuestions(ctx, "a", 1, OneShotOpts{SessionKey: "s", InteractiveQuestions: true}, &opts)
	backend := make(chan ChatEvent, 3)
	out := m.oneShotQuestionEvents(ctx, backend, q)
	opts.OnQuestionResolved("old")
	backend <- ChatEvent{Type: "user_question", RequestID: "old", Questions: json.RawMessage(`[{"question":"Q?"}]`)}
	ev := <-out
	if ev.Type != "question_resolved" {
		t.Fatal(ev)
	}
	cancel()
	backend <- ChatEvent{Type: "done", Message: &Message{Content: "cancelled partial"}}
	close(backend)
	found := false
	for ev := range out {
		if ev.Type == "user_question" {
			t.Fatal("expired question resurrected")
		}
		if ev.Type == "done" {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal was lost on cancellation")
	}
}

func TestOneShotQuestionBackpressureNeverDropsPrompt(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := make(chan ChatEvent, 100)
	out := make(chan ChatEvent, 1)
	out <- ChatEvent{Type: "text", Delta: "occupied"}
	done := make(chan struct{})
	go func() { defer close(done); m.processOneShotEvents(ctx, "a", backend, out, false) }()
	backend <- ChatEvent{Type: "user_question", RequestID: "r"}
	close(backend)
	<-out
	select {
	case ev := <-out:
		if ev.Type != "user_question" {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("question dropped")
	}
	<-done
}

func TestClaudeInvalidAnswerLeavesQuestionPending(t *testing.T) {
	w, _ := newTestStdinWriter()
	q := newClaudeQuestionState(w)
	q.register("r", json.RawMessage(askQuestionInput), 0, "")
	if err := q.answer("r", map[string]any{"wrong": "A"}, false, ""); !errors.Is(err, ErrInvalidQuestionAnswer) {
		t.Fatal(err)
	}
	if err := q.answer("r", map[string]any{"色は?": "青"}, false, ""); err != nil {
		t.Fatal(err)
	}
}

func TestQuestionSingleChoiceAndTransportLimits(t *testing.T) {
	qs := []UserQuestion{{ID: "q", Question: "Q?"}}
	if err := ValidateQuestionAnswers(qs, map[string]any{"q": []string{"a", "b"}}); err == nil {
		t.Fatal("single choice accepted multiple answers")
	}
	if err := ValidateQuestionAnswers(qs, map[string]any{"q": 123}); err == nil {
		t.Fatal("non-string accepted")
	}
}

func TestCodexAsyncQuestionUsesServerResolution(t *testing.T) {
	events := make(chan ChatEvent, 10)
	writes := 0
	q := newCodexQuestionState(func(any) error { writes++; return nil }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
	defer q.close()
	req := codexQuestionRequest(`1`)
	raw := json.RawMessage(`{"isBlocking":false,"autoResolutionMs":1,"questions":[{"id":"x","question":"X?"}]}`)
	req.Params = &raw
	_, err := q.register(req, true)
	if err != nil {
		t.Fatal(err)
	}
	ev := <-events
	if ev.QuestionBlocking == nil || *ev.QuestionBlocking {
		t.Fatal("lost nonblocking semantics")
	}
	q.mu.Lock()
	timer := q.pending[ev.RequestID].timer
	q.mu.Unlock()
	if timer != nil {
		t.Fatal("async question armed a client timeout")
	}
	q.resolveRPC(json.RawMessage(`1`))
	if writes != 0 {
		t.Fatal("client raced native resolution with a write")
	}
	if (<-events).Type != "question_resolved" {
		t.Fatal("missing resolution")
	}
}

func TestQuestionWriteFailureTerminatesUnusableBackend(t *testing.T) {
	events := make(chan ChatEvent, 8)
	terminated := false
	q := newCodexQuestionState(func(any) error { return errors.New("write failed") }, func(ev ChatEvent) bool { events <- ev; return true }, nil)
	q.onWriteFailure = func() { terminated = true }
	defer q.close()
	_, _ = q.register(codexQuestionRequest(`1`), false)
	ev := <-events
	if err := q.answer(ev.RequestID, map[string]any{"color": "Blue"}, false, ""); err == nil {
		t.Fatal("write error hidden")
	}
	if !terminated {
		t.Fatal("question consumed but backend left blocked")
	}
	foundError := false
	for len(events) > 0 {
		if (<-events).Type == "error" {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("missing terminal error")
	}
}

func TestQuestionResolutionQueueSurvivesUIBackpressureAndCloses(t *testing.T) {
	m := &Manager{busy: map[string]busyEntry{"a": {outCh: make(chan ChatEvent, 1)}}}
	m.busy["a"].outCh <- ChatEvent{Type: "text"}
	m.clearQuestion("a", "r")
	q := m.busy["a"].questionLifecycle
	if ev := <-q.events; ev.RequestID != "r" {
		t.Fatal(ev)
	}
	for i := 0; i < cap(q.events); i++ {
		q.events <- ChatEvent{Type: "question_resolved"}
	}
	done := make(chan struct{})
	go func() { m.clearQuestion("a", "blocked"); close(done) }()
	m.clearBusy("a")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed turn left callback blocked")
	}
}

func TestExternalAsyncQuestionDoesNotMarkAgentBlocked(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := ChatOptions{}
	q := m.setupOneShotQuestions(ctx, "a", 1, OneShotOpts{SessionKey: "s", InteractiveQuestions: true}, &opts)
	opts.OnQuestionReady(func(string, map[string]any, bool, string) error { return nil })
	backend := make(chan ChatEvent, 1)
	out := m.oneShotQuestionEvents(ctx, backend, q)
	blocking := false
	backend <- ChatEvent{Type: "user_question", RequestID: "r", Questions: json.RawMessage(`[{"question":"Q?"}]`), QuestionBlocking: &blocking}
	<-out
	if m.HasPendingQuestion("a") {
		t.Fatal("async prompt marked turn blocked")
	}
	if err := m.AnswerOneShotQuestion("a", "s", "", "r", map[string]any{"Q?": "A"}, false, ""); err != nil {
		t.Fatal(err)
	}
	close(backend)
	for range out {
	}
}
