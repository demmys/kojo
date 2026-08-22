package auth

import (
	"net/http"
	"testing"
)

var (
	adminP = Principal{Role: RoleAgent, AgentID: "ag_x", AgentAdmin: true}
	// A grant may be held alongside the destructive one.
	adminPrivP = Principal{Role: RolePrivAgent, AgentID: "ag_x", AgentAdmin: true}
	plainP     = Principal{Role: RoleAgent, AgentID: "ag_x"}
)

// The grant is defined as power over OTHERS. Applying it to the holder
// would let an admin lift its own restrictions, so it must read false
// for its own ID.
func TestIsAgentAdminOver_SelfIsNotCovered(t *testing.T) {
	if !adminP.IsAgentAdminOver("ag_y") {
		t.Error("admin is not admin over another agent")
	}
	if adminP.IsAgentAdminOver("ag_x") {
		t.Error("admin is admin over itself")
	}
	if adminP.IsAgentAdminOver("") {
		t.Error("admin is admin over an empty target")
	}
	if plainP.IsAgentAdminOver("ag_y") {
		t.Error("plain agent is admin over another agent")
	}
	// Owner and Peer pass every gate on their own and carry no AgentID
	// to compare against.
	if (Principal{Role: RoleOwner}).IsAgentAdmin() {
		t.Error("owner reported as agent-admin")
	}
	if (Principal{Role: RolePeer, PeerID: "dev1", AgentAdmin: true}).IsAgentAdmin() {
		t.Error("peer reported as agent-admin")
	}
}

func TestAgentAdmin_Capabilities(t *testing.T) {
	if !adminP.CanCreateAgent() {
		t.Error("admin cannot create agents")
	}
	if plainP.CanCreateAgent() {
		t.Error("plain agent can create agents")
	}
	// Fork copies another agent's persona and memory into a new agent,
	// and the grant-management routes must not be self-propagating.
	if adminP.CanForkOrCreate() || adminP.CanSetPrivileged() || adminP.CanSetAgentAdmin() {
		t.Error("admin reached an owner-only grant/fork gate")
	}
	if !adminP.CanReadFull("ag_y") || !adminP.CanMutateSelf("ag_y") || !adminP.CanDeleteOrReset("ag_y") {
		t.Error("admin denied on another agent")
	}
	if plainP.CanReadFull("ag_y") || plainP.CanMutateSelf("ag_y") || plainP.CanDeleteOrReset("ag_y") {
		t.Error("plain agent allowed on another agent")
	}
	// The destructive grant is unchanged by the new one.
	if !adminPrivP.CanRestartServer() || adminP.CanRestartServer() {
		t.Error("restart gate no longer tracks Privileged alone")
	}
}

func TestAllowNonOwner_AgentAdmin(t *testing.T) {
	cases := []struct {
		method, path string
		p            Principal
		want         bool
	}{
		// Creating on the owner's behalf.
		{http.MethodPost, "/api/v1/agents", adminP, true},
		{http.MethodPost, "/api/v1/agents", plainP, false},
		// Managing another agent: settings, workspace files, memory.
		{http.MethodPatch, "/api/v1/agents/ag_y", adminP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/persona", adminP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/user-context", adminP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/status", adminP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/anchor", adminP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/memory", adminP, true},
		{http.MethodPost, "/api/v1/agents/ag_y/memory-entries", adminP, true},
		{http.MethodGet, "/api/v1/agents/ag_y/files", adminP, true},
		{http.MethodDelete, "/api/v1/agents/ag_y", adminP, true},
		{http.MethodPost, "/api/v1/agents/ag_y/reset", adminP, true},
		// Talking to another agent without a DM.
		{http.MethodPost, "/api/v1/agents/ag_y/messages", adminP, true},
		{http.MethodGet, "/api/v1/agents/ag_y/messages", adminP, true},
		// Grant management and fork stay owner-only, so the grant
		// cannot spread; handoff/switch stays with the agent itself.
		{http.MethodPost, "/api/v1/agents/ag_y/privilege", adminP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/agent-admin", adminP, false},
		{http.MethodPost, "/api/v1/agents/ag_x/agent-admin", adminP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/fork", adminP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/handoff/switch", adminP, false},
		// Owner-only surfaces outside the agent tree are unaffected.
		{http.MethodGet, "/api/v1/sessions", adminP, false},
		{http.MethodGet, "/api/v1/git/status", adminP, false},
		{http.MethodPost, "/api/v1/system/restart", adminP, false},
		{http.MethodPost, "/api/v1/system/restart", adminPrivP, true},
		// Nothing new over itself: the same routes a plain agent gets.
		{http.MethodPost, "/api/v1/agents/ag_x/messages", adminP, false},
		{http.MethodGet, "/api/v1/agents/ag_x/queued-messages", adminP, false},
		// A plain agent is untouched by any of this.
		{http.MethodPatch, "/api/v1/agents/ag_y", plainP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/messages", plainP, false},
	}
	for _, c := range cases {
		if got := AllowNonOwner(c.p, c.method, c.path); got != c.want {
			t.Errorf("AllowNonOwner(%v admin=%v, %s %s) = %v, want %v",
				c.p.Role, c.p.AgentAdmin, c.method, c.path, got, c.want)
		}
	}
}

// The resolver has to stamp the flag for both agent roles, or the grant
// silently does nothing for an agent that also holds Privileged.
func TestResolver_StampsAgentAdmin(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "owner-x")
	if err != nil {
		t.Fatal(err)
	}
	adminTok, _ := st.AgentToken("ag_admin")
	bothTok, _ := st.AgentToken("ag_both")
	plainTok, _ := st.AgentToken("ag_plain")

	r := NewResolver(st,
		func(id string) bool { return id == "ag_both" },
		func(id string) bool { return id == "ag_admin" || id == "ag_both" })

	if p := r.Resolve(adminTok); p.Role != RoleAgent || !p.AgentAdmin {
		t.Errorf("admin resolved to %+v", p)
	}
	if p := r.Resolve(bothTok); p.Role != RolePrivAgent || !p.AgentAdmin {
		t.Errorf("privileged admin resolved to %+v", p)
	}
	if p := r.Resolve(plainTok); p.Role != RoleAgent || p.AgentAdmin {
		t.Errorf("plain agent resolved to %+v", p)
	}
}
