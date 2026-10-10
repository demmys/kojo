package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHoldGrokMCPConfig_WritesAndRemovesManagedBlock(t *testing.T) {
	dir := t.TempDir()
	servers := map[string]mcpServerEntry{
		"kojo-ext-discord": {Command: "/opt/ext/bin/discord", Args: []string{"mcp"}, Env: map[string]string{"KOJO_EXT_TOKEN": `a"b\c`}},
		"kojo":             {Type: "http", URL: "http://127.0.0.1:8081/mcp", Headers: map[string]string{"X-Kojo-Token": "tok"}},
	}
	n, err := syncForTest(t, dir, servers)
	if err != nil || n != 2 {
		t.Fatalf("sync = %d, %v; want 2, nil", n, err)
	}
	path := grokMCPConfigPath(dir)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[mcp_servers."kojo-ext-discord"]`,
		`command = "/opt/ext/bin/discord"`,
		`args = ["mcp"]`,
		`env = {"KOJO_EXT_TOKEN" = "a\"b\\c"}`,
		`[mcp_servers."kojo"]`,
		`url = "http://127.0.0.1:8081/mcp"`,
		`headers = {"X-Kojo-Token" = "tok"}`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// Unchanged input leaves the file untouched.
	before, _ := os.Stat(path)
	if n, err := syncForTest(t, dir, servers); err != nil || n != 2 {
		t.Fatalf("second sync = %d, %v", n, err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("identical config was rewritten")
	}

	// No servers: the file kojo created goes away.
	if n, err := syncForTest(t, dir, nil); err != nil || n != 0 {
		t.Fatalf("empty sync = %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config not removed: %v", err)
	}
}

func TestHoldGrokMCPConfig_KeepsOperatorContent(t *testing.T) {
	dir := t.TempDir()
	path := grokMCPConfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	own := "[models]\ndefault = \"grok-4.5\"\n\n[mcp_servers.mine]\ncommand = \"/bin/mine\"\n"
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	servers := map[string]mcpServerEntry{
		"mine":  {Command: "/bin/kojo-mine"},
		"other": {Command: "/bin/other"},
	}
	n, err := syncForTest(t, dir, servers)
	if err != nil || n != 1 {
		t.Fatalf("sync = %d, %v; want 1, nil", n, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), own) {
		t.Errorf("operator content changed:\n%s", got)
	}
	if strings.Contains(string(got), "/bin/kojo-mine") {
		t.Errorf("operator-defined server was redeclared:\n%s", got)
	}
	if !strings.Contains(string(got), `[mcp_servers."other"]`) {
		t.Errorf("missing managed server:\n%s", got)
	}

	if n, err := syncForTest(t, dir, nil); err != nil || n != 0 {
		t.Fatalf("empty sync = %d, %v", n, err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != own {
		t.Errorf("after removing the block: %q, want %q", got, own)
	}
}

func TestStripGrokMCPBlock_NoEndMarker(t *testing.T) {
	in := "a = 1\n" + grokMCPBlockStart + "\n[mcp_servers.\"x\"]\ncommand = \"c\"\n"
	if got, err := stripGrokMCPBlock(in); err == nil {
		t.Errorf("no error; got %q", got)
	}
}

func TestHoldGrokMCPConfig_InlineTableIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := grokMCPConfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	own := "mcp_servers = { mine = { command = \"/bin/mine\" } }\n"
	if err := os.WriteFile(path, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := syncForTest(t, dir, map[string]mcpServerEntry{"other": {Command: "/bin/other"}})
	if err != nil || n != 0 {
		t.Fatalf("sync = %d, %v; want 0, nil", n, err)
	}
	if got, _ := os.ReadFile(path); string(got) != own {
		t.Errorf("file changed: %q", got)
	}
}

func TestBuildGrokArgs_TrustOnlyWithManagedMCP(t *testing.T) {
	a := &Agent{ID: "ag_grok"}
	if stringSliceContains(buildGrokArgs("/p", "/d", "", a, "", false), "--trust") {
		t.Error("--trust without managed MCP servers")
	}
	if !stringSliceContains(buildGrokArgs("/p", "/d", "", a, "", true), "--trust") {
		t.Error("missing --trust with managed MCP servers")
	}
}

func syncForTest(t *testing.T, dir string, servers map[string]mcpServerEntry) (int, error) {
	t.Helper()
	n, release, err := holdGrokMCPConfig(context.Background(), dir, servers)
	if err == nil {
		release()
	}
	return n, err
}

func TestGrokOperatorMCPNames(t *testing.T) {
	own := `mcp_servers.a = { command = "x" }
mcp_servers."b c".command = "x"

[mcp_servers]
d = { url = "u" }
e.command = "x"

[mcp_servers.f.env]
K = "v"

[mcp_servers.'g']
command = "x"

[other]
h = 1
`
	own += "\n[\"mcp_servers\".i]\ncommand = \"x\"\n\n['mcp_servers']\nj = { url = \"u\" }\n"
	got, closed := grokOperatorMCPNames(own)
	if closed {
		t.Error("closed without an inline table")
	}
	for _, name := range []string{"a", "b c", "d", "e", "f", "g", "i", "j"} {
		if !got[name] {
			t.Errorf("missing %q in %v", name, got)
		}
	}
	if got["h"] || got["K"] || got["command"] {
		t.Errorf("unexpected names: %v", got)
	}
}

func TestStripGrokMCPBlock_MarkerInsideALine(t *testing.T) {
	in := "note = \"" + grokMCPBlockStart + "\"\n"
	if got, err := stripGrokMCPBlock(in); err != nil || got != in {
		t.Errorf("got %q", got)
	}
}

func TestHoldGrokMCPConfig_DifferentServersWait(t *testing.T) {
	dir := t.TempDir()
	a := map[string]mcpServerEntry{"a": {Command: "/bin/a"}}
	b := map[string]mcpServerEntry{"b": {Command: "/bin/b"}}

	_, releaseA1, err := holdGrokMCPConfig(context.Background(), dir, a)
	if err != nil {
		t.Fatal(err)
	}
	// The same servers share the file.
	_, releaseA2, err := holdGrokMCPConfig(context.Background(), dir, a)
	if err != nil {
		t.Fatal(err)
	}

	// Different ones wait, and give up with their context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := holdGrokMCPConfig(ctx, dir, b); err == nil {
		t.Fatal("a second server set was written under a running turn")
	}

	done := make(chan error, 1)
	go func() {
		_, release, err := holdGrokMCPConfig(context.Background(), dir, b)
		if err == nil {
			release()
		}
		done <- err
	}()
	releaseA1()
	releaseA1() // idempotent
	select {
	case <-done:
		t.Fatal("did not wait for the second holder")
	case <-time.After(20 * time.Millisecond):
	}
	got, _ := os.ReadFile(grokMCPConfigPath(dir))
	if !strings.Contains(string(got), "/bin/a") {
		t.Fatalf("config changed under a running turn:\n%s", got)
	}
	releaseA2()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(grokMCPConfigPath(dir))
	if !strings.Contains(string(got), "/bin/b") || strings.Contains(string(got), "/bin/a") {
		t.Fatalf("config after the waiter ran:\n%s", got)
	}
}

func TestStripGrokMCPBlock_EveryBlock(t *testing.T) {
	block := grokMCPBlockStart + "\n[mcp_servers.\"x\"]\n" + grokMCPBlockEnd + "\n"
	in := "a = 1\n\n" + block + "b = 2\n\n" + block
	got, err := stripGrokMCPBlock(in)
	if err != nil || got != "a = 1\nb = 2\n" {
		t.Errorf("got %q, %v", got, err)
	}
}
