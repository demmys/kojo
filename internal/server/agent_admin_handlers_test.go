package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// postAgentAdmin drives the handler directly (route wiring already
// resolved {id} for it) with the given principal and body.
func postAgentAdmin(t *testing.T, srv *Server, id, body string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/agent-admin", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", id)
	r = authedRequest(r, p)
	w := httptest.NewRecorder()
	srv.handleAgentAdminAgent(w, r)
	return w
}

// The Owner may grant and revoke the flag; the grant lands on the
// manager (and therefore in settings_json via save()).
func TestAgentAdminEndpoint_OwnerGrantsAndRevokes(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "admin-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if srv.agents.IsAgentAdmin(a.ID) {
		t.Fatal("fresh agent is already an agent-admin")
	}

	w := postAgentAdmin(t, srv, a.ID, `{"agentAdmin":true}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["agentAdmin"] != true || resp["id"] != a.ID {
		t.Fatalf("response = %v", resp)
	}
	if !srv.agents.IsAgentAdmin(a.ID) {
		t.Fatal("flag not set on the manager")
	}
	if got, ok := srv.agents.Get(a.ID); !ok || !got.AgentAdmin {
		t.Fatalf("agent record not updated: %+v", got)
	}

	w = postAgentAdmin(t, srv, a.ID, `{"agentAdmin":false}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body %s", w.Code, w.Body.String())
	}
	if srv.agents.IsAgentAdmin(a.ID) {
		t.Fatal("flag still set after revoke")
	}
}

// The grant is not self-propagating: neither a plain agent, a
// privileged agent, nor an existing agent-admin may hand it out.
func TestAgentAdminEndpoint_NonOwnerForbidden(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "victim"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	callers := []auth.Principal{
		{Role: auth.RoleAgent, AgentID: "ag_caller"},
		{Role: auth.RolePrivAgent, AgentID: "ag_caller"},
		{Role: auth.RoleAgent, AgentID: "ag_caller", AgentAdmin: true},
		{Role: auth.RolePrivAgent, AgentID: "ag_caller", AgentAdmin: true},
		{Role: auth.RoleAgent, AgentID: a.ID, AgentAdmin: true}, // self-promotion
		{Role: auth.RolePeer, PeerID: "peer-1"},
		{Role: auth.RoleGuest},
	}
	for _, p := range callers {
		w := postAgentAdmin(t, srv, a.ID, `{"agentAdmin":true}`, p)
		if w.Code != http.StatusForbidden {
			t.Fatalf("caller %+v: status = %d, want 403 (body %s)", p, w.Code, w.Body.String())
		}
		if srv.agents.IsAgentAdmin(a.ID) {
			t.Fatalf("caller %+v flipped the flag", p)
		}
	}
}

// A missing target is a 404, not a silent success.
func TestAgentAdminEndpoint_UnknownAgent(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	w := postAgentAdmin(t, srv, "ag_nope", `{"agentAdmin":true}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// If-Match is honoured the same way the privilege endpoint honours it:
// wildcard rejected, stale etag → 412.
func TestAgentAdminEndpoint_IfMatch(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "etag-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	mk := func(ifMatch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/agent-admin",
			strings.NewReader(`{"agentAdmin":true}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Match", ifMatch)
		r.SetPathValue("id", a.ID)
		r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		srv.handleAgentAdminAgent(w, r)
		return w
	}
	if w := mk("*"); w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if w := mk(`"stale-etag"`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale etag status = %d, want 412 (body %s)", w.Code, w.Body.String())
	}
	if srv.agents.IsAgentAdmin(a.ID) {
		t.Fatal("flag flipped despite a failed precondition")
	}
}

// PATCH must never carry the grant, whatever the casing and whoever
// asks — the owner-only endpoint above is the single way in.
func TestUpdateAgent_RejectsAgentAdminKey(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "patch-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	bodies := []string{`{"agentAdmin":true}`, `{"AGENTADMIN":true}`, `{"name":"x","agentadmin":true}`}
	callers := []auth.Principal{
		{Role: auth.RoleOwner},
		{Role: auth.RoleAgent, AgentID: a.ID},
		{Role: auth.RoleAgent, AgentID: "ag_admin", AgentAdmin: true},
	}
	for _, body := range bodies {
		for _, p := range callers {
			r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+a.ID, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.SetPathValue("id", a.ID)
			r = authedRequest(r, p)
			w := httptest.NewRecorder()
			srv.handleUpdateAgent(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("body %s caller %+v: status = %d, want 403 (body %s)",
					body, p, w.Code, w.Body.String())
			}
			if srv.agents.IsAgentAdmin(a.ID) {
				t.Fatalf("body %s caller %+v minted a grant", body, p)
			}
		}
	}
}

// Agent creation is reachable for an agent-admin and refused for a
// plain (or merely privileged) agent.
func TestCreateAgent_AgentAdminAllowed(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	create := func(p auth.Principal, name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents",
			strings.NewReader(`{"name":"`+name+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r = authedRequest(r, p)
		w := httptest.NewRecorder()
		srv.handleCreateAgent(w, r)
		return w
	}
	if w := create(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_a"}, "nope"); w.Code != http.StatusForbidden {
		t.Fatalf("plain agent status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if w := create(auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_p"}, "nope2"); w.Code != http.StatusForbidden {
		t.Fatalf("priv agent status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	w := create(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_admin", AgentAdmin: true}, "made-by-admin")
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("agent-admin create status = %d, body %s", w.Code, w.Body.String())
	}
	var created struct {
		ID         string `json:"id"`
		AgentAdmin bool   `json:"agentAdmin"`
		Privileged bool   `json:"privileged"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, w.Body.String())
	}
	if created.AgentAdmin || created.Privileged {
		t.Fatalf("creation minted a grant: %+v", created)
	}
}

// A fork never inherits the grant.
func TestForkDropsAgentAdmin(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "fork-src"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := srv.agents.SetAgentAdmin(a.ID, true); err != nil {
		t.Fatalf("set agent admin: %v", err)
	}
	f, err := srv.agents.Fork(a.ID, agent.ForkOptions{Name: "fork-dst"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if f.AgentAdmin {
		t.Fatal("fork inherited agentAdmin")
	}
	if srv.agents.IsAgentAdmin(f.ID) {
		t.Fatal("fork registered as an agent-admin")
	}
}

// The grant is persisted (settings_json), not just held in memory: a
// fresh manager over the same config dir sees it.
func TestAgentAdminSurvivesManagerReload(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "persist-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if w := postAgentAdmin(t, srv, a.ID, `{"agentAdmin":true}`,
		auth.Principal{Role: auth.RoleOwner}); w.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body %s", w.Code, w.Body.String())
	}
	if err := srv.agents.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	mgr2, err := agent.NewManager(slog.Default())
	if err != nil {
		t.Fatalf("reopen manager: %v", err)
	}
	t.Cleanup(func() { _ = mgr2.Close() })
	if !mgr2.IsAgentAdmin(a.ID) {
		t.Fatal("agentAdmin did not survive the reload")
	}
}
