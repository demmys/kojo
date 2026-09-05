package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// A fresh opaque ID per prompt prevents a late answer matching a reused RPC ID
// after a process restart. Native numeric and string RPC IDs remain untouched.
type codexPendingQuestion struct {
	asyncItem string
	rpcID     json.RawMessage
	questions []UserQuestion
	timer     *time.Timer
}
type codexQuestionState struct {
	steer          func(string) error // installed before reading turn notifications
	asyncSeen      map[string]bool    // accessed by the stream reader only
	onWriteFailure func()             // immutable; terminates an unusable CLI pipe

	mu       sync.Mutex
	emitMu   sync.Mutex
	ended    bool
	pending  map[string]*codexPendingQuestion
	write    func(any) error
	send     func(ChatEvent) bool
	resolved func(string)
	closed   bool
}

func newCodexQuestionState(write func(any) error, send func(ChatEvent) bool, resolved func(string)) *codexQuestionState {
	return &codexQuestionState{pending: make(map[string]*codexPendingQuestion), write: write, send: send, resolved: resolved}
}
func (q *codexQuestionState) register(msg *rpcMessage, automated bool) (string, error) {
	if msg == nil || msg.ID == nil || msg.Params == nil {
		return "", fmt.Errorf("missing question params")
	}
	var p struct {
		Questions []UserQuestion `json:"questions"`
	}
	if err := json.Unmarshal(*msg.Params, &p); err != nil {
		return "", err
	}
	if len(p.Questions) == 0 || len(p.Questions) > 10 {
		return "", fmt.Errorf("invalid question count")
	}
	seen := make(map[string]bool)
	for i := range p.Questions {
		item := &p.Questions[i]
		if item.IsOther == nil {
			v := false
			item.IsOther = &v
		}
		if item.ID == "" || item.Question == "" || seen[item.ID] || item.IsSecret {
			return "", fmt.Errorf("invalid or secret question")
		}
		seen[item.ID] = true
	}
	id := uuid.NewString()
	entry := &codexPendingQuestion{rpcID: append(json.RawMessage(nil), (*msg.ID)...), questions: p.Questions}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return "", ErrAgentNotBusy
	}
	q.pending[id] = entry
	q.mu.Unlock()
	raw, _ := json.Marshal(p.Questions)
	// The request_user_input tool awaits this JSON-RPC answer even when
	// Default mode reports isBlocking=false (a native UI policy hint).
	// Unlike message-delivered async questions, Kojo holds this request open.
	blocking := true
	if !q.emit(ChatEvent{Type: "user_question", RequestID: id, Questions: raw, QuestionBlocking: &blocking}) {
		_ = q.answer(id, nil, true, "")
		return "", nil
	}
	timeout := time.Duration(0)
	// autoResolutionMs is deprecated; app-server owns native expiration and
	// announces it through serverRequest/resolved. Only Kojo's automated-turn
	// safety timeout can send an empty answer to an unwatched blocking prompt.
	if automated {
		timeout = automatedQuestionTimeout
	}
	q.mu.Lock()
	if q.pending[id] == entry && timeout > 0 {
		entry.timer = time.AfterFunc(timeout, func() { _ = q.answer(id, nil, true, "") })
	}
	q.mu.Unlock()
	return "user_input_pending", nil
}
func (q *codexQuestionState) answer(id string, answers map[string]any, deny bool, denyMessage string) error {
	q.mu.Lock()
	entry := q.pending[id]
	if entry == nil {
		q.mu.Unlock()
		return ErrQuestionNotFound
	}
	if !deny {
		if err := ValidateQuestionAnswers(entry.questions, answers); err != nil {
			q.mu.Unlock()
			return err
		}
	}
	delete(q.pending, id)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	q.mu.Unlock()
	if entry.asyncItem != "" {
		// No JSON-RPC response exists for request_user_input_async.
		// Claim before sending; uncertain delivery must never be replayed.
		defer q.notifyResolved(id)
		var text strings.Builder
		fmt.Fprintf(&text, "Answer to your asynchronous question (item %s):\n", entry.asyncItem)
		if deny {
			text.WriteString("The question was declined or could not be displayed. No answer or approval was provided. Do not assume consent. Do not wait for an answer to this form; ask in normal chat if clarification is still necessary.\n")
			if denyMessage != "" {
				fmt.Fprintf(&text, "Reason: %s\n", denyMessage)
			}
		} else {
			for _, item := range entry.questions {
				vals, _ := questionAnswerStrings(answers[item.ID])
				fmt.Fprintf(&text, "%s\nAnswer: %s\n", item.Question, strings.Join(vals, ", "))
			}
		}
		if q.steer == nil {
			return ErrAgentNotBusy
		}
		return q.steer(text.String())
	}
	result := make(map[string]any)
	if !deny {
		for _, item := range entry.questions {
			vals, _ := questionAnswerStrings(answers[item.ID])
			result[item.ID] = map[string]any{"answers": vals}
		}
	}
	err := q.write(map[string]any{"jsonrpc": "2.0", "id": entry.rpcID, "result": map[string]any{"answers": result}})
	if err != nil && q.onWriteFailure != nil {
		q.onWriteFailure()
	}
	q.notifyResolved(id)
	if err != nil {
		q.emit(ChatEvent{Type: "error", ErrorMessage: "Could not deliver the question answer; the backend connection was closed."})
	}
	return err
}
func (q *codexQuestionState) emit(e ChatEvent) bool {
	q.emitMu.Lock()
	defer q.emitMu.Unlock()
	if q.ended {
		return false
	}
	return q.send(e)
}
func (q *codexQuestionState) notifyResolved(id string) {
	q.emitMu.Lock()
	defer q.emitMu.Unlock()
	if q.ended {
		return
	}
	if q.resolved != nil {
		q.resolved(id)
	}
	q.send(ChatEvent{Type: "question_resolved", RequestID: id})
}
func (q *codexQuestionState) resolveRPC(raw json.RawMessage) {
	q.mu.Lock()
	var id string
	for key, p := range q.pending {
		if len(p.rpcID) > 0 && string(p.rpcID) == string(raw) {
			id = key
			delete(q.pending, key)
			if p.timer != nil {
				p.timer.Stop()
			}
			break
		}
	}
	q.mu.Unlock()
	if id != "" {
		q.notifyResolved(id)
	}
}
func (q *codexQuestionState) close() {
	q.emitMu.Lock()
	defer q.emitMu.Unlock()
	q.ended = true
	q.mu.Lock()
	q.closed = true
	ids := make([]string, 0, len(q.pending))
	for id, p := range q.pending {
		ids = append(ids, id)
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	q.pending = make(map[string]*codexPendingQuestion)
	q.mu.Unlock()
	// No event send at shutdown: the terminal event clears presentation state.
	for _, id := range ids {
		if q.resolved != nil {
			q.resolved(id)
		}
	}
}

// registerAsync converts Codex's native message-delivered question schema into
// the shared UI schema. Its answer is a new user input, not an RPC result.
func (q *codexQuestionState) registerAsync(msg *rpcMessage) error {
	if msg.Params == nil {
		return nil
	}
	var p struct {
		Item struct {
			ID        string `json:"id"`
			Delivery  string `json:"delivery"`
			Questions []struct {
				Title   string   `json:"title"`
				Options []string `json:"options"`
			} `json:"questions"`
		} `json:"item"`
	}
	if err := json.Unmarshal(*msg.Params, &p); err != nil {
		return err
	}
	if p.Item.Delivery != "async" || len(p.Item.Questions) == 0 {
		return nil
	}
	if p.Item.ID == "" || len(p.Item.Questions) > 10 {
		return fmt.Errorf("invalid async question")
	}
	if q.asyncSeen == nil {
		q.asyncSeen = make(map[string]bool)
	}
	if q.asyncSeen[p.Item.ID] {
		return nil
	}
	questions := make([]UserQuestion, 0, len(p.Item.Questions))
	for i, item := range p.Item.Questions {
		if strings.TrimSpace(item.Title) == "" {
			return fmt.Errorf("empty async question")
		}
		free := true
		question := UserQuestion{ID: fmt.Sprintf("question_%d", i), Question: item.Title, IsOther: &free}
		for _, label := range item.Options {
			question.Options = append(question.Options, UserQuestionOption{Label: label})
		}
		questions = append(questions, question)
	}
	id := uuid.NewString()
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrAgentNotBusy
	}
	q.pending[id] = &codexPendingQuestion{asyncItem: p.Item.ID, questions: questions}
	q.mu.Unlock()
	q.asyncSeen[p.Item.ID] = true
	raw, _ := json.Marshal(questions)
	blocking := false
	if !q.emit(ChatEvent{Type: "user_question", RequestID: id, Questions: raw, QuestionBlocking: &blocking}) {
		// We are on the stream reader: never wait for a steer ACK here.
		// Failed emission means the run is ending, so expire silently.
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		q.notifyResolved(id)
	}
	return nil
}
