package agent

import "strings"

// SlackNoReplyToken is a complete assistant response requesting no Slack post.
// A mention inside ordinary prose is not a control response.
const SlackNoReplyToken = "[[NO_REPLY]]"

// goalTurnText buffers only a possible control-token prefix for one native
// goal turn. Ordinary text streams immediately once it differs from the token.
// The same decision is applied to the terminal transcript before aggregation,
// so live delivery, persistence, and peer forwarding cannot disagree.
type goalTurnText struct {
	filter    bool
	separator bool
	started   bool
	pending   strings.Builder
	emit      func(ChatEvent) bool
}

func newGoalTurnText(filter, separator bool, emit func(ChatEvent) bool) *goalTurnText {
	return &goalTurnText{filter: filter, separator: separator, emit: emit}
}

func (t *goalTurnText) send(evt ChatEvent) bool {
	if evt.Type != "text" || evt.Delta == "" {
		return t.emit(evt)
	}
	if t.filter && !t.started {
		t.pending.WriteString(evt.Delta)
		if strings.HasPrefix(SlackNoReplyToken, strings.TrimSpace(t.pending.String())) {
			return true
		}
		evt.Delta = t.pending.String()
		t.pending.Reset()
	}
	return t.sendText(evt)
}

func (t *goalTurnText) sendText(evt ChatEvent) bool {
	if !t.started && t.separator {
		evt.Delta = "\n\n" + evt.Delta
	}
	t.started = true
	return t.emit(evt)
}

func (t *goalTurnText) finish(res *codexStreamResult) {
	if t.filter && !t.started {
		body := strings.TrimSpace(res.fullText.String())
		clean := res.turnCompleted && res.turnStatus == "completed" && res.processError == "" && !res.cancelled
		if body == SlackNoReplyToken || (!clean && strings.HasPrefix(SlackNoReplyToken, body)) {
			// Failed/cancelled partial tokens are never useful user content.
			// Keep the failure flags, usage, questions, and tools unchanged.
			res.fullText.Reset()
		} else if t.pending.Len() > 0 {
			if !t.sendText(ChatEvent{Type: "text", Delta: t.pending.String()}) {
				res.cancelled = true
			}
		}
		t.pending.Reset()
	}
	if res.fullText.Len() > 0 && t.separator {
		body := res.fullText.String()
		res.fullText.Reset()
		res.fullText.WriteString("\n\n")
		res.fullText.WriteString(body)
	}
}
