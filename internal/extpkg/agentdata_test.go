package extpkg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeDataFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestAgentDataLifecycle(t *testing.T) {
	m := newManager(t)
	// Two packages and two agents. The directories are made by hand:
	// what is on disk is what counts, with or without a binding.
	transcript := func(pkg, agentID string) string {
		return filepath.Join(m.DataDir(pkg, agentID), "chat_history", "slack", "C1", "1.jsonl")
	}
	other := func(pkg, agentID string) string {
		return filepath.Join(m.DataDir(pkg, agentID), "cursor.json")
	}
	for _, pkg := range []string{"slack", "demo"} {
		for _, agentID := range []string{"ag_1", "ag_2"} {
			writeDataFile(t, transcript(pkg, agentID))
			writeDataFile(t, other(pkg, agentID))
		}
	}

	// A reset drops the transcripts of that agent, in every package,
	// and nothing else.
	if err := m.ClearAgentChatHistory("ag_1"); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"slack", "demo"} {
		if exists(transcript(pkg, "ag_1")) {
			t.Fatalf("%s transcript survived the reset", pkg)
		}
		if !exists(other(pkg, "ag_1")) {
			t.Fatalf("%s lost data that is not a transcript", pkg)
		}
		if !exists(transcript(pkg, "ag_2")) {
			t.Fatalf("%s lost another agent's transcript", pkg)
		}
	}

	// A delete drops everything of that agent.
	if err := m.RemoveAgentData("ag_1"); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"slack", "demo"} {
		if exists(m.DataDir(pkg, "ag_1")) {
			t.Fatalf("%s data dir survived the delete", pkg)
		}
		if !exists(other(pkg, "ag_2")) {
			t.Fatalf("%s lost another agent's data", pkg)
		}
	}

	// Both are no-ops for an agent with nothing stored, and refuse an
	// ID that could climb out of the data root.
	if err := m.RemoveAgentData("ag_none"); err != nil {
		t.Fatal(err)
	}
	if err := m.ClearAgentChatHistory("ag_none"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveAgentData("../../x"); err == nil {
		t.Fatal("unsafe agent id accepted")
	}
}

func TestSetAgentBindingIf(t *testing.T) {
	m := newManager(t)
	install(t, m, serviceManifest(ScopePerAgent), map[string]string{"bin/svc": sleepScript})

	refused := errors.New("refused")
	_, err := m.SetAgentBindingIf("demo", "ag_1", AgentBinding{Enabled: true},
		func(Installed, []string) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the condition's error", err)
	}
	if row, _ := m.Get("demo"); len(row.Agents) != 0 {
		t.Fatalf("binding written despite the refusal: %+v", row.Agents)
	}

	// The condition sees the row as it is now.
	var seen string
	if _, err := m.SetAgentBindingIf("demo", "ag_1", AgentBinding{Enabled: true},
		func(row Installed, _ []string) error { seen = row.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if row, _ := m.Get("demo"); seen != "demo" || !row.Agents["ag_1"].Enabled {
		t.Fatalf("seen = %q, agents = %+v", seen, row.Agents)
	}
}

func TestSecretSettingKeys(t *testing.T) {
	m := newManager(t)
	if _, err := m.SecretSettingKeys("demo"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	install(t, m, `{
  "id": "demo", "name": "Demo", "version": "1.0.0", "scopes": [],
  "contributes": { "settings": { "scope": "per-agent", "schema": "s.json" } }
}`, map[string]string{"s.json": `{"properties":{
  "token": {"type":"string","format":"password"},
  "name": {"type":"string"}
}}`})
	keys, err := m.SecretSettingKeys("demo")
	if err != nil || len(keys) != 1 || keys[0] != "token" {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
}

func TestSupervisorWithAgentStopped(t *testing.T) {
	// fn runs even when nothing is supervised.
	ran := false
	(*Supervisor)(nil).WithAgentStopped("ag_1", func() { ran = true })
	if !ran {
		t.Fatal("fn skipped on a nil supervisor")
	}

	if runtime.GOOS == "windows" {
		t.Skip("shell script service")
	}
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	install(t, m, serviceManifest(ScopePerAgent), map[string]string{"bin/svc": sleepScript})
	sup := NewSupervisor(m, quietLogger())
	defer sup.Shutdown()
	for _, id := range []string{"ag_1", "ag_2"} {
		if _, err := m.SetAgentBinding("demo", id, AgentBinding{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	sup.Reconcile()

	var during []serviceSpec
	sup.WithAgentStopped("ag_1", func() { during = sup.Running() })
	if len(during) != 1 || during[0].AgentID != "ag_2" {
		t.Fatalf("running while stopped = %+v, want only ag_2", during)
	}
	// Still bound, so it is back afterwards.
	if got := sup.Running(); len(got) != 2 {
		t.Fatalf("running afterwards = %+v, want both", got)
	}
}
