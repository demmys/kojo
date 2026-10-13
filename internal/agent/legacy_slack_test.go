package agent

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
)

// setLegacySlackBot plants built-in Slack settings on a local agent the
// way an older kojo left them. Nothing in the product can do this any
// more, so the test reaches into the manager.
func setLegacySlackBot(t *testing.T, m *Manager, id string, cfg SlackBotConfig) {
	t.Helper()
	m.mu.Lock()
	a, ok := m.agents[id]
	if ok {
		a.SlackBot = &cfg
	}
	m.mu.Unlock()
	if !ok {
		t.Fatalf("agent %s is not local", id)
	}
	m.save()
}

func TestLegacySlackBotsCoversLocalAndRemoteAgents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	local, err := m.Create(AgentConfig{Name: "local"})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := m.Create(AgentConfig{Name: "remote", Model: "keep-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(AgentConfig{Name: "never-had-slack"}); err != nil {
		t.Fatal(err)
	}
	setLegacySlackBot(t, m, local.ID, SlackBotConfig{Enabled: true})
	setLegacySlackBot(t, m, remote.ID, SlackBotConfig{ThreadReplies: true})
	// The runtime moves to another device; the row stays here.
	m.TeardownAgentRuntime(remote.ID)

	got := map[string]LegacySlackBot{}
	for _, l := range m.LegacySlackBots() {
		got[l.AgentID] = l
	}
	if len(got) != 2 {
		t.Fatalf("legacy bots = %+v, want the two configured agents", got)
	}
	if !got[local.ID].Config.Enabled || !got[remote.ID].Config.ThreadReplies {
		t.Fatalf("settings not carried: %+v", got)
	}
	if want := filepath.Join(AgentDir(local.ID), "chat_history", "slack"); got[local.ID].HistoryDir != want {
		t.Fatalf("history dir = %q, want %q", got[local.ID].HistoryDir, want)
	}

	for _, id := range []string{local.ID, remote.ID} {
		if err := m.ClearLegacySlackBot(id); err != nil {
			t.Fatalf("clear %s: %v", id, err)
		}
		// Repeating it is not an error.
		if err := m.ClearLegacySlackBot(id); err != nil {
			t.Fatalf("clear %s again: %v", id, err)
		}
	}
	if left := m.LegacySlackBots(); len(left) != 0 {
		t.Fatalf("legacy bots after clear = %+v", left)
	}
	// Clearing the remote row edits one field, not the whole mirror.
	if a := m.GetRemote(remote.ID); a == nil || a.Model != "keep-model" {
		t.Fatalf("remote row after clear = %+v", a)
	}
}

func TestClearLegacySlackBotRefusedDuringDeviceSwitch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	a, err := m.Create(AgentConfig{Name: "switching"})
	if err != nil {
		t.Fatal(err)
	}
	setLegacySlackBot(t, m, a.ID, SlackBotConfig{Enabled: true})

	m.busyMu.Lock()
	if m.switching == nil {
		m.switching = map[string]bool{}
	}
	m.switching[a.ID] = true
	m.busyMu.Unlock()

	if err := m.ClearLegacySlackBot(a.ID); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("err = %v, want ErrAgentBusy", err)
	}
	if len(m.LegacySlackBots()) != 1 {
		t.Fatal("settings cleared while the agent was switching devices")
	}
}
