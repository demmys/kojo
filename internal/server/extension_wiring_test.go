package server

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
)

func TestExtensionServiceWanted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mgr, err := agent.NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv := &Server{agents: mgr, logger: slog.Default()}

	create := func(name string) string {
		t.Helper()
		a, err := mgr.Create(agent.AgentConfig{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	local, archived := create("local"), create("archived")
	remote, remoteArchived := create("remote"), create("remote-archived")
	for _, id := range []string{archived, remoteArchived} {
		if err := mgr.Archive(id, "test-passcode"); err != nil {
			t.Fatal(err)
		}
	}
	// The runtime moves to another device; the row stays here.
	for _, id := range []string{remote, remoteArchived} {
		mgr.TeardownAgentRuntime(id)
	}

	cases := []struct {
		name      string
		extension string
		agentID   string
		want      bool
	}{
		{"local agent", "demo", local, true},
		{"local agent, slack", "slack", local, true},
		{"archived agent", "slack", archived, false},
		// Any other package runs where the runtime is, or it would
		// answer twice.
		{"agent held elsewhere", "demo", remote, false},
		// Slack stays on the device it was configured on.
		{"agent held elsewhere, slack", "slack", remote, true},
		{"archived agent held elsewhere, slack", "slack", remoteArchived, false},
		{"unknown agent", "slack", "ag_missing", false},
	}
	for _, c := range cases {
		if got := srv.extensionServiceWanted(c.extension, c.agentID); got != c.want {
			t.Errorf("%s: wanted = %v, want %v", c.name, got, c.want)
		}
	}
}

// A binding lives on the device it was made on, so it stays editable
// after the agent's runtime moves away; an agent that does not exist
// anywhere is still refused.
func TestExtensionBindingAcceptsAgentHeldElsewhere(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mgr, err := agent.NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv := newExtTestServer(t)
	srv.agents = mgr

	repo := newExtTestRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`,"ackScopes":["chat:send"]}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install failed: %s", rr.Body)
	}

	a, err := mgr.Create(agent.AgentConfig{Name: "moved"})
	if err != nil {
		t.Fatal(err)
	}
	mgr.TeardownAgentRuntime(a.ID)

	put := func(agentID string) int {
		return extCall(t, srv.handlePutExtensionAgentBinding, http.MethodPut,
			"/api/v1/extensions/demo/agents/"+agentID, `{"enabled":true}`,
			map[string]string{"id": "demo", "agentId": agentID}).Code
	}
	if code := put(a.ID); code != http.StatusOK {
		t.Fatalf("binding an agent held elsewhere = %d, want 200", code)
	}
	if code := put("ag_missing"); code != http.StatusNotFound {
		t.Fatalf("binding an unknown agent = %d, want 404", code)
	}
}
