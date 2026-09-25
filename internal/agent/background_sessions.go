package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Agent-facing view/control of keyed lingering sessions: the thread
// conversations (Slack threads, WebUI threads) whose CLI process is kept alive
// after a turn because run_in_background tasks are still running.
//
// Served by GET/DELETE /api/v1/agents/{id}/background-sessions[...] (see the
// background-sessions guide).

// KeyedStopRequestedReason is the close reason of a session stopped through
// the background-sessions API. Surfaces render it as an explicit stop.
const KeyedStopRequestedReason = "エージェントの依頼で停止"

var (
	// ErrBackgroundSessionNotFound: no lingering/background session for the key.
	ErrBackgroundSessionNotFound = errors.New("background session not found")
	// ErrBackgroundTaskNotFound: the session has no such pending task.
	ErrBackgroundTaskNotFound = errors.New("background task not found")
)

// BackgroundTaskInfo describes one still-running background task.
type BackgroundTaskInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	// StartedAt is when kojo first saw the task (the CLI reports no start
	// time); ElapsedSeconds is measured from it.
	StartedAt      string `json:"startedAt"`
	ElapsedSeconds int64  `json:"elapsedSeconds"`
}

// BackgroundSessionInfo describes one keyed session with background tasks.
type BackgroundSessionInfo struct {
	SessionKey string `json:"sessionKey"`
	// Surface is "webui_thread", "slack", or "other".
	Surface    string `json:"surface"`
	ThreadID   string `json:"threadId,omitempty"`
	ThreadName string `json:"threadName,omitempty"`
	// State: "lingering" (idle, waiting for tasks), "turn" (a turn is running
	// on the process), "closing".
	State          string               `json:"state"`
	LingeringSince string               `json:"lingeringSince,omitempty"`
	PendingCount   int                  `json:"pendingCount"`
	Tasks          []BackgroundTaskInfo `json:"tasks"`
}

// BackgroundSessionsSnapshot is the list response.
type BackgroundSessionsSnapshot struct {
	Sessions []BackgroundSessionInfo `json:"sessions"`
	// Lingering is how many sessions occupy a linger slot; Cap is the
	// per-agent maximum. A turn that ends with tasks pending while
	// Lingering == Cap cannot keep them running.
	Lingering int `json:"lingering"`
	Cap       int `json:"cap"`
}

// keyedSessionSnapshot is the backend-level view of one keyed session.
type keyedSessionSnapshot struct {
	sessionKey  string
	state       string
	lingerSince time.Time
	pending     int
	tasks       []claudeBackgroundTask
	taskSeen    map[string]time.Time
}

// keyedSessionsWithTasks snapshots the agent's keyed sessions that have
// background tasks pending or hold a linger slot.
func (b *ClaudeBackend) keyedSessionsWithTasks(agentID string) []keyedSessionSnapshot {
	b.sessMu.Lock()
	var sessions []*claudeSession
	for _, s := range b.sessions {
		if s.keyed && s.agentID == agentID {
			sessions = append(sessions, s)
		}
	}
	b.sessMu.Unlock()
	var out []keyedSessionSnapshot
	for _, s := range sessions {
		s.mu.Lock()
		if s.pendingTasks == 0 && !s.lingerSlot {
			s.mu.Unlock()
			continue
		}
		snap := keyedSessionSnapshot{
			sessionKey:  s.sessionKey,
			lingerSince: s.lingerSince,
			pending:     s.pendingTasks,
			tasks:       append([]claudeBackgroundTask(nil), s.tasks...),
			taskSeen:    make(map[string]time.Time, len(s.taskSeen)),
		}
		for k, v := range s.taskSeen {
			snap.taskSeen[k] = v
		}
		switch {
		case s.state == sessDead || s.closing:
			snap.state = "closing"
		case s.state == sessInTurn:
			snap.state = "turn"
		default:
			snap.state = "lingering"
		}
		s.mu.Unlock()
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sessionKey < out[j].sessionKey })
	return out
}

func (b *ClaudeBackend) keyedSession(agentID, sessionKey string) *claudeSession {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	return b.sessions[keyedPoolKey(agentID, sessionKey)]
}

// stopBackgroundSession stops every background task of the keyed session.
// An idle (lingering) session is closed, which kills its tasks; the abandoned
// notice (reason KeyedStopRequestedReason) informs the thread. A session in
// the middle of a turn — possibly the very turn calling this API — is not
// killed: its tasks are stopped via stop_task control requests and
// notify(pending) is called so the caller can post the stop notice itself.
func (b *ClaudeBackend) stopBackgroundSession(agentID, sessionKey string, notify func(pending int)) error {
	s := b.keyedSession(agentID, sessionKey)
	if s == nil {
		return ErrBackgroundSessionNotFound
	}
	s.mu.Lock()
	if s.state == sessDead || s.closing {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	pending := s.pendingTasks
	if s.state == sessInTurn {
		ids := make([]string, 0, len(s.tasks))
		for _, t := range s.tasks {
			ids = append(ids, t.TaskID)
		}
		s.mu.Unlock()
		if len(ids) == 0 {
			return ErrBackgroundSessionNotFound
		}
		for _, id := range ids {
			s.stopTask(id)
		}
		if notify != nil {
			notify(len(ids))
		}
		return nil
	}
	if pending == 0 && !s.lingerSlot {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	s.closeReason = KeyedStopRequestedReason
	s.setClosingLocked()
	s.mu.Unlock()
	s.closeKeyed("")
	return nil
}

// stopBackgroundTask stops one pending task of the keyed session via the CLI's
// stop_task control request. The process keeps running; once nothing is
// pending the normal linger grace closes it.
func (b *ClaudeBackend) stopBackgroundTask(agentID, sessionKey, taskID string) error {
	s := b.keyedSession(agentID, sessionKey)
	if s == nil {
		return ErrBackgroundSessionNotFound
	}
	s.mu.Lock()
	if s.state == sessDead || s.closing {
		s.mu.Unlock()
		return ErrBackgroundSessionNotFound
	}
	found := false
	for _, t := range s.tasks {
		if t.TaskID == taskID {
			found = true
			break
		}
	}
	s.mu.Unlock()
	// The CLI acknowledges unknown task ids with success too, so validate
	// against the latest snapshot to give the caller a real 404.
	if !found {
		return ErrBackgroundTaskNotFound
	}
	s.stopTask(taskID)
	return nil
}

// stopTask writes a stop_task control request (verified against claude
// 2.1.280: the CLI kills the task, emits background_tasks_changed without it
// and a task_notification with status "stopped"; no auto-turn follows).
func (s *claudeSession) stopTask(taskID string) {
	line, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": "kojo-stop-task-" + generateMessageID(),
		"request":    map[string]any{"subtype": "stop_task", "task_id": taskID},
	})
	s.stdinW.mu.Lock()
	if !s.stdinW.closed {
		_, _ = s.stdinW.w.Write(append(line, '\n'))
	}
	s.stdinW.mu.Unlock()
}

func (m *Manager) claudeBackend() *ClaudeBackend {
	cb, _ := m.backends["claude"].(*ClaudeBackend)
	return cb
}

// keyedSessionSurface classifies a session key for the API.
func (m *Manager) keyedSessionSurface(agentID, sessionKey string) (surface, threadID, threadName string) {
	if id, ok := strings.CutPrefix(sessionKey, webUIThreadKeyPrefix); ok {
		name := ""
		if m.groupdms != nil {
			if g, ok := m.groupdms.Get(id); ok && g != nil {
				name = g.Name
			}
		}
		return "webui_thread", id, name
	}
	if rest, ok := strings.CutPrefix(sessionKey, agentID+":slack:"); ok {
		return "slack", rest, ""
	}
	return "other", "", ""
}

// ListBackgroundSessions returns the agent's thread sessions that are still
// running background tasks.
func (m *Manager) ListBackgroundSessions(agentID string) (BackgroundSessionsSnapshot, error) {
	if _, ok := m.Get(agentID); !ok {
		return BackgroundSessionsSnapshot{}, ErrAgentNotFound
	}
	out := BackgroundSessionsSnapshot{Sessions: []BackgroundSessionInfo{}, Cap: maxLingeringSessionsPerAgent}
	cb := m.claudeBackend()
	if cb == nil {
		return out, nil
	}
	now := time.Now()
	for _, snap := range cb.keyedSessionsWithTasks(agentID) {
		info := BackgroundSessionInfo{
			SessionKey:   snap.sessionKey,
			State:        snap.state,
			PendingCount: snap.pending,
			Tasks:        []BackgroundTaskInfo{},
		}
		info.Surface, info.ThreadID, info.ThreadName = m.keyedSessionSurface(agentID, snap.sessionKey)
		if !snap.lingerSince.IsZero() {
			info.LingeringSince = snap.lingerSince.Format(time.RFC3339)
		}
		for _, t := range snap.tasks {
			ti := BackgroundTaskInfo{ID: t.TaskID, Type: t.TaskType, Description: t.Description}
			if at, ok := snap.taskSeen[t.TaskID]; ok {
				ti.StartedAt = at.Format(time.RFC3339)
				ti.ElapsedSeconds = int64(now.Sub(at).Seconds())
			}
			info.Tasks = append(info.Tasks, ti)
		}
		out.Sessions = append(out.Sessions, info)
	}
	out.Lingering = cb.lingeringCount(agentID, "")
	return out, nil
}

// StopBackgroundSession stops all background tasks of one thread session and
// posts a stop notice into that thread.
func (m *Manager) StopBackgroundSession(agentID, sessionKey string) error {
	if _, ok := m.Get(agentID); !ok {
		return ErrAgentNotFound
	}
	cb := m.claudeBackend()
	if cb == nil {
		return ErrBackgroundSessionNotFound
	}
	return cb.stopBackgroundSession(agentID, sessionKey, func(pending int) {
		go m.handleKeyedTasksAbandoned(agentID, sessionKey, pending, KeyedStopRequestedReason)
	})
}

// StopBackgroundTask stops a single background task of a thread session.
func (m *Manager) StopBackgroundTask(agentID, sessionKey, taskID string) error {
	if _, ok := m.Get(agentID); !ok {
		return ErrAgentNotFound
	}
	cb := m.claudeBackend()
	if cb == nil {
		return ErrBackgroundSessionNotFound
	}
	return cb.stopBackgroundTask(agentID, sessionKey, taskID)
}

// keyedTurnNote builds the short context note injected at the start of a
// lingering-capable keyed turn: a pending one-time note for this key, plus how
// many OTHER threads of the agent are still running background tasks (and a
// warning when the linger cap is already full).
func (m *Manager) keyedTurnNote(agentID, sessionKey string, backend ChatBackend) string {
	var parts []string
	if note := m.popKeyedNote(agentID, sessionKey); note != "" {
		parts = append(parts, note)
	}
	cb, ok := backend.(*ClaudeBackend)
	if !ok || cb == nil {
		return strings.Join(parts, "\n")
	}
	if n := cb.lingeringCount(agentID, sessionKey); n > 0 {
		apiBase := ""
		if m.groupdms != nil {
			apiBase = m.groupdms.APIBase()
		}
		line := fmt.Sprintf("[kojo] 他に裏でバックグラウンドタスク実行中のスレッドが%d件あります。一覧: GET %s/api/v1/agents/%s/background-sessions", n, apiBase, agentID)
		if n >= maxLingeringSessionsPerAgent {
			line += fmt.Sprintf("（待機上限%d件に到達: このターンで run_in_background を使うと、ターン終了時に継続できず停止されます）", maxLingeringSessionsPerAgent)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}
