package auth

import (
	"net/http"
	"testing"
)

var (
	deputyP = Principal{Role: RoleAgent, AgentID: "ag_x", OwnerDeputy: true}
	// A grant may be held alongside the destructive one.
	deputyPrivP = Principal{Role: RolePrivAgent, AgentID: "ag_x", OwnerDeputy: true}
	plainP      = Principal{Role: RoleAgent, AgentID: "ag_x"}
)

// The grant is defined as power over OTHERS. Applying it to the holder
// would let a deputy lift its own restrictions, so it must read false
// for its own ID.
func TestIsOwnerDeputyOver_SelfIsNotCovered(t *testing.T) {
	if !deputyP.IsOwnerDeputyOver("ag_y") {
		t.Error("deputy is not deputy over another agent")
	}
	if deputyP.IsOwnerDeputyOver("ag_x") {
		t.Error("deputy is deputy over itself")
	}
	if deputyP.IsOwnerDeputyOver("") {
		t.Error("deputy is deputy over an empty target")
	}
	if plainP.IsOwnerDeputyOver("ag_y") {
		t.Error("plain agent is deputy over another agent")
	}
	// Owner and Peer pass every gate on their own and carry no AgentID
	// to compare against.
	if (Principal{Role: RoleOwner}).IsOwnerDeputy() {
		t.Error("owner reported as owner-deputy")
	}
	if (Principal{Role: RolePeer, PeerID: "dev1", OwnerDeputy: true}).IsOwnerDeputy() {
		t.Error("peer reported as owner-deputy")
	}
}

func TestOwnerDeputy_Capabilities(t *testing.T) {
	if !deputyP.CanCreateAgent() {
		t.Error("deputy cannot create agents")
	}
	if plainP.CanCreateAgent() {
		t.Error("plain agent can create agents")
	}
	// The grant-management routes must not be self-propagating, and
	// CanForkOrCreate stays owner-only because unrelated global routes
	// reuse it as their owner gate.
	if deputyP.CanForkOrCreate() || deputyP.CanSetPrivileged() || deputyP.CanSetOwnerDeputy() {
		t.Error("deputy reached an owner-only grant gate")
	}
	// Fork follows the same self-exclusion as everything else: allowed
	// on another agent, denied on the deputy itself.
	if !deputyP.CanFork("ag_y") {
		t.Error("deputy cannot fork another agent")
	}
	if deputyP.CanFork("ag_x") {
		t.Error("deputy can fork itself")
	}
	if plainP.CanFork("ag_y") || (Principal{Role: RolePrivAgent, AgentID: "ag_x"}).CanFork("ag_y") {
		t.Error("a non-deputy agent can fork")
	}
	if !(Principal{Role: RoleOwner}).CanFork("ag_y") {
		t.Error("owner cannot fork")
	}
	if !deputyP.CanReadFull("ag_y") || !deputyP.CanMutateSelf("ag_y") || !deputyP.CanDeleteOrReset("ag_y") {
		t.Error("deputy denied on another agent")
	}
	if plainP.CanReadFull("ag_y") || plainP.CanMutateSelf("ag_y") || plainP.CanDeleteOrReset("ag_y") {
		t.Error("plain agent allowed on another agent")
	}
	// The destructive grant is unchanged by the new one.
	if !deputyPrivP.CanRestartServer() || deputyP.CanRestartServer() {
		t.Error("restart gate no longer tracks Privileged alone")
	}
}

func TestAllowNonOwner_OwnerDeputy(t *testing.T) {
	cases := []struct {
		method, path string
		p            Principal
		want         bool
	}{
		// Creating on the owner's behalf.
		{http.MethodPost, "/api/v1/agents", deputyP, true},
		{http.MethodPost, "/api/v1/agents", plainP, false},
		// Managing another agent: settings, workspace files, memory.
		{http.MethodPatch, "/api/v1/agents/ag_y", deputyP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/persona", deputyP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/user-context", deputyP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/status", deputyP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/anchor", deputyP, true},
		{http.MethodPut, "/api/v1/agents/ag_y/memory", deputyP, true},
		{http.MethodPost, "/api/v1/agents/ag_y/memory-entries", deputyP, true},
		{http.MethodGet, "/api/v1/agents/ag_y/files", deputyP, true},
		{http.MethodDelete, "/api/v1/agents/ag_y", deputyP, true},
		{http.MethodPost, "/api/v1/agents/ag_y/reset", deputyP, true},
		// Talking to another agent without a DM.
		{http.MethodPost, "/api/v1/agents/ag_y/messages", deputyP, true},
		{http.MethodGet, "/api/v1/agents/ag_y/messages", deputyP, true},
		// Grant management stays owner-only, so the grant
		// cannot spread; handoff/switch stays with the agent itself.
		{http.MethodPost, "/api/v1/agents/ag_y/privilege", deputyP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/owner-deputy", deputyP, false},
		{http.MethodPost, "/api/v1/agents/ag_x/owner-deputy", deputyP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/fork", deputyP, true},
		{http.MethodPost, "/api/v1/agents/ag_x/fork", deputyP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/handoff/switch", deputyP, false},
		// Owner-only surfaces outside the agent tree are unaffected.
		{http.MethodGet, "/api/v1/sessions", deputyP, false},
		{http.MethodGet, "/api/v1/git/status", deputyP, false},
		{http.MethodPost, "/api/v1/system/restart", deputyP, false},
		{http.MethodPost, "/api/v1/system/restart", deputyPrivP, true},
		// Nothing new over itself: the same routes a plain agent gets.
		{http.MethodPost, "/api/v1/agents/ag_x/messages", deputyP, false},
		{http.MethodGet, "/api/v1/agents/ag_x/queued-messages", deputyP, false},
		// A plain agent is untouched by any of this.
		{http.MethodPatch, "/api/v1/agents/ag_y", plainP, false},
		{http.MethodPost, "/api/v1/agents/ag_y/messages", plainP, false},
	}
	for _, c := range cases {
		if got := AllowNonOwner(c.p, c.method, c.path); got != c.want {
			t.Errorf("AllowNonOwner(%v deputy=%v, %s %s) = %v, want %v",
				c.p.Role, c.p.OwnerDeputy, c.method, c.path, got, c.want)
		}
	}
}

// The resolver has to stamp the flag for both agent roles, or the grant
// silently does nothing for an agent that also holds Privileged.
func TestResolver_StampsOwnerDeputy(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "owner-x")
	if err != nil {
		t.Fatal(err)
	}
	deputyTok, _ := st.AgentToken("ag_deputy")
	bothTok, _ := st.AgentToken("ag_both")
	plainTok, _ := st.AgentToken("ag_plain")

	r := NewResolver(st,
		func(id string) bool { return id == "ag_both" },
		func(id string) bool { return id == "ag_deputy" || id == "ag_both" })

	if p := r.Resolve(deputyTok); p.Role != RoleAgent || !p.OwnerDeputy {
		t.Errorf("deputy resolved to %+v", p)
	}
	if p := r.Resolve(bothTok); p.Role != RolePrivAgent || !p.OwnerDeputy {
		t.Errorf("privileged deputy resolved to %+v", p)
	}
	if p := r.Resolve(plainTok); p.Role != RoleAgent || p.OwnerDeputy {
		t.Errorf("plain agent resolved to %+v", p)
	}
}
