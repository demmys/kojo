package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// A fresh opaque ID per prompt prevents a late answer matching a reused RPC ID
// after a process restart. Native numeric and string RPC IDs remain untouched.
type codexPendingQuestion struct {
	rpcID     json.RawMessage
	questions []UserQuestion
	timer     *time.Timer
}
type codexQuestionState struct {
	onWriteFailure func() // immutable; terminates an unusable CLI pipe

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
		Blocking  *bool          `json:"isBlocking"`
	}
	if err := json.Unmarshal(*msg.Params, &p); err != nil {
		return "", err
	}
	if len(p.Questions) == 0 || len(p.Questions) > 10 {
		return "", fmt.Errorf("invalid question count")
	}
	seen := make(map[string]bool)
	for _, item := range p.Questions {
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
	if !q.emit(ChatEvent{Type: "user_question", RequestID: id, Questions: raw, QuestionBlocking: p.Blocking}) {
		_ = q.answer(id, nil, true, "")
		return "", nil
	}
	timeout := time.Duration(0)
	// isBlocking=false is an async prompt: the model continues working.
	// autoResolutionMs is deprecated; app-server owns native expiration and
	// announces it through serverRequest/resolved. Only Kojo's automated-turn
	// safety timeout can send an empty answer to an unwatched blocking prompt.
	if automated && (p.Blocking == nil || *p.Blocking) {
		timeout = automatedQuestionTimeout
	}
	q.mu.Lock()
	if q.pending[id] == entry && timeout > 0 {
		entry.timer = time.AfterFunc(timeout, func() { _ = q.answer(id, nil, true, "") })
	}
	q.mu.Unlock()
	return "user_input_pending", nil
}
func (q *codexQuestionState) answer(id string, answers map[string]any, deny bool, _ string) error {
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
		if string(p.rpcID) == string(raw) {
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
