package agent

import (
	"context"
	"fmt"
	"strings"
)

// KeyedBackgroundHandler is implemented by a response surface (the Slack bot,
// the GroupDMManager for WebUI threads) that owns keyed conversations of an
// agent. It receives unsolicited turns
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

// isWebUIThreadKey reports whether sessionKey belongs to a WebUI thread room
// (GroupDMManager's "groupdm:<id>" keys).
func isWebUIThreadKey(sessionKey string) bool {
	return strings.HasPrefix(sessionKey, webUIThreadKeyPrefix)
}

const webUIThreadKeyPrefix = "groupdm:"

// keyedBackgroundHandler resolves the response surface owning sessionKey:
// WebUI thread keys belong to the GroupDMManager, everything else to the
// per-agent registered handler (the Slack bot).
func (m *Manager) keyedBackgroundHandler(agentID, sessionKey string) KeyedBackgroundHandler {
	if isWebUIThreadKey(sessionKey) {
		if m.groupdms != nil {
			return m.groupdms
		}
		return nil
	}
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	return m.keyedBgHandlers[agentID].h
}

func (m *Manager) hasKeyedBackgroundHandler(agentID, sessionKey string) bool {
	return m.keyedBackgroundHandler(agentID, sessionKey) != nil
}

type keyedBgSteer struct {
	id int64
	fn SteerFunc
}

// registerKeyedBgSteer exposes a running WebUI-thread background turn to
// SteerOneShot (the thread UI steers while ThreadLive is active). Returns an
// identity-guarded unregister.
func (m *Manager) registerKeyedBgSteer(sessionKey string, fn SteerFunc) func() {
	m.keyedBgMu.Lock()
	if m.keyedBgSteers == nil {
		m.keyedBgSteers = make(map[string]keyedBgSteer)
	}
	m.keyedBgSeq++
	id := m.keyedBgSeq
	m.keyedBgSteers[sessionKey] = keyedBgSteer{id: id, fn: fn}
	m.keyedBgMu.Unlock()
	return func() {
		m.keyedBgMu.Lock()
		if cur, ok := m.keyedBgSteers[sessionKey]; ok && cur.id == id {
			delete(m.keyedBgSteers, sessionKey)
		}
		m.keyedBgMu.Unlock()
	}
}

func (m *Manager) keyedBgSteerFor(sessionKey string) SteerFunc {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	return m.keyedBgSteers[sessionKey].fn
}

// handleKeyedBackgroundTurn routes an unsolicited turn from a keyed lingering
// session to the response surface owning the key. It deliberately never touches
// the main transcript, the busy slot, or the broadcaster: the turn belongs to
// the thread. It is tracked as a one-shot so reset/delete/shutdown
// drains (cancelOneShots/waitOneShotClear) see and can cancel it.
func (m *Manager) handleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan ChatEvent, _ AnswerFunc, abort func(), steer SteerFunc) {
	drain := func() {
		for range events {
		}
	}
	h := m.keyedBackgroundHandler(agentID, sessionKey)
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
	if steer != nil && isWebUIThreadKey(sessionKey) {
		// A WebUI thread message typed while the notification turn streams is
		// steered into it (Slack keeps its FIFO follow-up semantics).
		defer m.registerKeyedBgSteer(sessionKey, steer)()
	}

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

// handleKeyedTasksAbandoned forwards a keyed session's abandoned-tasks notice
// to the owning surface and leaves a one-time note for the agent's next turn
// on that key, so the agent learns its tasks never completed (an explicit
// stop requested through the background-sessions API needs no note).
func (m *Manager) handleKeyedTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if pending > 0 && reason != KeyedStopRequestedReason {
		m.setKeyedNote(agentID, sessionKey, fmt.Sprintf(
			"[kojo] 前回このスレッドのターン終了後も実行中だったバックグラウンドタスク%d件は、完了前に終了しました（%s）。その結果は届きません。必要なら再実行してください。",
			pending, reason))
	}
	if h := m.keyedBackgroundHandler(agentID, sessionKey); h != nil {
		h.KeyedBackgroundTasksAbandoned(agentID, sessionKey, pending, reason)
	}
}

func keyedNoteKey(agentID, sessionKey string) string {
	return agentID + "\x00" + sessionKey
}

func (m *Manager) setKeyedNote(agentID, sessionKey, note string) {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	if m.keyedNotes == nil {
		m.keyedNotes = make(map[string]string)
	}
	m.keyedNotes[keyedNoteKey(agentID, sessionKey)] = note
}

func (m *Manager) popKeyedNote(agentID, sessionKey string) string {
	m.keyedBgMu.Lock()
	defer m.keyedBgMu.Unlock()
	k := keyedNoteKey(agentID, sessionKey)
	note := m.keyedNotes[k]
	delete(m.keyedNotes, k)
	return note
}
