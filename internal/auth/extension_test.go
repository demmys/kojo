package auth

import (
	"testing"
)

func extPrincipal(scopes []string, agents ...string) Principal {
	return Principal{
		Role:        RoleExtension,
		ExtensionID: "demo",
		Scopes:      scopes,
		AgentScope:  agents,
	}
}

func TestExtensionPrincipalCaps(t *testing.T) {
	p := extPrincipal([]string{"chat:send"}, "ag_1")
	if !p.IsExtension() {
		t.Fatal("IsExtension false")
	}
	if !p.HasScope("chat:send") || p.HasScope("agents:write") {
		t.Fatal("HasScope wrong")
	}
	if !p.BoundTo("ag_1") || p.BoundTo("ag_2") || p.BoundTo("") {
		t.Fatal("BoundTo wrong")
	}
	// An extension is strictly weaker than an agent: none of the
	// agent-role capabilities may leak into it.
	if p.IsOwner() || p.IsAgent() || p.CanDeleteOrReset("ag_1") || p.CanCreateAgent() || p.CanRestartServer() {
		t.Fatalf("extension principal has agent/owner capabilities: %+v", p)
	}
	// Scopes are meaningless for every other role.
	for _, other := range []Principal{
		{Role: RoleAgent, AgentID: "ag_1", Scopes: []string{"agents:write"}},
		{Role: RoleGuest, Scopes: []string{"agents:write"}},
	} {
		if other.HasScope("agents:write") {
			t.Fatalf("non-extension principal claimed a scope: %+v", other)
		}
	}
}

func TestAllowExtensionRoutes(t *testing.T) {
	all := []string{"chat:send", "chat:read", "events:subscribe", "agents:read",
		"agents:write", "kv:own", "files:agent", "blob:read"}

	cases := []struct {
		name   string
		p      Principal
		method string
		path   string
		want   bool
	}{
		// Discovery needs no scope at all.
		{"info without scopes", extPrincipal(nil), "GET", "/api/v1/info", true},

		// KV is confined to the package's own namespace.
		{"own kv read", extPrincipal([]string{"kv:own"}), "GET", "/api/v1/kv/ext.demo/k", true},
		{"own kv write", extPrincipal([]string{"kv:own"}), "PUT", "/api/v1/kv/ext.demo/k", true},
		{"own kv delete", extPrincipal([]string{"kv:own"}), "DELETE", "/api/v1/kv/ext.demo/k", true},
		{"other package kv", extPrincipal([]string{"kv:own"}), "GET", "/api/v1/kv/ext.other/k", false},
		{"agent kv", extPrincipal([]string{"kv:own"}), "GET", "/api/v1/kv/ag_1/k", false},
		{"namespace prefix trick", extPrincipal([]string{"kv:own"}), "GET", "/api/v1/kv/ext.demo2/k", false},
		{"kv without scope", extPrincipal(all[:1]), "GET", "/api/v1/kv/ext.demo/k", false},
		{"kv post", extPrincipal([]string{"kv:own"}), "POST", "/api/v1/kv/ext.demo/k", false},

		// Instance-wide reads, each behind its own scope.
		{"events", extPrincipal([]string{"events:subscribe"}), "GET", "/api/v1/events", true},
		{"ws", extPrincipal([]string{"events:subscribe"}), "GET", "/api/v1/ws", true},
		{"events unscoped", extPrincipal(all[:1]), "GET", "/api/v1/events", false},
		{"blob", extPrincipal([]string{"blob:read"}), "GET", "/api/v1/blob/abc", true},
		{"blob head", extPrincipal([]string{"blob:read"}), "HEAD", "/api/v1/blob/abc", true},
		{"blob delete", extPrincipal([]string{"blob:read"}), "DELETE", "/api/v1/blob/abc", false},
		{"roster", extPrincipal([]string{"agents:read"}), "GET", "/api/v1/agents", true},
		{"directory", extPrincipal([]string{"agents:read"}), "GET", "/api/v1/agents/directory", true},
		{"roster unscoped", extPrincipal([]string{"chat:read"}), "GET", "/api/v1/agents", false},

		// Agent-addressed routes need the binding AND the scope.
		{"bound agent read", extPrincipal([]string{"agents:read"}, "ag_1"), "GET", "/api/v1/agents/ag_1", true},
		{"unbound agent read", extPrincipal([]string{"agents:read"}, "ag_2"), "GET", "/api/v1/agents/ag_1", false},
		{"unbound with every scope", extPrincipal(all), "POST", "/api/v1/agents/ag_1/messages", false},
		{"bound messages read", extPrincipal([]string{"chat:read"}, "ag_1"), "GET", "/api/v1/agents/ag_1/messages", true},
		{"bound messages send", extPrincipal([]string{"chat:send"}, "ag_1"), "POST", "/api/v1/agents/ag_1/messages", true},
		{"send without scope", extPrincipal([]string{"chat:read"}, "ag_1"), "POST", "/api/v1/agents/ag_1/messages", false},
		{"files read", extPrincipal([]string{"files:agent"}, "ag_1"), "GET", "/api/v1/agents/ag_1/files", true},
		{"files write refused", extPrincipal(all, "ag_1"), "PUT", "/api/v1/agents/ag_1/files", false},
		{"patch agent", extPrincipal([]string{"agents:write"}, "ag_1"), "PATCH", "/api/v1/agents/ag_1", true},
		{"patch without scope", extPrincipal([]string{"agents:read"}, "ag_1"), "PATCH", "/api/v1/agents/ag_1", false},
		{"attention", extPrincipal([]string{"chat:send"}, "ag_1"), "POST", "/api/v1/agents/ag_1/attention", true},

		// Nothing destructive or administrative, whatever the scopes.
		{"delete agent", extPrincipal(all, "ag_1"), "DELETE", "/api/v1/agents/ag_1", false},
		{"reset agent", extPrincipal(all, "ag_1"), "POST", "/api/v1/agents/ag_1/reset", false},
		{"create agent", extPrincipal(all, "ag_1"), "POST", "/api/v1/agents", false},
		{"restart system", extPrincipal(all, "ag_1"), "POST", "/api/v1/system/restart", false},
		{"read extensions", extPrincipal(all, "ag_1"), "GET", "/api/v1/extensions", false},
		{"install extension", extPrincipal(all, "ag_1"), "POST", "/api/v1/extensions", false},
		{"read own token", extPrincipal(all, "ag_1"), "GET", "/api/v1/extensions/demo/token", false},
		{"read creds", extPrincipal(all, "ag_1"), "GET", "/api/v1/credentials", false},
		{"agent token", extPrincipal(all, "ag_1"), "GET", "/api/v1/agents/ag_1/token", false},
		{"exec", extPrincipal(all, "ag_1"), "POST", "/api/v1/agents/ag_1/exec", false},
		// The agent's own MCP transport is its tool surface — every
		// tool it holds, with no scope in front of it. A package
		// bound to the agent still does not get to speak through it.
		{"agent mcp", extPrincipal(all, "ag_1"), "POST", "/api/v1/agents/ag_1/mcp", false},
		{"agent mcp get", extPrincipal(all, "ag_1"), "GET", "/api/v1/agents/ag_1/mcp", false},
		{"unknown route", extPrincipal(all, "ag_1"), "GET", "/api/v1/whatever", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AllowNonOwner(c.p, c.method, c.path); got != c.want {
				t.Fatalf("AllowNonOwner(%s %s) = %v, want %v", c.method, c.path, got, c.want)
			}
		})
	}
}

func TestExtensionKVNamespace(t *testing.T) {
	if got := ExtensionKVNamespace("demo"); got != "ext.demo" {
		t.Fatalf("namespace = %q", got)
	}
	if got := ExtensionKVNamespace(""); got != "" {
		t.Fatalf("empty id produced namespace %q", got)
	}
	// An extension with no ID must not fall through onto some other
	// package's rows via an empty namespace match.
	p := Principal{Role: RoleExtension, Scopes: []string{"kv:own"}}
	if AllowNonOwner(p, "GET", "/api/v1/kv//k") {
		t.Fatal("empty extension id matched a KV namespace")
	}
}

func TestResolverExtensionTokens(t *testing.T) {
	dir := t.TempDir()
	st, err := NewTokenStore(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	agentTok, err := st.AgentToken("ag_1")
	if err != nil {
		t.Fatal(err)
	}
	r := NewResolver(st, nil, nil)

	// Before wiring, an extension token is just a guest.
	if p := r.Resolve("ext-token"); p.Role != RoleGuest {
		t.Fatalf("unwired resolver gave %v", p.Role)
	}

	r.SetExtensionResolver(func(token string) (ExtensionIdentity, bool) {
		switch token {
		case "ext-token":
			return ExtensionIdentity{ID: "demo", Scopes: []string{"chat:send"}, AgentScope: []string{"ag_1"}}, true
		case agentTok, st.OwnerToken():
			// An extension resolver that claims a token belonging to a
			// stronger role must never win.
			return ExtensionIdentity{ID: "evil", Scopes: []string{"agents:write"}}, true
		}
		return ExtensionIdentity{}, false
	})

	p := r.Resolve("ext-token")
	if p.Role != RoleExtension || p.ExtensionID != "demo" || !p.BoundTo("ag_1") {
		t.Fatalf("Resolve = %+v", p)
	}
	if p := r.Resolve(st.OwnerToken()); p.Role != RoleOwner {
		t.Fatalf("owner token downgraded to %v", p.Role)
	}
	if p := r.Resolve(agentTok); p.Role != RoleAgent || p.AgentID != "ag_1" {
		t.Fatalf("agent token downgraded to %+v", p)
	}
	if p := r.Resolve("nonsense"); p.Role != RoleGuest {
		t.Fatalf("unknown token = %v", p.Role)
	}

	// An identity with an empty ID is refused rather than becoming a
	// scope-holding principal with no namespace.
	r.SetExtensionResolver(func(string) (ExtensionIdentity, bool) {
		return ExtensionIdentity{Scopes: []string{"kv:own"}}, true
	})
	if p := r.Resolve("whatever"); p.Role != RoleGuest {
		t.Fatalf("empty extension id resolved to %v", p.Role)
	}
}
