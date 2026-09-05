package agent

import (
	"strings"
	"testing"
)

func TestGoalTurnText(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		failed, web      bool
	}{
		{name: "silence", body: SlackNoReplyToken},
		{name: "whitespace silence", body: " \n" + SlackNoReplyToken + "\t\n"},
		{name: "ordinary", body: "hello", want: "hello"},
		{name: "discussion suffix", body: SlackNoReplyToken + " means silence", want: SlackNoReplyToken + " means silence"},
		{name: "embedded", body: "Use " + SlackNoReplyToken, want: "Use " + SlackNoReplyToken},
		{name: "quoted", body: "`" + SlackNoReplyToken + "`", want: "`" + SlackNoReplyToken + "`"},
		{name: "code block", body: "```\n" + SlackNoReplyToken + "\n```", want: "```\n" + SlackNoReplyToken + "\n```"},
		{name: "multiline prose", body: "before\n\n" + SlackNoReplyToken + "\n\nafter", want: "before\n\n" + SlackNoReplyToken + "\n\nafter"},
		{name: "clean partial", body: "[[NO_", want: "[[NO_"},
		{name: "failed partial", body: "[[NO_", failed: true},
		{name: "failed silence", body: SlackNoReplyToken, failed: true},
		{name: "failed prose", body: "partial reply", want: "partial reply", failed: true},
		{name: "web literal", body: SlackNoReplyToken, want: SlackNoReplyToken, web: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every byte boundary, including a fully completed-item fallback.
			for split := 0; split <= len(tc.body); split++ {
				for _, separator := range []bool{false, true} {
					var live strings.Builder
					f := newGoalTurnText(!tc.web, separator, func(e ChatEvent) bool {
						if e.Type == "text" {
							live.WriteString(e.Delta)
						}
						return true
					})
					res := &codexStreamResult{turnCompleted: true, turnStatus: "completed"}
					if tc.failed {
						res.processError = "failure"
						res.turnStatus = "failed"
					}
					for _, delta := range []string{tc.body[:split], tc.body[split:]} {
						res.fullText.WriteString(delta)
						f.send(ChatEvent{Type: "text", Delta: delta})
						if tc.want == "" && live.Len() != 0 {
							t.Fatalf("token leaked before finish: %q", live.String())
						}
					}
					f.finish(res)
					want := tc.want
					if separator && want != "" {
						want = "\n\n" + want
					}
					if live.String() != want || res.fullText.String() != want {
						t.Fatalf("split=%d separator=%v live=%q terminal=%q want=%q", split, separator, live.String(), res.fullText.String(), want)
					}
					if tc.failed && res.processError != "failure" {
						t.Fatal("failure lost")
					}
				}
			}
		})
	}
}

func TestGoalTurnTextPreservesNonTextAndCancellation(t *testing.T) {
	var events []ChatEvent
	f := newGoalTurnText(true, false, func(e ChatEvent) bool {
		events = append(events, e)
		return e.Type != "text"
	})
	f.send(ChatEvent{Type: "text", Delta: "[["})
	for _, kind := range []string{"thinking", "tool_use", "tool_result", "user_question", "error"} {
		if !f.send(ChatEvent{Type: kind}) {
			t.Fatal("non-text event was not forwarded")
		}
	}
	res := &codexStreamResult{turnCompleted: true, turnStatus: "completed"}
	res.fullText.WriteString("[[")
	f.finish(res)
	if !res.cancelled || len(events) != 6 {
		t.Fatalf("cancelled=%v events=%+v", res.cancelled, events)
	}
}
