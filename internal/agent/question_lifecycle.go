package agent

import "sync"

// Callback-driven backends (Claude) feed the same reliable lifecycle path as
// native events. Never send directly to a lossy UI channel from a timer.
type questionLifecycle struct {
	events chan ChatEvent
	done   chan struct{}
	once   sync.Once
}

func (q *questionLifecycle) close() {
	if q != nil {
		q.once.Do(func() { close(q.done) })
	}
}

// Caller holds busyMu. The pointer is pinned to the busy entry's generation.
func (m *Manager) questionLifecycleLocked(agentID string) *questionLifecycle {
	entry, ok := m.busy[agentID]
	if !ok {
		return nil
	}
	if entry.questionLifecycle == nil {
		entry.questionLifecycle = &questionLifecycle{events: make(chan ChatEvent, 64), done: make(chan struct{})}
		m.busy[agentID] = entry
	}
	return entry.questionLifecycle
}
