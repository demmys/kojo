package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// Restart wake: POST /api/v1/system/restart {"wake":true} asks kojo to
// auto-trigger one chat turn for an agent after the re-exec, closing the
// agent's autonomous deploy loop (edit → test → build → restart(wake) →
// auto turn → verify version → continue) without a human message.
//
// The marker must survive the exec, so it lives in the kv store
// (machine scope — a restart is a per-daemon event). It is consumed
// at-most-once on boot: the row is deleted BEFORE the chat fires, so a
// crash mid-consume loses the wake rather than duplicating it.
const (
	restartWakeNamespace = "system"
	restartWakeKey       = "restart_wake"
)

// ArmRestartWake persists the restart-wake marker for agentID. Called by
// the restart drain goroutine right before the shutdown trigger fires —
// never earlier, so an aborted drain leaves no marker behind.
func (m *Manager) ArmRestartWake(agentID string) error {
	st := m.Store()
	if st == nil {
		return errors.New("restart wake: store unavailable")
	}
	rec := &store.KVRecord{
		Namespace: restartWakeNamespace,
		Key:       restartWakeKey,
		Value:     agentID,
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	_, err := st.PutKV(context.Background(), rec, store.KVPutOptions{})
	return err
}

// DisarmRestartWake best-effort clears the marker. Called when the
// restart trigger was refused (signal shutdown won the race — a stop,
// not a restart). Failures are logged only: the process is exiting and
// a leftover marker fires one benign turn on the next boot.
func (m *Manager) DisarmRestartWake() {
	st := m.Store()
	if st == nil {
		return
	}
	if err := st.DeleteKV(context.Background(), restartWakeNamespace, restartWakeKey, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.logger.Warn("restart wake: disarm failed", "err", err)
	}
}

// ConsumeRestartWake reads and clears the restart-wake marker, then
// fires one system-role chat turn for the marked agent telling it the
// restart completed. Called once from main after the listeners are up
// (the system prompt embeds the agent API base, which is wired during
// listener setup). All failures are logged, never fatal — a lost wake
// just means the agent waits for the next human message, exactly the
// pre-wake behaviour.
func (m *Manager) ConsumeRestartWake(version string, bootTime time.Time) {
	st := m.Store()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, err := st.GetKV(ctx, restartWakeNamespace, restartWakeKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.logger.Warn("restart wake: marker read failed", "err", err)
		}
		return
	}
	// Only consume markers written BEFORE this process started. A
	// marker armed by THIS process (a fresh restart request racing an
	// absurdly delayed boot consumer) belongs to the NEXT boot —
	// consuming it here would both fire early and strip the wake from
	// the process it was meant for.
	if rec.UpdatedAt > bootTime.UnixMilli() {
		m.logger.Info("restart wake: marker newer than this boot; leaving for the next process")
		return
	}
	agentID := rec.Value
	// Delete FIRST — at-most-once semantics. If the delete fails we do
	// NOT fire: a marker we cannot clear would re-trigger a turn on
	// every subsequent boot. Conditional on the etag we just read so a
	// concurrently rewritten marker is never destroyed unseen.
	if err := st.DeleteKV(ctx, restartWakeNamespace, restartWakeKey, rec.ETag); err != nil {
		m.logger.Warn("restart wake: marker clear failed; skipping wake", "err", err)
		return
	}
	if agentID == "" {
		return
	}
	msg := fmt.Sprintf(
		"[System] kojo restarted; version %s is now running. This turn was auto-triggered by the wake option on your restart request. Verify the deploy and continue your pending work.",
		version)
	// Retry on ErrAgentBusy for a bounded window: a boot-time cron
	// check-in can grab the busy slot in the same seconds this fires,
	// and the wake must not silently vanish over that collision. Any
	// other error (agent gone, archived, backend) aborts immediately.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := m.WakeChat(agentID, msg)
		if err == nil {
			m.logger.Info("restart wake: turn triggered", "agent", agentID, "version", version)
			return
		}
		if !errors.Is(err, ErrAgentBusy) || time.Now().After(deadline) {
			m.logger.Warn("restart wake: chat failed", "agent", agentID, "err", err)
			return
		}
		m.logger.Info("restart wake: agent busy; retrying", "agent", agentID)
		time.Sleep(5 * time.Second)
	}
}

// WakeChat fires an asynchronous system-role chat turn with a
// caller-supplied prompt. Mirrors Checkin's plumbing (timeout from the
// agent's TimeoutMinutes, background event drain, transcript
// persistence via the normal Chat path) but with an arbitrary message
// instead of checkin.md.
func (m *Manager) WakeChat(agentID, message string) error {
	m.mu.Lock()
	a, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}
	if a.Archived {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAgentArchived, agentID)
	}
	timeoutMinutes := a.TimeoutMinutes
	m.mu.Unlock()

	timeout := cronTimeout
	if timeoutMinutes > 0 {
		timeout = time.Duration(timeoutMinutes) * time.Minute
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	events, err := m.Chat(ctx, agentID, message, "system", nil, BusySourceCron)
	if err != nil {
		cancel()
		return err
	}
	go func() {
		defer cancel()
		for range events {
		}
		if ctx.Err() == context.DeadlineExceeded {
			m.logger.Warn("wake chat timed out", "agent", agentID, "timeout", timeout)
		} else {
			m.logger.Info("wake chat completed", "agent", agentID)
		}
	}()
	return nil
}
