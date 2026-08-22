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
	"github.com/loppo-llc/kojo/internal/store"
)

// postOwnerDeputy drives the handler directly (route wiring already
// resolved {id} for it) with the given principal and body.
func postOwnerDeputy(t *testing.T, srv *Server, id, body string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+id+"/owner-deputy", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", id)
	r = authedRequest(r, p)
	w := httptest.NewRecorder()
	srv.handleOwnerDeputyAgent(w, r)
	return w
}

// The Owner may grant and revoke the flag; the grant lands on the
// manager (and therefore in settings_json via save()).
func TestOwnerDeputyEndpoint_OwnerGrantsAndRevokes(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "deputy-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if srv.agents.IsOwnerDeputy(a.ID) {
		t.Fatal("fresh agent is already an owner-deputy")
	}

	w := postOwnerDeputy(t, srv, a.ID, `{"ownerDeputy":true}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("grant status = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["ownerDeputy"] != true || resp["id"] != a.ID {
		t.Fatalf("response = %v", resp)
	}
	if !srv.agents.IsOwnerDeputy(a.ID) {
		t.Fatal("flag not set on the manager")
	}
	if got, ok := srv.agents.Get(a.ID); !ok || !got.OwnerDeputy {
		t.Fatalf("agent record not updated: %+v", got)
	}

	w = postOwnerDeputy(t, srv, a.ID, `{"ownerDeputy":false}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body %s", w.Code, w.Body.String())
	}
	if srv.agents.IsOwnerDeputy(a.ID) {
		t.Fatal("flag still set after revoke")
	}
}

// The grant is not self-propagating: neither a plain agent, a
// privileged agent, nor an existing owner-deputy may hand it out.
func TestOwnerDeputyEndpoint_NonOwnerForbidden(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "victim"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	callers := []auth.Principal{
		{Role: auth.RoleAgent, AgentID: "ag_caller"},
		{Role: auth.RolePrivAgent, AgentID: "ag_caller"},
		{Role: auth.RoleAgent, AgentID: "ag_caller", OwnerDeputy: true},
		{Role: auth.RolePrivAgent, AgentID: "ag_caller", OwnerDeputy: true},
		{Role: auth.RoleAgent, AgentID: a.ID, OwnerDeputy: true}, // self-promotion
		{Role: auth.RolePeer, PeerID: "peer-1"},
		{Role: auth.RoleGuest},
	}
	for _, p := range callers {
		w := postOwnerDeputy(t, srv, a.ID, `{"ownerDeputy":true}`, p)
		if w.Code != http.StatusForbidden {
			t.Fatalf("caller %+v: status = %d, want 403 (body %s)", p, w.Code, w.Body.String())
		}
		if srv.agents.IsOwnerDeputy(a.ID) {
			t.Fatalf("caller %+v flipped the flag", p)
		}
	}
}

// A missing target is a 404, not a silent success.
func TestOwnerDeputyEndpoint_UnknownAgent(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	w := postOwnerDeputy(t, srv, "ag_nope", `{"ownerDeputy":true}`, auth.Principal{Role: auth.RoleOwner})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// If-Match is honoured the same way the privilege endpoint honours it:
// wildcard rejected, stale etag → 412.
func TestOwnerDeputyEndpoint_IfMatch(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "etag-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	mk := func(ifMatch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/owner-deputy",
			strings.NewReader(`{"ownerDeputy":true}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Match", ifMatch)
		r.SetPathValue("id", a.ID)
		r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
		w := httptest.NewRecorder()
		srv.handleOwnerDeputyAgent(w, r)
		return w
	}
	if w := mk("*"); w.Code != http.StatusBadRequest {
		t.Fatalf("wildcard status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if w := mk(`"stale-etag"`); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale etag status = %d, want 412 (body %s)", w.Code, w.Body.String())
	}
	if srv.agents.IsOwnerDeputy(a.ID) {
		t.Fatal("flag flipped despite a failed precondition")
	}
}

// PATCH must never carry the grant, whatever the casing and whoever
// asks — the owner-only endpoint above is the single way in.
func TestUpdateAgent_RejectsOwnerDeputyKey(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "patch-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	bodies := []string{`{"ownerDeputy":true}`, `{"OWNERDEPUTY":true}`, `{"name":"x","ownerdeputy":true}`}
	callers := []auth.Principal{
		{Role: auth.RoleOwner},
		{Role: auth.RoleAgent, AgentID: a.ID},
		{Role: auth.RoleAgent, AgentID: "ag_deputy", OwnerDeputy: true},
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
			if srv.agents.IsOwnerDeputy(a.ID) {
				t.Fatalf("body %s caller %+v minted a grant", body, p)
			}
		}
	}
}

// Agent creation is reachable for an owner-deputy and refused for a
// plain (or merely privileged) agent.
func TestCreateAgent_OwnerDeputyAllowed(t *testing.T) {
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
	w := create(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_deputy", OwnerDeputy: true}, "made-by-deputy")
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("owner-deputy create status = %d, body %s", w.Code, w.Body.String())
	}
	var created struct {
		ID          string `json:"id"`
		OwnerDeputy bool   `json:"ownerDeputy"`
		Privileged  bool   `json:"privileged"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, w.Body.String())
	}
	if created.OwnerDeputy || created.Privileged {
		t.Fatalf("creation minted a grant: %+v", created)
	}
}

// A fork never inherits the grant.
func TestForkDropsOwnerDeputy(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "fork-src"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := srv.agents.SetOwnerDeputy(a.ID, true); err != nil {
		t.Fatalf("set owner deputy: %v", err)
	}
	f, err := srv.agents.Fork(a.ID, agent.ForkOptions{Name: "fork-dst"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if f.OwnerDeputy {
		t.Fatal("fork inherited ownerDeputy")
	}
	if srv.agents.IsOwnerDeputy(f.ID) {
		t.Fatal("fork registered as an owner-deputy")
	}
}

// The grant is persisted (settings_json), not just held in memory: a
// fresh manager over the same config dir sees it.
func TestOwnerDeputySurvivesManagerReload(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "persist-target"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if w := postOwnerDeputy(t, srv, a.ID, `{"ownerDeputy":true}`,
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
	if !mgr2.IsOwnerDeputy(a.ID) {
		t.Fatal("ownerDeputy did not survive the reload")
	}
}

// A deputy may fork someone ELSE, and is refused on itself — forking
// itself would hand its own memory to an agent the Owner never
// deputised. A plain or merely privileged agent is refused outright.
func TestForkAgent_DeputyOverOthersOnly(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	src, err := srv.agents.Create(agent.AgentConfig{Name: "fork-source"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	fork := func(target string, p auth.Principal, name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target+"/fork",
			strings.NewReader(`{"name":"`+name+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", target)
		r = authedRequest(r, p)
		w := httptest.NewRecorder()
		srv.handleForkAgent(w, r)
		return w
	}
	deputy := auth.Principal{Role: auth.RoleAgent, AgentID: "ag_deputy", OwnerDeputy: true}

	if w := fork(src.ID, auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_p"}, "nope"); w.Code != http.StatusForbidden {
		t.Fatalf("priv agent fork status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if w := fork("ag_deputy", deputy, "self-clone"); w.Code != http.StatusForbidden {
		t.Fatalf("self-fork status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	w := fork(src.ID, deputy, "forked-by-deputy")
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("deputy fork status = %d, body %s", w.Code, w.Body.String())
	}
	var forked struct {
		ID          string `json:"id"`
		OwnerDeputy bool   `json:"ownerDeputy"`
		Privileged  bool   `json:"privileged"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &forked); err != nil {
		t.Fatalf("bad json: %v (body %s)", err, w.Body.String())
	}
	if forked.OwnerDeputy || forked.Privileged {
		t.Fatalf("fork carries a grant: %+v", forked)
	}
}

// disabledInjections on a REMOTE target: the hub must decide on the
// original principal. Once proxied the holder stamps RolePeer, which
// passes the handler guard by design — so a remote agent self-PATCHing
// its own capability surface has to be refused before the forward.
func TestRemoteAgentPatchDisabledInjections_DeputyVsSelf(t *testing.T) {
	srv := newRemoteAgentPatchServer(t, "ag_remote", store.PeerStatusOnline)
	body := `{"disabledInjections":["status"]}`

	// The remote agent itself: refused at the hub, never forwarded.
	w := patchRemoteAgent(srv, "ag_remote", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_remote"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("self status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	// A deputy holding the grant is still refused on ITSELF.
	w = patchRemoteAgent(srv, "ag_remote", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_remote", OwnerDeputy: true})
	if w.Code != http.StatusForbidden {
		t.Fatalf("deputy-self status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	// A deputy acting on someone else clears the gate and reaches the
	// proxy, which fails to dial the seeded dead peer → 502.
	w = patchRemoteAgent(srv, "ag_remote", body,
		auth.Principal{Role: auth.RoleAgent, AgentID: "ag_deputy", OwnerDeputy: true})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("deputy status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
}
