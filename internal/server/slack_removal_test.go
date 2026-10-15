package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/extpkg"
	"github.com/loppo-llc/kojo/internal/store"
)

// These tests cover what the agent lifecycle does for chat packages now
// that Slack is one of them rather than built in.

// newPackageLifecycleServer is an agent server with the demo package
// installed and bound to a fresh agent, which also has a transcript and
// one unrelated file in the package's data directory.
func newPackageLifecycleServer(t *testing.T) (srv *Server, agentID, transcript, other string) {
	t.Helper()
	srv = newChunkedSyncTestServer(t)
	mgr, err := extpkg.NewManager(t.TempDir(), "v0.127.0", srv.logger)
	if err != nil {
		t.Fatal(err)
	}
	srv.extensions = mgr
	srv.extSupervisor = extpkg.NewSupervisor(mgr, srv.logger)
	t.Cleanup(srv.extSupervisor.Shutdown)

	repo := newExtTestRepo(t)
	commit := extPreviewCommit(t, srv, repo)
	rr := extCall(t, srv.handleInstallExtension, http.MethodPost, "/api/v1/extensions",
		`{"url":`+jsonString(repo)+`,"commit":`+jsonString(commit)+`,"ackScopes":["chat:send"]}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install failed: %s", rr.Body)
	}
	a, err := srv.agents.Create(agent.AgentConfig{Name: "bound"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SetAgentBinding("demo", a.ID, extpkg.AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dir := mgr.DataDir("demo", a.ID)
	transcript = filepath.Join(dir, "chat_history", "slack", "C1", "1.2.jsonl")
	other = filepath.Join(dir, "cursor.json")
	for _, p := range []string{transcript, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, a.ID, transcript, other
}

func lifecycleCall(h http.HandlerFunc, method, target, agentID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.SetPathValue("id", agentID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// The built-in bot's thread transcripts lived in the agent's own
// directory, so a reset wiped them. A package's live elsewhere and must
// be cleared explicitly, or the reset agent is handed its old threads
// back as context.
func TestResetAgentDataClearsPackageTranscripts(t *testing.T) {
	srv, agentID, transcript, other := newPackageLifecycleServer(t)

	// A reset that fails changes nothing.
	if rr := lifecycleCall(srv.handleResetAgentData, http.MethodPost, "/api/v1/agents/ag_missing/reset", "ag_missing"); rr.Code != http.StatusNotFound {
		t.Fatalf("reset of an unknown agent = %d: %s", rr.Code, rr.Body)
	}
	if !fileExists(transcript) {
		t.Fatal("a failed reset removed a transcript")
	}

	if rr := lifecycleCall(srv.handleResetAgentData, http.MethodPost, "/api/v1/agents/"+agentID+"/reset", agentID); rr.Code != http.StatusOK {
		t.Fatalf("reset = %d: %s", rr.Code, rr.Body)
	}
	if fileExists(transcript) {
		t.Fatal("transcript survived the reset")
	}
	if !fileExists(other) {
		t.Fatal("reset removed package data that is not a transcript")
	}
	// The integration itself is configuration, not memory: it stays.
	pkg, err := srv.extensions.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := pkg.Agents[agentID]; !ok || !b.Enabled {
		t.Fatalf("binding after reset = %+v", pkg.Agents)
	}
}

func TestArchiveKeepsAndDeleteRemovesPackageState(t *testing.T) {
	srv, agentID, transcript, _ := newPackageLifecycleServer(t)

	if rr := lifecycleCall(srv.handleDeleteAgent, http.MethodDelete, "/api/v1/agents/"+agentID+"?archive=true", agentID); rr.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", rr.Code, rr.Body)
	}
	pkg, err := srv.extensions.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pkg.Agents[agentID]; !ok || !fileExists(transcript) {
		t.Fatal("archive dropped package state that unarchive needs back")
	}
	if rr := lifecycleCall(srv.handleUnarchiveAgent, http.MethodPost, "/api/v1/agents/"+agentID+"/unarchive", agentID); rr.Code != http.StatusOK {
		t.Fatalf("unarchive = %d: %s", rr.Code, rr.Body)
	}

	if rr := lifecycleCall(srv.handleDeleteAgent, http.MethodDelete, "/api/v1/agents/"+agentID, agentID); rr.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rr.Code, rr.Body)
	}
	pkg, err = srv.extensions.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pkg.Agents[agentID]; ok {
		t.Fatal("binding survived the delete")
	}
	if fileExists(srv.extensions.DataDir("demo", agentID)) {
		t.Fatal("package data survived the delete")
	}
}

// Nothing in the API can put built-in Slack settings on an agent any
// more: they would sit there until the next start and be "migrated".
func TestAgentAPICannotSetLegacySlackSettings(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	owner := auth.Principal{Role: auth.RoleOwner}
	const slack = `"slackBot":{"enabled":true,"threadReplies":true}`

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name":"made","tool":"claude","cronExpr":"",`+slack+`}`))
	rr := httptest.NewRecorder()
	srv.handleCreateAgent(rr, authedRequest(r, owner))
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body)
	}
	var created agent.Agent
	readJSONResponse(t, rr, &created)

	r = httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+created.ID, strings.NewReader(`{"name":"renamed",`+slack+`}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", created.ID)
	rr = httptest.NewRecorder()
	srv.handleUpdateAgent(rr, authedRequest(r, owner))
	if rr.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "slackBot") {
		t.Fatalf("response echoes Slack settings: %s", rr.Body)
	}

	a, ok := srv.agents.Get(created.ID)
	if !ok || a.Name != "renamed" {
		t.Fatalf("agent after patch = %+v", a)
	}
	if a.SlackBot != nil || len(srv.agents.LegacySlackBots()) != 0 {
		t.Fatalf("the API stored Slack settings: %+v", a.SlackBot)
	}

	// The endpoints that used to write them are gone.
	mux := http.NewServeMux()
	srv.registerRoutes(mux, Config{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		for _, path := range []string{"/slackbot", "/mcp"} {
			req := httptest.NewRequest(method, "/api/v1/agents/"+created.ID+path, nil)
			if _, pattern := mux.Handler(req); pattern != "" {
				t.Errorf("%s %s is still routed (%s)", method, path, pattern)
			}
		}
	}
}

func TestKeepLocalLegacySlackSettings(t *testing.T) {
	local := map[string]any{"enabled": true}
	cases := []struct {
		name     string
		incoming map[string]any
		current  *store.AgentRecord
		want     any
	}{
		{"sender's copy is dropped", map[string]any{"slackBot": map[string]any{"enabled": false}}, nil, nil},
		{"sender's copy is dropped whatever its case", map[string]any{"SlackBot": 1}, &store.AgentRecord{}, nil},
		{"receiver's own copy is kept", map[string]any{"model": "m"}, &store.AgentRecord{Settings: map[string]any{"slackBot": local}}, local},
		{"receiver's own copy beats the sender's", map[string]any{"slackBot": map[string]any{"enabled": false}}, &store.AgentRecord{Settings: map[string]any{"slackBot": local}}, local},
	}
	for _, c := range cases {
		keepLocalLegacySlackSettings(c.incoming, c.current)
		got, has := findSettingFold(c.incoming, "slackBot")
		if c.want == nil {
			if has {
				t.Errorf("%s: slackBot = %v, want none", c.name, got)
			}
			continue
		}
		if m, _ := got.(map[string]any); !has || m["enabled"] != true {
			t.Errorf("%s: slackBot = %v, want the local value", c.name, got)
		}
	}
}

// A goal waiting on a Slack thread can only be resumed through the
// package's event stream; with none connected the caller is told so.
func TestGoalResumeWithoutSlackPackage(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	req := goalRecoveryRequest{AgentID: "ag_1", SessionKey: "ag_1:slack:C1:1.2", UserID: "U123", ThreadID: "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b", Generation: 4}
	err := s.resumeGoalSurface(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "Slack extension is not connected") {
		t.Fatalf("resume with nothing connected = %v", err)
	}
}
