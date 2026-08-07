package agent

import (
	"context"
	"strings"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// BuildSessionHistoryContext converts the WebUI main transcript to the same
// fresh-session fallback used by Slack and WebUI thread turns. A backend only
// injects it when it selects its native fresh-session path.
func (m *Manager) BuildSessionHistoryContext(parent context.Context, agentID string) string {
	return m.buildSessionHistoryContext(parent, agentID, "")
}

func (m *Manager) buildSessionHistoryContext(parent context.Context, agentID, excludeMessageID string) string {
	ctx, cancel := boundedCtx(parent)
	defer cancel()

	limit := chathistory.DefaultMaxMessages
	if excludeMessageID != "" {
		limit++ // keep a full 100-message fallback after excluding the live turn
	}
	msgs, err := loadMessagesCtx(ctx, agentID, limit)
	if err != nil {
		if m != nil && m.logger != nil {
			m.logger.Debug("session history context skipped", "agent", agentID, "err", err)
		}
		return ""
	}
	history := make([]chathistory.HistoryMessage, 0, len(msgs))
	for _, msg := range stripVolatileContext(msgs) {
		if msg == nil || msg.ID == excludeMessageID ||
			(msg.Role != "user" && msg.Role != "assistant") || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		isAssistant := msg.Role == "assistant"
		userID, userName := "user", "User"
		if isAssistant {
			userID, userName = agentID, "Agent"
		}
		history = append(history, chathistory.HistoryMessage{
			Platform: "kojo", MessageID: msg.ID, UserID: userID,
			UserName: userName, Text: msg.Content, Timestamp: msg.Timestamp,
			IsBot: isAssistant,
		})
	}
	return formatSessionHistoryContext(history, agentID)
}

func formatSessionHistoryContext(history []chathistory.HistoryMessage, selfUserID string) string {
	if len(history) == 0 {
		return ""
	}
	ctx := chathistory.FormatForInjection(history, selfUserID,
		chathistory.DefaultMaxMessages, chathistory.DefaultMaxChars)
	if ctx == "" {
		return ""
	}
	return finishSessionHistoryContext(ctx)
}

func finishSessionHistoryContext(ctx string) string {
	if ctx == "" {
		return ""
	}
	// The transcript is inserted inside Kojo's volatile <context> block.
	// Do not let an old user-authored message close that framing early and
	// turn historical data into apparent top-level instructions.
	ctx = strings.ReplaceAll(ctx, "</context>", "&lt;/context&gt;")
	return ctx + "\n---\n\n"
}

// formatResumeSessionContext returns a deliberately overlapping head+tail
// recap. It is not a delivery delta: no-reply/empty/error turns intentionally
// leave no canonical assistant row, so a self-reply-derived cursor would
// replay an arbitrary suffix. The bounded recap is labelled as history and
// excludes the live request, preserving the pre-refactor safety behavior
// without adding a second persistent delivery-cursor protocol.
func formatResumeSessionContext(history []chathistory.HistoryMessage, selfUserID string) string {
	return finishSessionHistoryContext(chathistory.FormatForInjectionHeadTail(
		history, selfUserID, chathistory.DefaultHeadCount,
		chathistory.DefaultTailCount, chathistory.DefaultMaxChars))
}

// FormatOneShotHistoryContexts converts a response surface's canonical
// transcript into the two bounded contexts consumed by ChatOneShot backends.
// It is exported for trusted internal transports that must relay continuity
// without sending an unbounded raw transcript across the network.
func FormatOneShotHistoryContexts(history []chathistory.HistoryMessage, selfUserID string) (fresh, resume string) {
	return formatSessionHistoryContext(history, selfUserID),
		formatResumeSessionContext(history, selfUserID)
}

// injectSessionHistoryContext enforces the shared continuity rule for every
// response surface and backend: a fresh session gets the canonical transcript,
// while a resumed external session gets a small, explicitly historical recap.
func injectSessionHistoryContext(userMessage, freshContext, resumeContext string, resumed bool) string {
	historyContext := freshContext
	if resumed {
		historyContext = resumeContext
	}
	if historyContext == "" {
		return userMessage
	}
	if strings.HasPrefix(userMessage, "<context>") {
		closeIdx := strings.Index(userMessage, "</context>")
		if closeIdx > 0 && strings.Contains(userMessage[:closeIdx], volatileContextSentinel) {
			injected := strings.TrimRight(historyContext, "\r\n")
			return userMessage[:closeIdx] + "\n" + injected + "\n" + userMessage[closeIdx:]
		}
	}
	return historyContext + userMessage
}
