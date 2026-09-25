package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Keyed lingering sessions (Slack thread turns).
//
// A keyed turn normally runs like the legacy per-turn spawn: the CLI process
// exits right after the turn's result. The difference is only visible when the
// CLI reports still-running run_in_background tasks
// (system/background_tasks_changed with a non-empty list) at the result: the
// process is then kept alive so the CLI can deliver the task-notification turn
// instead of killing the tasks at stdin EOF. That later turn is routed to the
// Manager's keyed background handler (never the main transcript). The process
// lives only while tasks are pending (+keyedLingerGrace once they drain) or a
// turn is active, bounded by keyedLingerMax overall.

var (
	// keyedLingerGrace is how long an idle keyed session with no pending
	// tasks stays up after having lingered (a notification turn usually
	// follows the tasks=[] snapshot within a second).
	keyedLingerGrace = 60 * time.Second
	// keyedLingerMax is the hard cap on how long a keyed session may linger
	// waiting for background tasks.
	keyedLingerMax = 2 * time.Hour
	// keyedCloseGrace bounds how long a closing keyed session with nothing
	// pending may take to exit on its own after stdin EOF before it is
	// force-cancelled (today's per-turn path waits for a natural exit).
	keyedCloseGrace = 30 * time.Second
	// maxLingeringSessionsPerAgent caps how many keyed sessions of one agent
	// may linger at once waiting for pending background tasks. Only lingering
	// is capped: every keyed turn spawns lingering-capable and a normal turn
	// is never blocked by the cap. A turn that ends with tasks pending while
	// the cap is full closes (killing the tasks) and the response surface is
	// told why via the abandoned notice (see lingerCapReason).
	maxLingeringSessionsPerAgent = 50
)

// lingerCapReason is the close reason of a session whose tasks could not be
// continued because the per-agent linger cap was full.
func lingerCapReason() string {
	return fmt.Sprintf("同時待機上限(%dスレッド)に達していたため継続できませんでした", maxLingeringSessionsPerAgent)
}

// keyedSpawn carries the spawn-time inputs of a keyed session.
type keyedSpawn struct {
	sessionKey string
	opts       ChatOptions
}

// keyedPoolKey is the ClaudeBackend.sessions key of a keyed session. The NUL
// separator cannot appear in an agent ID, so it never collides with a
// main-chat entry (keyed by the bare agent ID).
func keyedPoolKey(agentID, sessionKey string) string {
	return agentID + "\x00" + sessionKey
}

// pk returns the session's pool key (agent ID for sessions built without one).
func (s *claudeSession) pk() string {
	if s.poolKey == "" {
		return s.agentID
	}
	return s.poolKey
}

// cancelErrMessage maps a cancelled turn context to the same marker the
// per-turn path's emitCancelDone uses.
func cancelErrMessage(ctx context.Context) string {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ErrMsgCancelled
	}
	return ErrMsgTimeout
}

// backgroundHandler returns the sink consumer for an unsolicited turn on this
// session, or nil when none is registered (the turn is then dropped). Keyed
// sessions route ONLY to the keyed handler so a Slack-thread notification
// never reaches the main transcript or the busy slot.
// steer (keyed only, nil otherwise) injects a user line into the running
// notification turn.
func (s *claudeSession) backgroundHandler() func(<-chan ChatEvent, AnswerFunc, func(), SteerFunc) {
	if s.keyed {
		fn := s.b.onKeyedBackgroundTurn
		if fn == nil {
			return nil
		}
		agentID, key := s.agentID, s.sessionKey
		return func(events <-chan ChatEvent, answer AnswerFunc, abort func(), steer SteerFunc) {
			fn(agentID, key, events, answer, abort, steer)
		}
	}
	fn := s.b.onBackgroundTurn
	if fn == nil {
		return nil
	}
	agentID := s.agentID
	return func(events <-chan ChatEvent, answer AnswerFunc, abort func(), _ SteerFunc) {
		fn(agentID, events, answer, abort)
	}
}

// onBackgroundTasksChanged records a background_tasks_changed snapshot
// (REPLACE semantics). While idle it also drives the linger lifetime: tasks
// drained → start the grace; tasks pending again → cancel it.
func (s *claudeSession) onBackgroundTasksChanged(tasks []claudeBackgroundTask) {
	if !s.keyed {
		return
	}
	n := len(tasks)
	s.mu.Lock()
	s.pendingTasks = n
	s.recordTasksLocked(tasks)
	if n == 0 {
		// Nothing pending: the session no longer occupies a linger slot.
		s.releaseLingerSlotLocked()
	}
	if s.state != sessIdle || s.closing {
		s.mu.Unlock()
		return
	}
	if n == 0 {
		s.armGraceLocked()
		s.mu.Unlock()
		return
	}
	if !s.acquireLingerSlotLocked() {
		// Tasks reappeared on an idle session that had released its slot and
		// the cap is full meanwhile: it cannot keep waiting.
		if s.closeReason == "" {
			s.closeReason = lingerCapReason()
		}
		s.setClosingLocked()
		s.mu.Unlock()
		s.closeKeyed("")
		return
	}
	s.stopGraceLocked()
	s.armLingerMaxLocked()
	s.mu.Unlock()
}

// recordTasksLocked stores the latest task snapshot for the background-session
// API, keeping each task's first-seen time across REPLACE snapshots. Caller
// holds mu.
func (s *claudeSession) recordTasksLocked(tasks []claudeBackgroundTask) {
	now := time.Now()
	seen := make(map[string]time.Time, len(tasks))
	for _, t := range tasks {
		if at, ok := s.taskSeen[t.TaskID]; ok {
			seen[t.TaskID] = at
		} else {
			seen[t.TaskID] = now
		}
	}
	s.taskSeen = seen
	s.tasks = append(s.tasks[:0:0], tasks...)
}

// setClosingLocked marks the session closing and frees its linger slot: a
// closing session is exiting and must not keep another thread from lingering.
// The pool entry itself stays until onEOF (same-key respawn waits for the exit,
// see closeKeyed), so the slot and the pool entry are deliberately decoupled.
// Caller holds mu.
func (s *claudeSession) setClosingLocked() {
	if !s.closing && s.pendingTasks > s.pendingAtClose {
		// Snapshot what is being cut off: the CLI may report tasks=[] while
		// shutting down, which would otherwise hide the abandoned tasks.
		s.pendingAtClose = s.pendingTasks
	}
	s.closing = true
	s.releaseLingerSlotLocked()
}

// acquireLingerSlotLocked reserves one of the agent's linger slots for this
// session (idempotent). Returns false when the cap is full. Caller holds mu
// (lock order: s.mu → b.lingerMu).
func (s *claudeSession) acquireLingerSlotLocked() bool {
	if s.lingerSlot {
		return true
	}
	if s.b == nil || !s.b.tryAcquireLingerSlot(s) {
		return false
	}
	s.lingerSlot = true
	s.lingerSince = time.Now()
	return true
}

// releaseLingerSlotLocked frees the session's linger slot, if held. Caller holds mu.
func (s *claudeSession) releaseLingerSlotLocked() {
	if !s.lingerSlot {
		return
	}
	s.lingerSlot = false
	s.lingerSince = time.Time{}
	if s.b != nil {
		s.b.releaseLingerSlot(s)
	}
}

func (b *ClaudeBackend) tryAcquireLingerSlot(s *claudeSession) bool {
	b.lingerMu.Lock()
	defer b.lingerMu.Unlock()
	set := b.lingerSlots[s.agentID]
	if _, ok := set[s]; ok {
		return true
	}
	if len(set) >= maxLingeringSessionsPerAgent {
		return false
	}
	if set == nil {
		if b.lingerSlots == nil {
			b.lingerSlots = make(map[string]map[*claudeSession]struct{})
		}
		set = make(map[*claudeSession]struct{})
		b.lingerSlots[s.agentID] = set
	}
	set[s] = struct{}{}
	return true
}

func (b *ClaudeBackend) releaseLingerSlot(s *claudeSession) {
	b.lingerMu.Lock()
	defer b.lingerMu.Unlock()
	set := b.lingerSlots[s.agentID]
	delete(set, s)
	if len(set) == 0 {
		delete(b.lingerSlots, s.agentID)
	}
}

// lingeringCount returns how many of the agent's keyed sessions currently hold
// a linger slot, excluding the session for excludeKey (if non-empty).
func (b *ClaudeBackend) lingeringCount(agentID, excludeKey string) int {
	b.lingerMu.Lock()
	defer b.lingerMu.Unlock()
	n := 0
	for s := range b.lingerSlots[agentID] {
		if excludeKey != "" && s.sessionKey == excludeKey {
			continue
		}
		n++
	}
	return n
}

// afterKeyedTurnLocked decides a keyed session's fate at a turn's result.
// Returns the pending task count to advertise on the done event (0 when not
// lingering) and whether the session must be closed now. Caller holds mu.
func (s *claudeSession) afterKeyedTurnLocked(unsolicited bool) (pending int, closeNow bool) {
	if s.closing {
		return 0, false
	}
	if s.lingerExpired {
		if s.closeReason == "" {
			s.closeReason = "待機上限(2時間)に到達"
		}
		s.setClosingLocked()
		return 0, true
	}
	if s.pendingTasks > 0 {
		if !s.acquireLingerSlotLocked() {
			// The per-agent linger cap is full. The turn itself already ran
			// normally; only continuing its background tasks is refused. The
			// close reports them via the abandoned notice (surface + agent).
			if s.closeReason == "" {
				s.closeReason = lingerCapReason()
			}
			s.logger.Warn("keyed claude session: linger cap full; closing with background tasks pending",
				"agent", s.agentID, "sessionKey", s.sessionKey, "pending", s.pendingTasks, "cap", maxLingeringSessionsPerAgent)
			s.setClosingLocked()
			return 0, true
		}
		s.stopGraceLocked()
		s.armLingerMaxLocked()
		return s.pendingTasks, false
	}
	// Nothing pending. A plain solicited turn on a session that never
	// lingered closes right away — exactly today's close-at-result. After a
	// notification turn (or once the session has lingered) keep a short
	// grace so a follow-up notification or a steered user line can land.
	if unsolicited || s.lingerTimer != nil {
		s.armGraceLocked()
		return 0, false
	}
	// Claim the close under the same lock that exposes sessIdle so a racing
	// same-key startTurn sees closing instead of being killed mid-turn.
	s.setClosingLocked()
	return 0, true
}

func (s *claudeSession) armGraceLocked() {
	s.stopGraceLocked()
	gen := s.graceGen
	s.graceTimer = time.AfterFunc(keyedLingerGrace, func() {
		s.mu.Lock()
		if gen != s.graceGen || s.state != sessIdle || s.pendingTasks > 0 || s.closing {
			s.mu.Unlock()
			return
		}
		// Claim the close under the same lock as the idle check so a racing
		// startTurn either wins (state=inTurn first) or sees closing.
		s.setClosingLocked()
		s.graceTimer = nil
		s.mu.Unlock()
		s.logger.Info("keyed claude session: linger grace elapsed; closing", "agent", s.agentID, "sessionKey", s.sessionKey)
		s.closeKeyed("")
	})
}

func (s *claudeSession) stopGraceLocked() {
	// Invalidate any fired-but-blocked callback of the previous timer.
	s.graceGen++
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
}

func (s *claudeSession) armLingerMaxLocked() {
	if s.lingerTimer != nil {
		return
	}
	s.logger.Info("keyed claude session lingering for background tasks", "agent", s.agentID, "sessionKey", s.sessionKey, "pending", s.pendingTasks)
	s.lingerTimer = time.AfterFunc(keyedLingerMax, func() {
		s.mu.Lock()
		if s.state == sessDead || s.closing {
			s.mu.Unlock()
			return
		}
		if s.state == sessInTurn {
			// Let the running turn finish; completeTurn closes.
			s.lingerExpired = true
			s.mu.Unlock()
			return
		}
		s.setClosingLocked()
		if s.closeReason == "" {
			s.closeReason = "待機上限(2時間)に到達"
		}
		s.mu.Unlock()
		s.logger.Warn("keyed claude session: linger limit reached; closing", "agent", s.agentID, "sessionKey", s.sessionKey)
		s.closeKeyed("")
	})
}

func (s *claudeSession) stopLingerTimersLocked() {
	s.stopGraceLocked()
	if s.lingerTimer != nil {
		// The field stays non-nil: a closing/dead session never lingers again.
		s.lingerTimer.Stop()
	}
}

// markCloseReason records why the session is being closed (first wins).
func (s *claudeSession) markCloseReason(reason string) {
	if reason == "" {
		return
	}
	s.mu.Lock()
	if s.closeReason == "" {
		s.closeReason = reason
	}
	s.mu.Unlock()
}

// closeKeyed closes the keyed session. The entry deliberately stays in the
// pool (marked closing) until onEOF evicts it, so a follow-up turn for the
// same key finds it, waits for the exit, and only then respawns on the same
// deterministic session id (no "session in use" collision during the grace).
func (s *claudeSession) closeKeyed(reason string) {
	s.markCloseReason(reason)
	s.close()
}

// closingOrDead reports whether the session can no longer take a turn.
func (s *claudeSession) closingOrDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == sessDead || s.closing
}

// steerIntoUnsolicited delivers a user message that raced a keyed session's
// notification turn: the line is written onto the live stdin (the CLI folds it
// into the running turn or queues it as the next one, which again opens as an
// unsolicited turn routed to the keyed handler). The caller gets a turn that
// immediately completes with the Slack no-reply token, so it posts nothing and
// releases the thread; the answer arrives through the background handler.
func (s *claudeSession) steerIntoUnsolicited(userMessage string) (<-chan ChatEvent, bool) {
	s.mu.Lock()
	if s.state != sessInTurn || !s.unsolicited || s.closing || s.turnSteer == nil {
		s.mu.Unlock()
		return nil, false
	}
	ts := s.turnSteer
	s.mu.Unlock()
	if err := ts.writeUserLine(userMessage); err != nil {
		return nil, false
	}
	ch := make(chan ChatEvent, 1)
	// SteeredIntoBackground tells surfaces without a NO_REPLY filter (WebUI
	// threads) to post nothing for this turn.
	ch <- ChatEvent{Type: "done", Message: assembleAssistantMessage(SlackNoReplyToken, "", nil, nil), SteeredIntoBackground: true}
	close(ch)
	return ch, true
}

// awaitKeyedExit waits for a closing keyed session to exit (so a respawn on
// the same deterministic session id is not rejected as "in use"), escalating
// to a group kill past the close grace.
func awaitKeyedExit(s *claudeSession) {
	s.awaitDead(keyedCloseGrace + 2*time.Second)
	if !s.isDead() {
		s.forceKill()
		s.awaitDead(3 * time.Second)
	}
}

// chatViaKeyedSession runs a keyed turn on a lingering-capable process.
// handled=false means the caller must fall back to the per-turn spawn path
// (the pool kept racing).
func (b *ClaudeBackend) chatViaKeyedSession(ctx context.Context, agent *Agent, userMessage, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, bool, error) {
	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, true, err
	}
	if err := ensureClaudeProjectDir(dir); err != nil {
		return nil, true, fmt.Errorf("prepare claude project dir: %w", err)
	}
	pk := keyedPoolKey(agent.ID, opts.SessionKey)
	canAnswer := opts.OnQuestionReady != nil

	for attempt := 0; attempt < 3; attempt++ {
		b.sessMu.Lock()
		sess := b.sessions[pk]
		if sess != nil && sess.closingOrDead() {
			// Leave the entry pooled until its onEOF evicts it (a respawn on the
			// same deterministic session id would be rejected as "in use");
			// just wait for the exit and re-evaluate.
			b.sessMu.Unlock()
			awaitKeyedExit(sess)
			b.sessMu.Lock()
			if b.sessions[pk] == sess && sess.isDead() {
				delete(b.sessions, pk)
			}
			b.sessMu.Unlock()
			continue
		}
		b.sessMu.Unlock()

		inv := b.buildClaudeInvocation(agent, systemPrompt, dir, opts.OneShot, opts.MCPServers, opts.AutomatedTrigger, opts.SessionKey)

		b.sessMu.Lock()
		if cur := b.sessions[pk]; cur != sess {
			b.sessMu.Unlock()
			continue // raced another spawn/close; re-evaluate
		}
		if sess != nil && inv.sessionWasReset {
			// Context-threshold reset: the live process still holds the
			// context the reset drops. The fingerprint is deliberately NOT a
			// respawn reason for keyed sessions (the Slack speaker line in the
			// system prompt changes per message); a keyed process only exists
			// while its background tasks are pending, and respawning would
			// kill them.
			delete(b.sessions, pk)
			b.sessMu.Unlock()
			sess.markCloseReason("コンテキスト上限によるセッションリセット")
			closeSessionsSync([]*claudeSession{sess})
			removeClaudeSession(agent.ID, opts.SessionKey)
			continue
		}
		spawned := false
		if sess == nil {
			// No spawn-time cap: a normal turn is never blocked or degraded.
			// Only lingering past the result is capped (afterKeyedTurnLocked).
			var err error
			sess, err = b.spawnSession(agent.ID, dir, fingerprintArgs(inv.args), inv.args, &keyedSpawn{sessionKey: opts.SessionKey, opts: opts})
			if err != nil {
				b.sessMu.Unlock()
				return nil, true, err
			}
			b.sessions[pk] = sess
			spawned = true
		}
		b.sessMu.Unlock()

		// Same context-injection rule as the per-turn path for a fresh
		// process; a reused live process already holds the thread context.
		fresh := spawned && !opts.OneShot && inv.bootstrapRecentContext
		msg := injectSessionHistoryContext(userMessage, opts.FreshSessionContext, opts.ResumeSessionContext, !opts.OneShot && !fresh)
		if fresh && opts.FreshSessionContext == "" && opts.RecentMessagesContext != "" {
			msg = injectRecentMessagesContext(msg, opts.RecentMessagesContext)
		}

		ch, err := sess.startTurn(ctx, agent, msg, canAnswer, opts.AutomatedTrigger)
		if err == nil {
			if opts.OnSteerReady != nil {
				opts.OnSteerReady(sess.turnSteerFunc())
			}
			sess.qstate.setOnResolved(opts.OnQuestionResolved)
			if canAnswer {
				opts.OnQuestionReady(sess.qstate.answer)
			}
			return ch, true, nil
		}
		if errors.Is(err, ErrAgentBusy) {
			// A notification turn is running on this thread's process.
			if sch, ok := sess.steerIntoUnsolicited(msg); ok {
				return sch, true, nil
			}
			return nil, true, err
		}
		// Closing/dead or a write failure: drop it and retry with a fresh
		// process (a failed write on a just-spawned process is terminal).
		sess.mu.Lock()
		sess.setClosingLocked()
		sess.mu.Unlock()
		sess.closeKeyed("")
		if spawned {
			return nil, true, err
		}
		awaitKeyedExit(sess)
	}
	return nil, false, nil
}

// CloseKeyedSessionSync closes the keyed lingering session for
// (agentID, sessionKey), if any, and waits (bounded) for it to exit. Used by
// ForceFreshSession before the key's session files are discarded.
func (b *ClaudeBackend) CloseKeyedSessionSync(agentID, sessionKey, reason string) {
	if sessionKey == "" {
		return
	}
	pk := keyedPoolKey(agentID, sessionKey)
	b.sessMu.Lock()
	sess := b.sessions[pk]
	delete(b.sessions, pk)
	b.sessMu.Unlock()
	if sess == nil {
		return
	}
	sess.markCloseReason(reason)
	closeSessionsSync([]*claudeSession{sess})
}

// HasKeyedSession reports whether a keyed lingering session is live for the key.
func (b *ClaudeBackend) HasKeyedSession(agentID, sessionKey string) bool {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	_, ok := b.sessions[keyedPoolKey(agentID, sessionKey)]
	return ok
}
