package agent

import (
	"strings"
	"time"
	"unicode/utf8"
)

// attentionReasonMaxRunes caps the operator-facing note attached to a
// raise. The reason renders inline in the dashboard row, so anything
// longer would push the rest of the row off-screen rather than inform.
// Longer input is truncated, not rejected — a page must never fail
// because the agent was verbose.
const attentionReasonMaxRunes = 200

// attentionEntry is one outstanding page: the optional operator-facing
// note plus when it was raised (surfaced as a relative time in the UI).
type attentionEntry struct {
	reason string
	at     time.Time
}

// truncateAttentionReason normalises the agent-supplied note: collapse
// the whole thing to a single line (the UI renders one line; an embedded
// newline would just be dropped mid-render) and cap the length.
func truncateAttentionReason(reason string) string {
	reason = strings.TrimSpace(strings.Join(strings.Fields(reason), " "))
	if utf8.RuneCountInString(reason) <= attentionReasonMaxRunes {
		return reason
	}
	runes := []rune(reason)
	return strings.TrimSpace(string(runes[:attentionReasonMaxRunes])) + "…"
}

// RaiseAttention flags agentID as wanting the operator's eyes and fires
// OnAttentionRaised. Unlike AskUserQuestion this does NOT block the turn:
// the agent keeps running, the dashboard just highlights the row until
// the operator opens the chat.
//
// Every call is a distinct page, so re-raising while a page is already
// outstanding refreshes the reason/timestamp AND re-notifies — the agent
// only calls this deliberately, and a second call means it has something
// new to say.
//
// Returns the entry this call stored — NOT a re-read of the map. A
// concurrent raise may have already overwritten it by the time this
// returns, and echoing the caller's own (reason, at) back is both the
// honest answer and the only way to keep the pair from tearing across
// two generations.
func (m *Manager) RaiseAttention(agentID, reason string) (string, time.Time) {
	reason = truncateAttentionReason(reason)
	now := time.Now()
	m.busyMu.Lock()
	if m.attention == nil {
		m.attention = make(map[string]attentionEntry)
	}
	m.attention[agentID] = attentionEntry{reason: reason, at: now}
	m.busyMu.Unlock()
	if m.OnAttentionRaised != nil {
		// Own goroutine, mirroring OnQuestionRaised: a slow web-push
		// send must not stall the agent's HTTP request.
		go m.OnAttentionRaised(agentID, reason)
	}
	return reason, now
}

// ClearAttention drops any outstanding page for agentID and reports
// whether one was actually cleared. Idempotent — the dashboard clears on
// every chat open, so the common case is "nothing to clear".
func (m *Manager) ClearAttention(agentID string) bool {
	m.busyMu.Lock()
	_, had := m.attention[agentID]
	delete(m.attention, agentID)
	m.busyMu.Unlock()
	return had
}

// AttentionFor returns the outstanding page for agentID, if any.
func (m *Manager) AttentionFor(agentID string) (raised bool, reason string, at time.Time) {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	e, ok := m.attention[agentID]
	if !ok {
		return false, "", time.Time{}
	}
	return true, e.reason, e.at
}

// applyAttention folds the live page state onto a copied Agent for the
// list/read responses. Never call it on an agent that may be Saved — the
// fields are runtime-only (and defensively stripped in store.go).
func (m *Manager) applyAttention(a *Agent) {
	if a == nil {
		return
	}
	raised, reason, at := m.AttentionFor(a.ID)
	a.Attention = raised
	a.AttentionReason = reason
	if raised && !at.IsZero() {
		a.AttentionAt = at.UnixMilli()
	} else {
		a.AttentionAt = 0
	}
}
