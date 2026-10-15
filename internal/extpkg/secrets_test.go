package extpkg

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memSecrets is an in-memory SecretStore keyed the way the credential
// store's primary key is.
type memSecrets struct {
	rows map[string]string
	fail bool
	// failRead makes every read fail the way an undecryptable row does.
	failRead bool
}

func newMemSecrets() *memSecrets { return &memSecrets{rows: map[string]string{}} }

func secretRowKey(provider, agentID, sourceID, key string) string {
	return strings.Join([]string{provider, agentID, sourceID, key}, "\x00")
}

func (s *memSecrets) GetToken(provider, agentID, sourceID, key string) (string, error) {
	if s.failRead {
		return "", errors.New("cannot decrypt")
	}
	v, ok := s.rows[secretRowKey(provider, agentID, sourceID, key)]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func (s *memSecrets) SetToken(provider, agentID, sourceID, key, value string, _ time.Time) error {
	if s.fail {
		return errors.New("store down")
	}
	s.rows[secretRowKey(provider, agentID, sourceID, key)] = value
	return nil
}

func (s *memSecrets) DeleteToken(provider, agentID, sourceID, key string) error {
	delete(s.rows, secretRowKey(provider, agentID, sourceID, key))
	return nil
}

func (s *memSecrets) DeleteTokensBySource(provider, agentID, sourceID string) error {
	prefix := secretRowKey(provider, agentID, sourceID, "")
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			delete(s.rows, k)
		}
	}
	return nil
}

const secretManifest = `{
  "id": "demo", "name": "Demo", "version": "1.0.0",
  "contributes": {
    "skills": ["skills/demo"],
    "settings": { "scope": "per-agent", "schema": "settings.schema.json" }
  }
}`

const secretGlobalManifest = `{
  "id": "demo", "name": "Demo", "version": "1.0.0",
  "contributes": {
    "skills": ["skills/demo"],
    "settings": { "scope": "global", "schema": "settings.schema.json" }
  }
}`

const secretSchema = `{
  "type": "object",
  "properties": {
    "botToken": { "type": "string", "format": "password" },
    "channel":  { "type": "string" },
    "count":    { "type": "integer", "format": "password" }
  }
}`

func secretFiles() map[string]string {
	return map[string]string{
		"skills/demo/SKILL.md": "# d\n",
		"settings.schema.json": secretSchema,
	}
}

func TestSecretKeysFromSchema(t *testing.T) {
	got := secretKeysFromSchema([]byte(secretSchema))
	if len(got) != 1 || got[0] != "botToken" {
		t.Fatalf("secret keys = %v, want [botToken] (only string properties count)", got)
	}
	if secretKeysFromSchema([]byte("not json")) != nil {
		t.Fatal("an unparsable schema must yield no secrets")
	}
}

func TestAgentSecretsNeverReachTheRegistry(t *testing.T) {
	m := newManager(t)
	store := newMemSecrets()
	m.SetSecretStore(store)
	install(t, m, secretManifest, secretFiles())

	row, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{
		Enabled: true,
		Config:  map[string]any{"botToken": "s3cr3t-value", "channel": "general"},
		// A caller-supplied name list is ignored.
		Secrets: []string{"forged"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := row.Agents["ag_1"]
	if _, leaked := b.Config["botToken"]; leaked {
		t.Fatal("secret stayed in the binding config")
	}
	if b.Config["channel"] != "general" {
		t.Fatalf("plain setting lost: %v", b.Config)
	}
	if len(b.Secrets) != 1 || b.Secrets[0] != "botToken" {
		t.Fatalf("secret names = %v", b.Secrets)
	}
	data, err := os.ReadFile(filepath.Join(m.Root(), stateFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cr3t-value") {
		t.Fatal("secret value was written to the registry file")
	}
	api, _ := json.Marshal(row)
	if strings.Contains(string(api), "s3cr3t-value") {
		t.Fatal("secret value appears in the API copy")
	}

	got, err := m.Secrets("demo", "ag_1")
	if err != nil || got["botToken"] != "s3cr3t-value" {
		t.Fatalf("Secrets = %v, %v", got, err)
	}
	if other, _ := m.Secrets("demo", "ag_2"); len(other) != 0 {
		t.Fatalf("another agent saw the secret: %v", other)
	}

	// An untouched form field arrives empty (or not at all): keep.
	for _, cfg := range []map[string]any{
		{"botToken": "", "channel": "random"},
		{"channel": "random"},
	} {
		row, err = m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "s3cr3t-value" {
			t.Fatalf("resubmitting without the secret dropped it: %v", got)
		}
		if len(row.Agents["ag_1"].Secrets) != 1 {
			t.Fatalf("secret name list lost: %v", row.Agents["ag_1"].Secrets)
		}
	}

	// null forgets it.
	row, err = m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Agents["ag_1"].Secrets) != 0 {
		t.Fatalf("secret name survived a null: %v", row.Agents["ag_1"].Secrets)
	}
	if got, _ := m.Secrets("demo", "ag_1"); len(got) != 0 {
		t.Fatalf("secret survived a null: %v", got)
	}
	if len(store.rows) != 0 {
		t.Fatalf("store still holds rows: %v", store.rows)
	}
}

func TestSecretRejectedWhenItCannotBeStored(t *testing.T) {
	m := newManager(t)
	install(t, m, secretManifest, secretFiles())
	// No store wired: refuse rather than fall back to plaintext.
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "x"}}); err == nil {
		t.Fatal("a secret was accepted with no store to keep it in")
	}
	if row, _ := m.Get("demo"); len(row.Agents) != 0 {
		t.Fatalf("failed binding was persisted: %v", row.Agents)
	}

	store := newMemSecrets()
	store.fail = true
	m.SetSecretStore(store)
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "x"}}); err == nil {
		t.Fatal("a store failure was swallowed")
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": 7}}); err == nil {
		t.Fatal("a non-string secret was accepted")
	}
}

func TestGlobalSecretsAndCleanup(t *testing.T) {
	m := newManager(t)
	store := newMemSecrets()
	m.SetSecretStore(store)
	install(t, m, secretGlobalManifest, secretFiles())

	row, err := m.SetConfig("demo", map[string]any{"botToken": "global", "channel": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := row.Config["botToken"]; leaked || len(row.Secrets) != 1 {
		t.Fatalf("global secret handling: config=%v secrets=%v", row.Config, row.Secrets)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "mine"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Secrets("demo", ""); got["botToken"] != "global" {
		t.Fatalf("global read = %v", got)
	}
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "mine" {
		t.Fatalf("agent secret must shadow the global one: %v", got)
	}
	if got, _ := m.Secrets("demo", "ag_2"); got["botToken"] != "global" {
		t.Fatalf("an agent without its own secret falls back to global: %v", got)
	}

	if err := m.ForgetAgent("ag_1"); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("ForgetAgent left the agent's secret behind: %v", store.rows)
	}
	if _, err := m.SetAgentBinding("demo", "ag_3", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "three"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("Remove left secrets behind: %v", store.rows)
	}
	if _, err := m.Secrets("demo", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Secrets on a removed package = %v", err)
	}
}

func TestServiceReceivesSecretsOutsideItsConfig(t *testing.T) {
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	m.SetSecretStore(newMemSecrets())
	row := install(t, m, secretManifest, secretFiles())
	row, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{
		Enabled: true, Config: map[string]any{"botToken": "tok", "channel": "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sup := NewSupervisor(m, quietLogger())
	spec := sup.spec(row, &Service{Scope: ScopePerAgent}, "/bin/true", "t", "ag_1")
	env := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["KOJO_EXT_SECRETS"] != `{"botToken":"tok"}` {
		t.Fatalf("KOJO_EXT_SECRETS = %q", env["KOJO_EXT_SECRETS"])
	}
	if strings.Contains(env["KOJO_EXT_CONFIG"], "tok") || !strings.Contains(env["KOJO_EXT_CONFIG"], `"channel":"c"`) {
		t.Fatalf("KOJO_EXT_CONFIG = %q", env["KOJO_EXT_CONFIG"])
	}
	if env["KOJO_UPLOAD_DIR"] == "" {
		t.Fatal("KOJO_UPLOAD_DIR missing from the service contract")
	}
	// The MCP path must not carry secrets at all: its environment is
	// visible on the backend CLI's command line.
	for _, srv := range m.MCPServersForAgent("ag_1") {
		for k, v := range srv.Env {
			if strings.Contains(v, "tok") && k != "KOJO_EXT_TOKEN_FILE" {
				t.Fatalf("mcp env %s leaks a secret", k)
			}
		}
	}
}

func TestSecretWriteIsAllOrNothing(t *testing.T) {
	m := newManager(t)
	store := newMemSecrets()
	m.SetSecretStore(store)
	install(t, m, secretManifest, secretFiles())
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "first"}}); err != nil {
		t.Fatal(err)
	}

	// The registry cannot be written: the store must end up unchanged.
	statePath := filepath.Join(m.Root(), stateFilename)
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "second"}}); err == nil {
		t.Fatal("a failed registry save was reported as success")
	}
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "first" {
		t.Fatalf("a failed save left the new secret behind: %v", got)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": nil}}); err == nil {
		t.Fatal("a failed registry save was reported as success")
	}
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "first" {
		t.Fatalf("a failed save still deleted the secret: %v", got)
	}
	if err := os.RemoveAll(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, saved, 0o600); err != nil {
		t.Fatal(err)
	}

	// A value that fails validation leaves the store alone too.
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": strings.Repeat("x", maxSecretLen+1)}}); err == nil {
		t.Fatal("an oversized secret was accepted")
	}
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "first" {
		t.Fatalf("a rejected write changed the stored secret: %v", got)
	}
}

func TestPlaintextSecretsAreMigrated(t *testing.T) {
	m := newManager(t)
	// Installed and configured before any store existed, with a schema
	// that did not yet mark the token secret.
	files := secretFiles()
	files["settings.schema.json"] = `{"type":"object","properties":{"botToken":{"type":"string"},"channel":{"type":"string"}}}`
	install(t, m, secretManifest, files)
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "old-plain", "channel": "general"}}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(m.Root(), stateFilename)
	if data, _ := os.ReadFile(statePath); !strings.Contains(string(data), "old-plain") {
		t.Fatal("setup: the plain value should be in the registry")
	}

	// The package now declares it a password.
	if err := os.WriteFile(filepath.Join(pkgPath(m.Root(), "demo"), "settings.schema.json"), []byte(secretSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newMemSecrets()
	m.SetSecretStore(store)

	if data, _ := os.ReadFile(statePath); strings.Contains(string(data), "old-plain") {
		t.Fatal("the plaintext secret is still in the registry file")
	}
	row, _ := m.Get("demo")
	b := row.Agents["ag_1"]
	if _, leaked := b.Config["botToken"]; leaked {
		t.Fatal("the plaintext secret is still in the binding config")
	}
	if b.Config["channel"] != "general" || len(b.Secrets) != 1 || b.Secrets[0] != "botToken" {
		t.Fatalf("binding after migration = %+v", b)
	}
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "old-plain" {
		t.Fatalf("migrated secret = %v", got)
	}
}

func TestUnreadableSecretIsNotOverwritten(t *testing.T) {
	m := newManager(t)
	store := newMemSecrets()
	m.SetSecretStore(store)
	install(t, m, secretManifest, secretFiles())
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "first"}}); err != nil {
		t.Fatal(err)
	}
	store.failRead = true
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": "second"}}); err == nil {
		t.Fatal("a secret that could not be read back was overwritten")
	}
	store.failRead = false
	if got, _ := m.Secrets("demo", "ag_1"); got["botToken"] != "first" {
		t.Fatalf("stored secret changed: %v", got)
	}
	// Forgetting it is the way out.
	store.failRead = true
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"botToken": nil}}); err != nil {
		t.Fatalf("an unreadable secret could not be removed: %v", err)
	}
	store.failRead = false
	if got, _ := m.Secrets("demo", "ag_1"); len(got) != 0 {
		t.Fatalf("secret survived removal: %v", got)
	}
}
