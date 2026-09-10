package agent

import (
	"context"
	"encoding/json"
)

// QuestionAnswer is the common answer envelope used by external transports.
type QuestionAnswer struct {
	RequestID   string         `json:"requestId"`
	Answers     map[string]any `json:"answers,omitempty"`
	Deny        bool           `json:"deny,omitempty"`
	DenyMessage string         `json:"denyMessage,omitempty"`
}

// External answers are scoped to the exact turn, agent, session and originating
// Hub. They never enter Steer or the normal follow-up queue, even on failure.
// All fields except immutable identity/channels are guarded by oneShotCancelsMu.
type pendingExternalQuestion struct {
	questions []UserQuestion
	blocking  bool
}

type oneShotQuestionTurn struct {
	agentID, sessionKey, origin string
	answer                      AnswerFunc
	pending                     map[string]pendingExternalQuestion
	resolved                    map[string]bool
	events                      chan ChatEvent
	ctx                         context.Context
}

func (m *Manager) setupOneShotQuestions(ctx context.Context, agentID string, id int64, opts OneShotOpts, chatOpts *ChatOptions) *oneShotQuestionTurn {
	if !opts.InteractiveQuestions || opts.SessionKey == "" {
		return nil
	}
	q := &oneShotQuestionTurn{agentID: agentID, sessionKey: opts.SessionKey, origin: opts.OriginPeerID,
		pending: make(map[string]pendingExternalQuestion), resolved: make(map[string]bool), events: make(chan ChatEvent, 64), ctx: ctx}
	m.oneShotCancelsMu.Lock()
	if m.oneShotQuestions == nil {
		m.oneShotQuestions = make(map[int64]*oneShotQuestionTurn)
	}
	m.oneShotQuestions[id] = q
	m.oneShotCancelsMu.Unlock()
	chatOpts.OnQuestionReady = func(fn AnswerFunc) {
		m.oneShotCancelsMu.Lock()
		defer m.oneShotCancelsMu.Unlock()
		if m.oneShotQuestions[id] == q {
			q.answer = fn
		}
	}
	chatOpts.OnQuestionResolved = func(requestID string) {
		m.oneShotCancelsMu.Lock()
		live := m.oneShotQuestions[id] == q
		if live {
			delete(q.pending, requestID)
			q.resolved[requestID] = true
		}
		m.oneShotCancelsMu.Unlock()
		if live {
			ctxSend(ctx, q.events, ChatEvent{Type: "question_resolved", RequestID: requestID})
		}
	}
	return q
}

func (m *Manager) oneShotQuestionEvents(ctx context.Context, backend <-chan ChatEvent, q *oneShotQuestionTurn) <-chan ChatEvent {
	if q == nil {
		return backend
	}
	out := make(chan ChatEvent, 64)
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-backend:
				if !ok {
					return
				}
				if ev.Type == "user_question" {
					var questions []UserQuestion
					_ = json.Unmarshal(ev.Questions, &questions)
					m.oneShotCancelsMu.Lock()
					expired := q.resolved[ev.RequestID]
					if !expired {
						q.pending[ev.RequestID] = pendingExternalQuestion{questions: questions, blocking: ev.QuestionBlocking == nil || *ev.QuestionBlocking}
					}
					m.oneShotCancelsMu.Unlock()
					if expired {
						continue
					}
				}
				// Backend resolution events are supplied by the callback above as well.
				if ev.Type == "question_resolved" {
					continue
				}
				if !ctxSend(ctx, out, ev) {
					if ev.Type == "done" || ev.Type == "error" {
						out <- ev
					} else {
						ev.rejectAttachmentOwnership()
					}
					forwardQuestionCancelled(backend, out)
					return
				}
			case ev := <-q.events:
				if !ctxSend(ctx, out, ev) {
					forwardQuestionCancelled(backend, out)
					return
				}
			case <-ctx.Done():
				// Preserve terminal semantics by forwarding the drained backend stream.
				forwardQuestionCancelled(backend, out)
				return
			}
		}
	}()
	return out
}

func (m *Manager) AnswerOneShotQuestion(agentID, sessionKey, origin, requestID string, answers map[string]any, deny bool, denyMessage string) error {
	m.oneShotCancelsMu.Lock()
	var q *oneShotQuestionTurn
	for _, candidate := range m.oneShotQuestions {
		if candidate.agentID != agentID || candidate.sessionKey != sessionKey {
			continue
		}
		if q != nil {
			m.oneShotCancelsMu.Unlock()
			return ErrAgentNotBusy
		}
		q = candidate
	}
	if q == nil || q.ctx.Err() != nil {
		m.oneShotCancelsMu.Unlock()
		return ErrAgentNotBusy
	}
	if origin != "" && q.origin != origin {
		m.oneShotCancelsMu.Unlock()
		return ErrSteerOriginForbidden
	}
	questions, ok := q.pending[requestID]
	if !ok || q.answer == nil {
		m.oneShotCancelsMu.Unlock()
		return ErrQuestionNotFound
	}
	if !deny {
		if err := ValidateQuestionAnswers(questions.questions, answers); err != nil {
			m.oneShotCancelsMu.Unlock()
			return err
		}
	}
	fn := q.answer
	// Reserve before releasing the lock; a second UI/Slack submission cannot
	// reach the backend, even if the first write is slow or delivery-uncertain.
	delete(q.pending, requestID)
	q.resolved[requestID] = true
	m.oneShotCancelsMu.Unlock()
	return fn(requestID, answers, deny, denyMessage)
}

// Keep the same cancellation contract as processOneShotEvents: the outer
// manager keeps draining this channel for the authoritative terminal message.
func forwardQuestionCancelled(backend <-chan ChatEvent, out chan<- ChatEvent) {
	for ev := range backend {
		if ev.Type == "done" || ev.Type == "error" {
			out <- ev
		} else {
			ev.rejectAttachmentOwnership()
		}
	}
}
