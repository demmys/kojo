package extpkg

import (
	"errors"
	"os"
	"path/filepath"
)

// Per-agent package data and the agent lifecycle.
//
// A binding (registry row + secrets) and a data directory are separate
// things: ForgetAgent drops the first, the functions here deal with the
// second. They are kept apart because the callers differ — an archived
// agent keeps both, a reset agent keeps its binding but loses its
// transcripts, a deleted agent loses everything.

// chatHistoryDirName is the directory, inside a package's per-agent
// data directory, where a chat-platform package keeps its conversation
// transcripts. It is the same name kojo itself used for the built-in
// Slack bot's cache, and it is the one part of a package's data that
// kojo knows the meaning of: an agent reset clears it.
const chatHistoryDirName = "chat_history"

// SecretSettingKeys lists the settings a package's schema marks as
// secrets (type "string", format "password"). A caller that is about to
// hand a credential to a package uses it to confirm the value will be
// filed in the credential store rather than written to the registry in
// the clear.
func (m *Manager) SecretSettingKeys(id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return m.secretKeys(row), nil
}

// agentDataDirs returns every package's data directory for one agent
// that exists on disk. It walks the data root rather than the registry
// so a directory left by a package that has since been unbound is found
// too.
func (m *Manager) agentDataDirs(agentID string) ([]string, error) {
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(m.root, dataDirName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := m.DataDir(e.Name(), agentID)
		switch _, err := os.Lstat(dir); {
		case err == nil:
			out = append(out, dir)
		case !errors.Is(err, os.ErrNotExist):
			// Unreadable is not absent: reporting it keeps a reset or
			// a delete from passing over data it could not look at.
			return nil, err
		}
	}
	return out, nil
}

// RemoveAgentData deletes everything packages have stored for an agent.
// Called when the agent is deleted, after its services have stopped:
// what is left would otherwise be transcripts of a deleted agent's
// conversations that nothing can reach.
func (m *Manager) RemoveAgentData(agentID string) error {
	dirs, err := m.agentDataDirs(agentID)
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range dirs {
		errs = append(errs, os.RemoveAll(dir))
	}
	return errors.Join(errs...)
}

// ClearAgentChatHistory deletes the conversation transcripts packages
// keep for an agent and leaves the rest of their data alone. Called
// when the agent's data is reset, with its services stopped: the agent
// forgets its conversations, so a package must not feed the old ones
// back to it as context.
func (m *Manager) ClearAgentChatHistory(agentID string) error {
	dirs, err := m.agentDataDirs(agentID)
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range dirs {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, chatHistoryDirName)))
	}
	return errors.Join(errs...)
}
