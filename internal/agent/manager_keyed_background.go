package agent

import "context"

// KeyedBackgroundHandler is implemented by a response surface (the Slack bot)
// that owns keyed conversations of an agent. It receives unsolicited turns
// produced by a keyed lingering claude session — the completion notification
// of a run_in_background task started by an earlier thread turn.
type KeyedBackgroundHandler interface {
	// HandleKeyedBackgroundTurn must consume events until the channel closes.
	// The stream has the same contract as a ChatOneShot result (deltas, then
	// a single terminal done/error); a done carrying BackgroundTasksPending>0
	// means more tasks are still running on the lingering process.
	// cancel aborts the background turn (e.g. a user !stop).
	HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, cancel func())
	// KeyedBackgroundTasksAbandoned reports that the keyed session exited
	// while `pending` background tasks were still running (best-effort
	// notice; their completion will never arrive).
	KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string)
}

type keyedBgRegistration struct {
	id int64
	h  KeyedBackgroundHandler
}

// RegisterKeyedBackgroundHandler installs h as the agent's keyed background
// handler (replacing any previous one) and returns an unregister func that
// only removes this registration.
func (m *Manager) RegisterKeyedBackgroundHandler(agentID string, h KeyedBackgroundHandler) func() {
	m.keyedBgMu.Lock()
	if m.keyedBgHandlers == nil {
		m.keyedBgHandlers = make(map[string]keyedBgRegistration)
	}
	m.keyedBgSeq++
	id := m.keyedBgSeq
	m.keyedBgHandlers[agentID] = keyedBgRegistration{id: id, h: h}
	m.keyedBgMu.Unlock()
	return func() {
		m.keyedBgMu.Lock()
		if cur, ok := m.keyedBgHandlers[agentID]; ok && cur.id == id {
			delete(m.keyedBgHandlers, agentID)
		}
		m.keyedBgMu.Unlock()
	}
}

func (m *Manager) keyedBackgroundHandler(agentID string) KeyedBackgroundHandler {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	return m.keyedBgHandlers[agentID].h
}

func (m *Manager) hasKeyedBackgroundHandler(agentID string) bool {
	return m.keyedBackgroundHandler(agentID) != nil
}

// handleKeyedBackgroundTurn routes an unsolicited turn from a keyed lingering
// session to the registered response surface. It deliberately never touches
// the main transcript, the busy slot, or the broadcaster: the turn belongs to
// the Slack thread. It is tracked as a one-shot so reset/delete/shutdown
// drains (cancelOneShots/waitOneShotClear) see and can cancel it.
func (m *Manager) handleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, _ AnswerFunc, abort func()) {
	drain := func() {
		for range events {
		}
	}
	h := m.keyedBackgroundHandler(agentID)
	if h == nil {
		m.logger.Info("keyed background turn discarded: no handler", "agent", agentID, "sessionKey", sessionKey)
		drain()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	entryCancel := func() {
		cancel()
		if abort != nil {
			// Async: cancelOneShots callers may hold locks; abort writes to
			// the CLI stdin and is identity-guarded by the session.
			go abort()
		}
	}
	m.busyMu.Lock()
	if m.quiescing || (m.switching != nil && m.switching[agentID]) || m.resetting[agentID] {
		m.busyMu.Unlock()
		cancel()
		m.logger.Info("keyed background turn discarded: agent gated (quiescing/switching/resetting)", "agent", agentID, "sessionKey", sessionKey)
		drain()
		return
	}
	osID := m.trackOneShot(agentID, entryCancel, sessionKey, "", "")
	m.busyMu.Unlock()

	var backendCh <-chan ChatEvent = events
	if isSlackConversationKey(agentID, sessionKey) {
		backendCh = filterSlackNoReplyEvents(backendCh)
	}
	out := make(chan ChatEvent, 64)
	go func() {
		defer close(out)
		defer cancel()
		defer m.untrackOneShot(agentID, osID)
		m.processOneShotEvents(ctx, agentID, backendCh, out, true)
	}()
	h.HandleKeyedBackgroundTurn(agentID, sessionKey, out, entryCancel)
	// The handler contract is to consume until close; drain defensively so
	// the relay goroutine (and the session's readLoop behind it) never wedge.
	for range out {
	}
}

// handleKeyedTasksAbandoned forwards a keyed session's abandoned-tasks notice.
func (m *Manager) handleKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if h := m.keyedBackgroundHandler(agentID); h != nil {
		h.KeyedBackgroundTasksAbandoned(agentID, sessionKey, pending, reason)
	}
}
