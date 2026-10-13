package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

func extChatCall(h http.HandlerFunc, p auth.Principal, method, target, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func demoExtPrincipal(agents ...string) auth.Principal {
	return auth.Principal{Role: auth.RoleExtension, ExtensionID: "demo", Scopes: []string{"chat:session"}, AgentScope: agents}
}

func TestExtensionSessionKeyIsNamespaced(t *testing.T) {
	got := extensionSessionKey("ag_1", "discord", "c1:t2")
	if got != "ag_1:ext.discord:c1:t2" {
		t.Fatalf("session key = %q", got)
	}
	// A package cannot spell its way into the Slack bot's key space:
	// whatever suffix it picks sits behind its own "ext.<id>:" tag.
	if strings.HasPrefix(extensionSessionKey("ag_1", "discord", "slack:C1:1.2"), "ag_1:slack:") {
		t.Fatal("extension key collides with the slack namespace")
	}
	for _, bad := range []string{"", ":x", "a b", "a/b", "../x", strings.Repeat("a", 161)} {
		if extensionSessionKeyRe.MatchString(bad) {
			t.Errorf("session key %q accepted", bad)
		}
	}
}

func TestExtensionChatRejectsNonExtensionsAndBadInput(t *testing.T) {
	s := newExtTestServer(t)
	pv := map[string]string{"id": "ag_1"}

	// The Owner has no extension ID to namespace a session with.
	rr := extChatCall(s.handleExtensionChat, auth.Principal{Role: auth.RoleOwner}, "POST", "/api/v1/agents/ag_1/ext-chat", `{"sessionKey":"k","message":"hi"}`, pv)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("owner call = %d", rr.Code)
	}
	// No router on this fixture (PeerOnly shape): unavailable, not a panic.
	rr = extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat", `{"sessionKey":"k","message":"hi"}`, pv)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-router call = %d", rr.Code)
	}

	s.externalChat = newExternalChatRouter(s)
	for name, body := range map[string]string{
		"bad key":       `{"sessionKey":"a b","message":"hi"}`,
		"empty message": `{"sessionKey":"k","message":"  "}`,
		"unknown field": `{"sessionKey":"k","message":"hi","goal":{}}`,
		"outside path":  `{"sessionKey":"k","message":"hi","attachments":[{"path":"/etc/passwd"}]}`,
	} {
		rr := extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat", body, pv)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s", name, rr.Code, rr.Body.String())
		}
	}
	rr = extChatCall(s.handleExtensionChatSteer, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat/steer", `{"sessionKey":"k","content":""}`, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty steer = %d", rr.Code)
	}
}

func TestExtensionChatSessionsAreExclusive(t *testing.T) {
	var e extensionChatSessions
	if !e.acquire("k") || e.acquire("k") {
		t.Fatal("a session key must be held by one turn at a time")
	}
	if !e.acquire("other") {
		t.Fatal("unrelated keys must not block each other")
	}
	e.release("k")
	if !e.acquire("k") {
		t.Fatal("release did not free the key")
	}
}

type extTestSecrets map[string]string

func (m extTestSecrets) GetToken(p, a, s, k string) (string, error) {
	v, ok := m[a+"/"+s+"/"+k]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}
func (m extTestSecrets) SetToken(p, a, s, k, v string, _ time.Time) error {
	m[a+"/"+s+"/"+k] = v
	return nil
}
func (m extTestSecrets) DeleteToken(p, a, s, k string) error       { delete(m, a+"/"+s+"/"+k); return nil }
func (m extTestSecrets) DeleteTokensBySource(p, a, s string) error { return nil }

func TestExtensionSecretsRoute(t *testing.T) {
	s := newExtTestServer(t)
	s.extensions.SetSecretStore(extTestSecrets{})
	dir := t.TempDir()
	writeExtTestFiles(t, dir, map[string]string{
		extpkg.ManifestFilename: `{"id":"demo","name":"Demo","version":"1.0.0",
			"contributes":{"skills":["skills/demo"],"settings":{"scope":"per-agent","schema":"s.json"}}}`,
		"skills/demo/SKILL.md": "# demo\n",
		"s.json":               `{"type":"object","properties":{"botToken":{"type":"string","format":"password"}}}`,
	})
	runExtTestGit(t, dir, "init", "-q", "-b", "main")
	commitExtTestRepo(t, dir)
	if _, err := s.extensions.Install(context.Background(), extpkg.InstallRequest{URL: dir, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// The owner-facing binding route accepts the secret and hands back
	// only its name.
	rr := extCall(t, s.handlePutExtensionAgentBinding, "PUT", "/api/v1/extensions/demo/agents/ag_1",
		`{"enabled":true,"config":{"botToken":"hunter2"}}`, map[string]string{"id": "demo", "agentId": "ag_1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("binding = %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "hunter2") || !strings.Contains(rr.Body.String(), `"secrets":["botToken"]`) {
		t.Fatalf("binding response = %s", rr.Body.String())
	}
	if rr := extCall(t, s.handleListExtensions, "GET", "/api/v1/extensions", "", nil); strings.Contains(rr.Body.String(), "hunter2") {
		t.Fatal("list leaked the secret")
	}

	rr = extChatCall(s.handleExtensionSecrets, demoExtPrincipal("ag_1"), "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"botToken":"hunter2"`) {
		t.Fatalf("own secrets = %d %s", rr.Code, rr.Body.String())
	}
	// Bound to ag_2 only: ag_1's secrets are out of reach.
	rr = extChatCall(s.handleExtensionSecrets, demoExtPrincipal("ag_2"), "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unbound agent secrets = %d", rr.Code)
	}
	// Another package's token resolves to that package, which has none.
	other := demoExtPrincipal("ag_1")
	other.ExtensionID = "other"
	rr = extChatCall(s.handleExtensionSecrets, other, "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code == http.StatusOK {
		t.Fatalf("foreign package read secrets: %s", rr.Body.String())
	}
	rr = extChatCall(s.handleExtensionSecrets, auth.Principal{Role: auth.RoleOwner}, "GET", "/api/v1/extension/secrets", "", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("owner on the self route = %d", rr.Code)
	}
}
