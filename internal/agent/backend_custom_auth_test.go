package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestAppendCustomProxyEnvUsesOnlyDummyCredential(t *testing.T) {
	env := appendCustomProxyEnv([]string{"PATH=/bin"}, "http://peer:8888")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://peer:8888",
		"ANTHROPIC_API_KEY=dummy",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("custom environment contains auth token: %v", env)
		}
	}
}

func TestCustomProxyEnvStripsInheritedCredentialsAndProxies(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-secret: leaked")
	t.Setenv("ANTHROPIC_API_KEY", "cloud-secret")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	env := filterEnv(customProxyRemoveEnvPrefixes(), "ag_custom", t.TempDir())
	for _, entry := range env {
		if strings.HasPrefix(entry, "ANTHROPIC_") || strings.HasPrefix(entry, "HTTPS_PROXY=") {
			t.Fatalf("inherited custom proxy environment was not stripped: %q", entry)
		}
	}
}

func TestAppendCustomProxyEnvAuthless(t *testing.T) {
	env := appendCustomProxyEnv(nil, "http://localhost:8080")
	if !slices.Contains(env, "ANTHROPIC_API_KEY=dummy") {
		t.Fatalf("authless env does not contain dummy API key: %v", env)
	}
}
