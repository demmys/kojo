//go:build windows

package session

import (
	"errors"
	"fmt"
	"time"
)

func init() {
	internalTools["shell"] = true
}

// ShellToolName returns the internal tool name for terminal sessions.
func ShellToolName() string { return "shell" }

// platformInit loads persisted sessions (all marked exited on Windows; no tmux persistence).
func (m *Manager) platformInit() {
	m.loadPersistedSessions()
}

// loadPersistedSessions restores previously saved sessions, all as exited.
func (m *Manager) loadPersistedSessions() {
	infos, err := m.store.Load()
	if err != nil {
		m.logger.Error("failed to load persisted sessions", "err", err)
		return
	}
	m.insertRestoredSessions(infos)
}

// restoreSession creates a Session from persisted info, always exited on Windows.
func (m *Manager) restoreSession(info SessionInfo) *Session {
	s := newRestoredSession(info)
	s.logger = m.logger
	close(s.done)
	return s
}

// platformStartUserTool starts a user-facing tool directly via ConPTY (no tmux on Windows).
func (m *Manager) platformStartUserTool(id, workDir, toolPath string, args []string, cols, rows uint16, envVars []string) (*startResult, error) {
	if len(envVars) > 0 {
		return nil, errors.New("environment variable injection is not supported on Windows (custom API sessions require Unix)")
	}
	cmdLine := buildCmdLine(toolPath, args)
	rwc, cmd, err := startConPTY(cmdLine, workDir, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("failed to start conpty: %w", err)
	}
	return &startResult{
		pty: rwc,
		cmd: cmd,
	}, nil
}

// platformStartInternalTool starts an internal tool (shell) via ConPTY.
func (m *Manager) platformStartInternalTool(id, tool, toolPath, workDir string, args []string, toolSessionID string) (*startResult, error) {
	shell := defaultShell()
	cmdLine := buildCmdLine(shell, nil)
	rwc, cmd, err := startConPTY(cmdLine, workDir, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to start shell: %w", err)
	}
	return &startResult{
		pty: rwc,
		cmd: cmd,
	}, nil
}

// platformStartLoops starts the background goroutines for a session on Windows.
// No drainLoop or tmuxWaitLoop needed.
func (m *Manager) platformStartLoops(s *Session) {
	go m.readLoop(s)
	go m.waitLoop(s)
}

// platformStop stops a session on Windows.
func (m *Manager) platformStop(s *Session, id string) error {
	s.mu.Lock()
	cmd := s.Cmd
	s.mu.Unlock()

	// Also stop any child sessions
	m.stopRunningChildren(id)

	if cmd != nil && cmd.Process != nil {
		_ = sendTermSignal(cmd.Process)
	}

	// Close the PTY to ensure readLoop exits
	s.mu.Lock()
	s.closePTYLocked()
	s.mu.Unlock()

	return nil
}

// platformStopAll kills all running sessions on Windows (no persistence).
func (m *Manager) platformStopAll() {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		s.mu.Lock()
		running := s.Status == StatusRunning
		s.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()

	for _, id := range ids {
		_ = m.Stop(id)
	}
	for _, id := range ids {
		if s, ok := m.Get(id); ok {
			select {
			case <-s.done:
			case <-time.After(shutdownTimeout):
			}
		}
	}
}

// platformCleanupDuplicate cleans up resources when a duplicate child is found.
func (m *Manager) platformCleanupDuplicate(res *startResult) {
	if res.cmd != nil && res.cmd.Process != nil {
		_ = res.cmd.Process.Kill()
		_ = res.cmd.Wait()
	}
	if res.pty != nil {
		res.pty.Close()
	}
}

// platformBuildInternalToolArgs builds the arguments for an internal tool session.
func platformBuildInternalToolArgs(id, tool, workDir string, args []string) (runArgs []string, toolSessionID string) {
	if tool == "shell" {
		return nil, "shell_" + id
	}
	return args, ""
}

// cleanupPipePane is a no-op on Windows (no pipe-pane).
func (s *Session) cleanupPipePane() {}

// platformPrepareRestart is a no-op on Windows (no tmux cleanup).
func (m *Manager) platformPrepareRestart(s *Session) {}

// buildInternalToolRestartArgs builds restart arguments for internal tools (shell).
func buildInternalToolRestartArgs(origArgs []string, toolSessionID string) []string {
	return nil // shell sessions restart from scratch
}

// tmuxRunAction is not available on Windows.
func tmuxRunAction(sessionName, action string) error {
	return errors.New("tmux actions are not supported on Windows")
}
