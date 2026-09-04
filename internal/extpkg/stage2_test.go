package extpkg

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietLogger keeps supervisor churn out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func install(t *testing.T, m *Manager, manifest string, files map[string]string, ack ...string) Installed {
	t.Helper()
	all := map[string]string{ManifestFilename: manifest}
	for k, v := range files {
		all[k] = v
	}
	row, err := m.Install(context.Background(), InstallRequest{
		URL: newRepo(t, all), AckScopes: ack, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// --- tokens ---

func TestInstallMintsTokenAndHidesItFromCopies(t *testing.T) {
	m := newManager(t)
	row := install(t, m, skillsOnlyManifest, map[string]string{"skills/demo/SKILL.md": "# d\n"}, "chat:send")
	if row.Token != "" {
		t.Fatal("Install leaked the token through its returned copy")
	}
	if got, _ := m.Get("demo"); got.Token != "" {
		t.Fatal("Get leaked the token")
	}
	for _, r := range m.List() {
		if r.Token != "" {
			t.Fatal("List leaked the token")
		}
	}
	tok, err := m.Token("demo")
	if err != nil || len(tok) != tokenBytes*2 {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
	if _, err := m.Token("nope"); err == nil {
		t.Fatal("Token on an unknown id must fail")
	}
}

func TestResolveTokenGatesAndScopes(t *testing.T) {
	m := newManager(t)
	install(t, m, skillsOnlyManifest, map[string]string{"skills/demo/SKILL.md": "# d\n"}, "chat:send")
	tok, err := m.Token("demo")
	if err != nil {
		t.Fatal(err)
	}

	ident, ok := m.ResolveToken(tok)
	if !ok || ident.ID != "demo" {
		t.Fatalf("ResolveToken = %+v, %v", ident, ok)
	}
	if len(ident.Scopes) != 1 || ident.Scopes[0] != "chat:send" {
		t.Fatalf("scopes = %v", ident.Scopes)
	}
	if len(ident.AgentScope) != 0 {
		t.Fatalf("unbound package has agent scope %v", ident.AgentScope)
	}

	if _, ok := m.ResolveToken(""); ok {
		t.Fatal("empty token resolved")
	}
	if _, ok := m.ResolveToken(strings.Repeat("0", len(tok))); ok {
		t.Fatal("wrong token resolved")
	}

	// A binding shows up immediately, and disabling it withdraws it.
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if ident, _ := m.ResolveToken(tok); len(ident.AgentScope) != 1 || ident.AgentScope[0] != "ag_1" {
		t.Fatalf("agent scope = %v", ident.AgentScope)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if ident, _ := m.ResolveToken(tok); len(ident.AgentScope) != 0 {
		t.Fatalf("disabled binding still in scope: %v", ident.AgentScope)
	}

	// The kill switch has to cut API access, not just the process.
	if _, err := m.SetEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ResolveToken(tok); ok {
		t.Fatal("disabled package still authenticates")
	}
}

func TestRotateTokenInvalidatesTheOldOne(t *testing.T) {
	m := newManager(t)
	install(t, m, skillsOnlyManifest, map[string]string{"skills/demo/SKILL.md": "# d\n"}, "chat:send")
	old, err := m.Token("demo")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := m.RotateToken("demo")
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("rotation returned the same token")
	}
	if _, ok := m.ResolveToken(old); ok {
		t.Fatal("old token still resolves")
	}
	if _, ok := m.ResolveToken(fresh); !ok {
		t.Fatal("fresh token does not resolve")
	}
	if _, err := m.RotateToken("nope"); err == nil {
		t.Fatal("rotating an unknown id must fail")
	}
}

func TestSetAgentBindingRejectsUnsafeAgentID(t *testing.T) {
	m := newManager(t)
	install(t, m, skillsOnlyManifest, map[string]string{"skills/demo/SKILL.md": "# d\n"}, "chat:send")
	for _, bad := range []string{"", "../escape", "a/b", `a\b`, "."} {
		if _, err := m.SetAgentBinding("demo", bad, AgentBinding{Enabled: true}); err == nil {
			t.Fatalf("agent id %q accepted", bad)
		}
	}
}

func TestRemoveDropsDataDir(t *testing.T) {
	m := newManager(t)
	install(t, m, skillsOnlyManifest, map[string]string{"skills/demo/SKILL.md": "# d\n"}, "chat:send")
	data := m.DataDir("demo", "ag_1")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.DataDir("demo", "")); !os.IsNotExist(err) {
		t.Fatalf("data dir survived removal: %v", err)
	}
}

// --- MCP contributions ---

const mcpManifest = `{
  "id": "demo",
  "name": "Demo",
  "version": "1.0.0",
  "scopes": ["chat:send"],
  "contributes": {
    "mcpServers": [
      {"name": "api", "command": "bin/srv", "args": ["--serve"],
       "env": {"EXTRA": "1", "KOJO_EXT_TOKEN": "stolen"}},
      {"name": "npx_one", "command": "npx", "args": ["-y", "pkg"]}
    ]
  }
}`

func TestMCPServersForAgent(t *testing.T) {
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	install(t, m, mcpManifest, map[string]string{"bin/srv": "#!/bin/sh\n"}, "chat:send")

	if got := m.MCPServersForAgent(""); got != nil {
		t.Fatalf("empty agent id got %+v", got)
	}
	if got := m.MCPServersForAgent("ag_1"); len(got) != 0 {
		t.Fatalf("unbound agent got %+v", got)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{
		Enabled: true, Config: map[string]any{"channel": "#general"},
	}); err != nil {
		t.Fatal(err)
	}

	got := m.MCPServersForAgent("ag_1")
	if len(got) != 2 {
		t.Fatalf("MCPServersForAgent = %+v", got)
	}
	if got[0].Name != "demo_api" || got[1].Name != "demo_npx_one" {
		t.Fatalf("names not namespaced: %+v", got)
	}
	// A package-relative command resolves inside the checkout; a bare
	// name is left for PATH lookup.
	if want := filepath.Join(m.Dir("demo"), "bin", "srv"); got[0].Command != want {
		t.Fatalf("command = %q, want %q", got[0].Command, want)
	}
	if got[1].Command != "npx" {
		t.Fatalf("bare command rewritten to %q", got[1].Command)
	}

	env := got[0].Env
	tok, _ := m.Token("demo")
	for k, want := range map[string]string{
		"KOJO_API_BASE":       "http://127.0.0.1:8081",
		"KOJO_EXT_ID":         "demo",
		"KOJO_EXT_VERSION":    "1.0.0",
		"KOJO_EXT_DIR":        m.Dir("demo"),
		"KOJO_EXT_DATA_DIR":   m.DataDir("demo", "ag_1"),
		"KOJO_EXT_AGENT_ID":   "ag_1",
		"KOJO_EXT_TOKEN_FILE": filepath.Join(m.DataDir("demo", "ag_1"), tokenFilename),
		// The backend CLI spawns this server from command-line
		// config, so the token must not be in its environment.
		"KOJO_EXT_TOKEN": "",
		// And the CLI's own environment must not leak through: the
		// agent token would be authority the package never asked for.
		"KOJO_AGENT_TOKEN": "",
		"EXTRA":            "1",
	} {
		if env[k] != want {
			t.Fatalf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
	// The token reaches the server through a 0600 file instead.
	data, err := os.ReadFile(env["KOJO_EXT_TOKEN_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != tok {
		t.Fatalf("token file = %q, want %q", data, tok)
	}
	if info, err := os.Stat(env["KOJO_EXT_TOKEN_FILE"]); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v", info.Mode().Perm())
	}
	// Per-agent config wins over the global one.
	if !strings.Contains(env["KOJO_EXT_CONFIG"], `"channel":"#general"`) {
		t.Fatalf("KOJO_EXT_CONFIG = %q", env["KOJO_EXT_CONFIG"])
	}

	if _, err := m.SetEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if got := m.MCPServersForAgent("ag_1"); len(got) != 0 {
		t.Fatalf("disabled package still contributes MCP: %+v", got)
	}
}

// --- supervisor ---

// serviceManifest builds a manifest whose service runs bin/svc on the
// current platform.
func serviceManifest(scope string) string {
	return `{"id":"demo","name":"Demo","version":"1.0.0","contributes":{"service":{"scope":"` +
		scope + `","exec":{"` + runtime.GOOS + "/" + runtime.GOARCH + `":"bin/svc"}}}}`
}

// sleepScript is a service that stays up until it is terminated and
// records that it ran, so a test can prove the process really started.
const sleepScript = "#!/bin/sh\necho started >> \"$KOJO_EXT_DATA_DIR/../ran\" 2>/dev/null || true\nwhile true; do sleep 0.1; done\n"

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSupervisorIdleWithoutAPIBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script service")
	}
	m := newManager(t)
	install(t, m, serviceManifest(ScopeGlobal), map[string]string{"bin/svc": sleepScript})
	sup := NewSupervisor(m, quietLogger())
	defer sup.Shutdown()
	sup.Reconcile()
	if got := sup.Running(); len(got) != 0 {
		t.Fatalf("started a service with no API base: %+v", got)
	}
	// And it comes up as soon as the listener is known.
	m.SetAPIBase("http://127.0.0.1:8081")
	sup.Reconcile()
	if got := sup.Running(); len(got) != 1 || got[0].ExtensionID != "demo" {
		t.Fatalf("Running() = %+v", got)
	}
}

func TestSupervisorGlobalServiceLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script service")
	}
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	install(t, m, serviceManifest(ScopeGlobal), map[string]string{"bin/svc": sleepScript})
	if err := os.MkdirAll(m.DataDir("demo", ""), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := NewSupervisor(m, quietLogger())
	defer sup.Shutdown()

	sup.Reconcile()
	ranMarker := filepath.Join(m.DataDir("demo", ""), "..", "ran")
	waitFor(t, "the service process to run", func() bool {
		_, err := os.Stat(ranMarker)
		return err == nil
	})

	// Reconciling again must not churn a healthy process.
	before := sup.Running()
	sup.Reconcile()
	if after := sup.Running(); len(after) != 1 || len(before) != 1 || !after[0].equal(before[0]) {
		t.Fatalf("idempotent reconcile changed the process set: %+v -> %+v", before, after)
	}

	// Disabling the package stops it.
	if _, err := m.SetEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	sup.Reconcile()
	if got := sup.Running(); len(got) != 0 {
		t.Fatalf("disabled package still supervised: %+v", got)
	}
}

func TestSupervisorPerAgentServiceFollowsBindings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script service")
	}
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	install(t, m, serviceManifest(ScopePerAgent), map[string]string{"bin/svc": sleepScript})
	sup := NewSupervisor(m, quietLogger())
	defer sup.Shutdown()

	sup.Reconcile()
	if got := sup.Running(); len(got) != 0 {
		t.Fatalf("per-agent service ran with no bindings: %+v", got)
	}

	for _, id := range []string{"ag_1", "ag_2"} {
		if _, err := m.SetAgentBinding("demo", id, AgentBinding{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	sup.Reconcile()
	got := sup.Running()
	if len(got) != 2 || got[0].AgentID != "ag_1" || got[1].AgentID != "ag_2" {
		t.Fatalf("Running() = %+v", got)
	}

	// An archived (filtered-out) agent loses its process.
	sup.SetAgentFilter(func(agentID string) bool { return agentID != "ag_2" })
	sup.Reconcile()
	if got := sup.Running(); len(got) != 1 || got[0].AgentID != "ag_1" {
		t.Fatalf("agent filter not applied: %+v", got)
	}

	// A config change restarts the process rather than leaving the
	// old environment in place.
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{
		Enabled: true, Config: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatal(err)
	}
	sup.Reconcile()
	got = sup.Running()
	if len(got) != 1 {
		t.Fatalf("Running() = %+v", got)
	}
	var cfg string
	for _, kv := range got[0].Env {
		if strings.HasPrefix(kv, "KOJO_EXT_CONFIG=") {
			cfg = kv
		}
	}
	if !strings.Contains(cfg, `"k":"v"`) {
		t.Fatalf("respawned service kept the old config: %q", cfg)
	}
}

func TestSupervisorShutdownIsFinal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script service")
	}
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	install(t, m, serviceManifest(ScopeGlobal), map[string]string{"bin/svc": sleepScript})
	sup := NewSupervisor(m, quietLogger())
	sup.Reconcile()
	if len(sup.Running()) != 1 {
		t.Fatal("service did not start")
	}
	sup.Shutdown()
	if got := sup.Running(); len(got) != 0 {
		t.Fatalf("Shutdown left processes: %+v", got)
	}
	// A late Reconcile after shutdown must not resurrect anything.
	sup.Reconcile()
	if got := sup.Running(); len(got) != 0 {
		t.Fatalf("Reconcile after Shutdown restarted services: %+v", got)
	}
}

func TestServiceEnvIsAllowlistedAndComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix env allowlist")
	}
	t.Setenv("KOJO_SECRET_SAUCE", "leak-me")
	m := newManager(t)
	m.SetAPIBase("http://127.0.0.1:8081")
	manifest := `{"id":"demo","name":"Demo","version":"1.0.0","contributes":{"service":{"scope":"global",` +
		`"exec":{"` + runtime.GOOS + "/" + runtime.GOARCH + `":"bin/svc"},"args":["--flag"],` +
		`"env":{"MINE":"yes","KOJO_EXT_ID":"spoofed"}}}}`
	install(t, m, manifest, map[string]string{"bin/svc": sleepScript})
	sup := NewSupervisor(m, quietLogger())
	defer sup.Shutdown()
	sup.Reconcile()
	got := sup.Running()
	if len(got) != 1 {
		t.Fatalf("Running() = %+v", got)
	}
	env := map[string]string{}
	for _, kv := range got[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if _, leaked := env["KOJO_SECRET_SAUCE"]; leaked {
		t.Fatal("the whole server environment was handed to the service")
	}
	if env["KOJO_EXT_ID"] != "demo" {
		t.Fatalf("manifest env spoofed a reserved variable: %q", env["KOJO_EXT_ID"])
	}
	if env["MINE"] != "yes" {
		t.Fatal("manifest env dropped")
	}
	if env["KOJO_EXT_AGENT_ID"] != "" {
		t.Fatalf("global service got an agent id: %q", env["KOJO_EXT_AGENT_ID"])
	}
	if len(got[0].Args) != 1 || got[0].Args[0] != "--flag" {
		t.Fatalf("args = %v", got[0].Args)
	}
}

func TestDataDirLayout(t *testing.T) {
	m := newManager(t)
	global := m.DataDir("demo", "")
	perAgent := m.DataDir("demo", "ag_1")
	if filepath.Base(global) != "demo" {
		t.Fatalf("global data dir = %q", global)
	}
	if want := filepath.Join(global, "agents", "ag_1"); perAgent != want {
		t.Fatalf("per-agent data dir = %q, want %q", perAgent, want)
	}
	// Data lives outside the checkout so an update cannot wipe it.
	if strings.HasPrefix(global, m.Dir("demo")) {
		t.Fatalf("data dir %q sits inside the checkout", global)
	}
}

// A service that writes a huge line without a newline must not be able
// to make the reader allocate the whole thing: the line is capped, the
// remainder is counted, and the reader keeps going for the next line.
func TestPipeToLogCapsUnboundedLine(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	h := slog.NewTextHandler(io.Discard, nil)
	log := slog.New(captureHandler{Handler: h, mu: &mu, lines: &lines})

	huge := strings.Repeat("x", logLineMax*3)
	pipeToLog(strings.NewReader(huge+"\nafter\n"), log, slog.LevelInfo)

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], strings.Repeat("x", 64)) ||
		!strings.Contains(lines[0], "truncated") {
		t.Fatalf("first line not truncated: %.120q", lines[0])
	}
	if got := len(lines[0]); got > logLineMax+64 {
		t.Fatalf("truncated line is %d bytes, over the %d cap", got, logLineMax)
	}
	// The stream survives the over-long line rather than stopping on it.
	if lines[1] != "after" {
		t.Fatalf("second line = %q", lines[1])
	}
}

// Output with no trailing newline still reaches the log when the pipe
// closes; a service killed mid-line loses nothing it already wrote.
func TestPipeToLogFlushesPartialFinalLine(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	log := slog.New(captureHandler{Handler: slog.NewTextHandler(io.Discard, nil), mu: &mu, lines: &lines})
	pipeToLog(strings.NewReader("no newline here"), log, slog.LevelWarn)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "no newline here" {
		t.Fatalf("lines = %q", lines)
	}
}

// captureHandler records the "line" attribute of every record so the
// pipe tests can assert on what was logged rather than on formatting.
type captureHandler struct {
	slog.Handler
	mu    *sync.Mutex
	lines *[]string
}

func (c captureHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "line" {
			c.mu.Lock()
			*c.lines = append(*c.lines, a.Value.String())
			c.mu.Unlock()
			return false
		}
		return true
	})
	return nil
}
