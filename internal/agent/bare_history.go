package agent

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Bounds for the transcript replayed to a session-less backend. The
// replay is capped three ways — how many rows are read, how many turns
// survive, and how much text each turn may contribute — because
// custom-bare usually points at a local llama-server whose context
// window is a few thousand tokens. Overrunning it does not error
// visibly: the server shifts the prompt window, which eats the head —
// the system prompt, i.e. the persona — first.
//
// The total is a heuristic, not a guarantee: kojo cannot see the
// server's n_ctx, and the system prompt plus the current turn are
// already spending an unknown slice of it. Deliberately budgeted well
// under a small (8K) window rather than tuned for a large one.
const (
	bareHistoryScanMessages    = 60
	bareHistoryMaxTurns        = 20
	bareHistoryMaxRunesPerTurn = 1500
	bareHistoryMaxRunesTotal   = 12000
)

// historyTruncationMarker replaces the middle of an over-long turn. It
// counts against the per-turn budget, so a truncated turn is never
// larger than an untruncated one that just fit.
const historyTruncationMarker = "\n[...truncated...]\n"

// internalNoticePrefixes are the kojo-generated system rows: chat
// failures, the turn-timeout notice, and the aborted-device-switch note
// written through Manager.AppendSystemNote. internal/server's
// agent_ws.go keys off NoticeErrorPrefix for the same reason.
var internalNoticePrefixes = []string{
	NoticeErrorPrefix,
	NoticeTimeoutText,
	NoticeSwitchAbortedPrefix,
}

// Prefixes for the system rows kojo writes itself. Exported so
// internal/server can stamp the same marker instead of hand-rolling a
// string the history replay would not recognise.
const (
	NoticeErrorPrefix         = "\u26a0\ufe0f Error: "
	NoticeTimeoutText         = "\u26a0\ufe0f この応答は制限時間超過により中断されました。"
	NoticeSwitchAbortedPrefix = "\u26a0\ufe0f デバイス移行を中断した: "
)

// HistoryTurn is one prior conversation turn replayed to a backend that
// keeps no session of its own.
type HistoryTurn struct {
	Role    string // "user" or "assistant"
	Content string
}

// backendReplaysHistory reports whether the backend needs kojo to hand
// it the prior conversation. Only custom-bare does: every other backend
// drives a CLI that owns its own session (claude --resume, codex rollout
// files, grok session keys) and would double the context if kojo replayed
// the transcript on top.
func backendReplaysHistory(b ChatBackend) bool {
	if b == nil {
		return false
	}
	return b.Name() == ToolCustomBare
}

// BuildHistoryTurns returns the bounded replay for agentID. Best-effort:
// a failed transcript read degrades to a single-turn chat rather than
// failing the turn.
//
// Call this BEFORE the incoming user message is appended to the
// transcript — the current turn travels as the backend's userMessage
// argument, so a replay that already contains it would send it twice.
func (m *Manager) BuildHistoryTurns(parent context.Context, agentID string) []HistoryTurn {
	return m.BuildHistoryTurnsBefore(parent, agentID, "")
}

// BuildHistoryTurnsBefore is the cursor variant: it replays only what
// precedes beforeMsgID. Regenerate needs it — the message being re-run
// travels as the userMessage argument, and everything after it is about
// to be (or has just been) tombstoned. An empty cursor means "up to the
// end of the transcript".
func (m *Manager) BuildHistoryTurnsBefore(parent context.Context, agentID, beforeMsgID string) []HistoryTurn {
	ctx, cancel := boundedCtx(parent)
	defer cancel()

	msgs, _, err := loadMessagesPaginatedCtx(ctx, agentID, bareHistoryScanMessages, beforeMsgID)
	if err != nil {
		if m != nil && m.logger != nil {
			m.logger.Debug("history replay skipped", "agent", agentID, "err", err)
		}
		return nil
	}
	return buildHistoryTurns(msgs)
}

func buildHistoryTurns(msgs []*Message) []HistoryTurn {
	// Drop the per-turn volatile context (clock, todos, memory hits)
	// that was prepended to each stored user message. It was true for
	// that turn only, it is re-injected fresh for the current one, and
	// it is by far the largest thing in the transcript.
	msgs = stripVolatileContext(msgs)

	turns := make([]HistoryTurn, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		role := msg.Role
		// A system-role row is an automated trigger (cron, group DM
		// relay) that the agent answered like any other prompt. Replay
		// it as a user turn: the OpenAI message schema reserves
		// "system" for the leading instruction block, and dropping the
		// row instead would strand the assistant reply it produced.
		if role == "system" {
			// ...unless it is one of kojo's own notices (backend
			// error, turn timeout, aborted device switch) or a
			// rate-limit reply the manager re-roled out of the
			// assistant lane. Those are operator-facing UI rows, not
			// something the agent was asked to act on, and replaying
			// one would feed a raw error string — endpoint URLs and
			// all — back to the model as if the user had typed it.
			if isInternalNotice(msg.Content) || isRateLimitMessage(msg) {
				continue
			}
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		// Truncate per row, BEFORE the same-role merge below: merging
		// first would materialise the full text of every row in the
		// scan window, and transcript rows have no size ceiling.
		content := truncateHistoryTurn(strings.TrimSpace(msg.Content))
		if content == "" {
			continue
		}
		// Merge consecutive same-role rows. Chat templates for local
		// models generally assume strict user/assistant alternation and
		// render a doubled role badly — or refuse the request outright.
		if n := len(turns); n > 0 && turns[n-1].Role == role {
			turns[n-1].Content += "\n\n" + content
			continue
		}
		turns = append(turns, HistoryTurn{Role: role, Content: content})
	}

	if len(turns) > bareHistoryMaxTurns {
		turns = turns[len(turns)-bareHistoryMaxTurns:]
	}
	for i := range turns {
		turns[i].Content = truncateHistoryTurn(turns[i].Content)
	}
	turns = trimHistoryToBudget(turns)

	// Open on a user turn: a leading assistant message answers nothing
	// and reads to the model as if it had spoken first.
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	if len(turns) == 0 {
		return nil
	}
	return turns
}

// trimHistoryToBudget drops whole turns from the oldest end until the
// replay fits bareHistoryMaxRunesTotal. Dropping whole turns (rather
// than truncating the oldest survivor further) keeps every turn the
// model does see intact and attributable.
func trimHistoryToBudget(turns []HistoryTurn) []HistoryTurn {
	total := 0
	for _, t := range turns {
		total += utf8.RuneCountInString(t.Content)
	}
	for len(turns) > 0 && total > bareHistoryMaxRunesTotal {
		total -= utf8.RuneCountInString(turns[0].Content)
		turns = turns[1:]
	}
	return turns
}

func truncateHistoryTurn(s string) string {
	if utf8.RuneCountInString(s) <= bareHistoryMaxRunesPerTurn {
		return s
	}
	budget := bareHistoryMaxRunesPerTurn - utf8.RuneCountInString(historyTruncationMarker)
	runes := []rune(s)
	head := budget / 2
	tail := budget - head
	return string(runes[:head]) + historyTruncationMarker + string(runes[len(runes)-tail:])
}

// isInternalNotice reports whether a system-role row is a kojo-generated
// notice rather than a prompt the agent was given. The prefixes are the
// only marker such a row carries, so they are declared once here and
// used at every write site; a new notice that skips them will leak into
// the replay as a user turn.
func isInternalNotice(content string) bool {
	content = strings.TrimSpace(content)
	for _, p := range internalNoticePrefixes {
		if strings.HasPrefix(content, p) {
			return true
		}
	}
	return false
}

// foldIntoUserMessage splices older text into userMessage while keeping
// the volatile-context block (and the persona anchor that follows it) at
// the very front. Those blocks are a contract with the model — leading
// reference data, marked as not-instructions — and pushing them into the
// middle of the message would break it.
func foldIntoUserMessage(userMessage, older string) string {
	n := volatileContextPrefixLen(userMessage)
	return userMessage[:n] + older + "\n\n" + userMessage[n:]
}

// volatileContextPrefixLen returns the byte length of the kojo-injected
// preamble at the head of a per-turn message: the <context> block plus
// an optional <persona-anchor> block. Zero when the message carries
// neither (a plain turn, or a user message that merely starts with the
// same tag — the sentinel and the anchor header gate that).
func volatileContextPrefixLen(s string) int {
	if !strings.HasPrefix(s, "<context>") {
		return 0
	}
	closeIdx := strings.Index(s, "</context>")
	if closeIdx < 0 || !strings.Contains(s[:closeIdx], volatileContextSentinel) {
		return 0
	}
	n := closeIdx + len("</context>")
	rest := strings.TrimLeft(s[n:], "\r\n")
	n = len(s) - len(rest)

	if !strings.HasPrefix(rest, "<persona-anchor>") {
		return n
	}
	anchorClose := strings.Index(rest, "</persona-anchor>")
	if anchorClose < 0 || !strings.Contains(rest[:anchorClose], personaAnchorHeader) {
		return n
	}
	after := strings.TrimLeft(rest[anchorClose+len("</persona-anchor>"):], "\r\n")
	return len(s) - len(after)
}
