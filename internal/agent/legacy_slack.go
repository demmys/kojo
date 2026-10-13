package agent

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// Legacy Slack state
//
// Slack used to be built into kojo: the bot's settings lived on the
// agent record (Agent.SlackBot) and its tokens in the credential store.
// It is now the "slack" extension package, and nothing in this package
// acts on the old field any more. What remains here is the read-and-
// clear surface the one-time migration needs, so the old state can be
// handed to the package and then removed.

// Where the built-in bot kept its tokens in the credential store.
const (
	LegacySlackTokenProvider = "slack"
	LegacySlackTokenSource   = ""
	LegacySlackAppTokenKey   = "app_token"
	LegacySlackBotTokenKey   = "bot_token"
)

// LegacySlackBot is one agent's leftover built-in Slack configuration.
type LegacySlackBot struct {
	AgentID string
	Config  SlackBotConfig
	// HistoryDir is where the built-in bot cached thread transcripts
	// for this agent. It may not exist.
	HistoryDir string
}

// LegacySlackBots lists every agent this device still holds built-in
// Slack settings for, local or held by another device, ordered by ID.
//
// The settings never travelled between devices (the agent-sync receiver
// drops them from the wire), so an agent appears here only on the
// device where the bot was configured. That is what makes this list
// safe to act on without coordinating with peers.
func (m *Manager) LegacySlackBots() []LegacySlackBot {
	if m == nil {
		return nil
	}
	var out []LegacySlackBot
	add := func(a *Agent) {
		if a == nil || a.SlackBot == nil {
			return
		}
		out = append(out, LegacySlackBot{
			AgentID:    a.ID,
			Config:     *a.SlackBot,
			HistoryDir: filepath.Join(agentDir(a.ID), "chat_history", "slack"),
		})
	}
	m.mu.Lock()
	for _, a := range m.agents {
		add(a)
	}
	m.mu.Unlock()
	for _, a := range m.ListRemote() {
		add(a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// ClearLegacySlackBot removes an agent's built-in Slack settings. It is
// the last step of the migration and is safe to repeat.
//
// It takes the mutation guard, so it fails with ErrAgentBusy while the
// agent is switching devices: the settings are then still in place and
// the caller retries later.
func (m *Manager) ClearLegacySlackBot(id string) error {
	releaseMut, err := m.AcquireMutation(id)
	if err != nil {
		return err
	}
	defer releaseMut()
	release := m.LockPatch(id)
	defer release()

	m.mu.Lock()
	a, local := m.agents[id]
	if local {
		if a.SlackBot == nil {
			m.mu.Unlock()
			return nil
		}
		a.SlackBot = nil
		a.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()
	if local {
		m.save()
		return nil
	}
	if m.store == nil {
		return fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	// A remote runtime: edit the one field in the stored mirror. A
	// load-modify-save of the whole row could write back settings the
	// holder has changed since.
	return m.store.UpdateAgentSetting(id, "slackBot", nil, true)
}
