package server

import (
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
)

// TestKVNamespaceAllowed pins the authorisation rule the kv handlers
// share. The policy layer already confines an extension's kv path;
// this is the handler-side half, and it is what makes the kv:own scope
// the operator approves at install time actually usable.
func TestKVNamespaceAllowed(t *testing.T) {
	ext := func(scopes ...string) auth.Principal {
		return auth.Principal{
			Role:        auth.RoleExtension,
			ExtensionID: "demo",
			Scopes:      scopes,
		}
	}
	cases := []struct {
		name string
		p    auth.Principal
		ns   string
		want bool
	}{
		{"owner anywhere", auth.Principal{Role: auth.RoleOwner}, "anything", true},
		{"extension own namespace", ext("kv:own"), "ext.demo", true},
		{"extension other package", ext("kv:own"), "ext.other", false},
		{"extension kojo namespace", ext("kv:own"), "slackbot", false},
		{"extension without the scope", ext("chat:read"), "ext.demo", false},
		{"agent", auth.Principal{Role: auth.RoleAgent, AgentID: "ag_1"}, "ext.demo", false},
		{"guest", auth.Principal{}, "ext.demo", false},
		// A principal with no extension ID has no namespace of its
		// own, so it must not match the empty-namespace form either.
		{"malformed extension", auth.Principal{Role: auth.RoleExtension, Scopes: []string{"kv:own"}}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := kvNamespaceAllowed(c.p, c.ns); got != c.want {
				t.Fatalf("kvNamespaceAllowed(%s) = %v, want %v", c.ns, got, c.want)
			}
		})
	}
}
