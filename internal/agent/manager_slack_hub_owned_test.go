package agent

import (
	"log/slog"
	"testing"
)

func TestUpdateSlackBotAlreadyGuardedUpdatesRemoteMirror(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	a, err := m.Create(AgentConfig{Name: "remote", Model: "keep-model"})
	if err != nil {
		t.Fatal(err)
	}
	m.TeardownAgentRuntime(a.ID)
	if _, ok := m.Get(a.ID); ok {
		t.Fatal("agent still local after teardown")
	}

	cfg := &SlackBotConfig{Enabled: true, ThreadReplies: true}
	releaseMutation, err := m.AcquireMutation(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := m.LockPatch(a.ID)
	err = m.UpdateSlackBotAlreadyGuarded(a.ID, cfg)
	release()
	releaseMutation()
	if err != nil {
		t.Fatal(err)
	}
	got := m.GetRemote(a.ID)
	if got == nil || got.SlackBot == nil || !got.SlackBot.Enabled || !got.SlackBot.ThreadReplies {
		t.Fatalf("remote Slack config = %#v", got)
	}
	if got.Model != "keep-model" {
		t.Fatalf("unrelated model = %q, want keep-model", got.Model)
	}

	releaseMutation, err = m.AcquireMutation(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	release = m.LockPatch(a.ID)
	err = m.UpdateSlackBotAlreadyGuarded(a.ID, nil)
	release()
	releaseMutation()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.GetRemote(a.ID); got == nil || got.SlackBot != nil {
		t.Fatalf("remote Slack config after delete = %#v", got)
	}
}
