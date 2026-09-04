package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// extTestLogger discards output: these tests deliberately exercise the
// warn paths.
func extTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- TOML encoding for Codex -c overrides ---

func TestTOMLValueEncoding(t *testing.T) {
	if got := tomlString(`a"b`); got != `"a\"b"` {
		t.Fatalf("tomlString = %s", got)
	}
	if got := tomlStringArray([]string{"-y", `p"q`}); got != `["-y","p\"q"]` {
		t.Fatalf("tomlStringArray = %s", got)
	}
	// Keys sorted, both sides quoted: a dotted key must not split into
	// a nested config path.
	got := tomlStringTable(map[string]string{"b.c": "2", "a": "1"})
	if got != `{"a" = "1", "b.c" = "2"}` {
		t.Fatalf("tomlStringTable = %s", got)
	}
	// A value that tries to close the override and start another one
	// stays inside the string: every inner quote is escaped, so only
	// the two delimiters are bare.
	esc := tomlString(`x"` + "\n" + `mcp_servers.evil.command="sh`)
	if !strings.HasPrefix(esc, `"`) || !strings.HasSuffix(esc, `"`) {
		t.Fatalf("not a quoted string: %s", esc)
	}
	if strings.Count(esc, `"`)-strings.Count(esc, `\"`) != 2 {
		t.Fatalf("unescaped quote survived: %s", esc)
	}
	if strings.Contains(esc, "\n") {
		t.Fatalf("raw newline survived: %q", esc)
	}
	// Control characters that TOML has no escape for become \uXXXX.
	// Go's strconv.Quote would emit \a and \x7f here, neither of which
	// TOML accepts — the override would fail to parse and take the
	// whole invocation with it.
	if got := tomlString("\a\v\x7f\x00"); got != `"\u0007\u000B\u007F\u0000"` {
		t.Fatalf("control chars encoded as %s", got)
	}
	// Non-ASCII stays literal: TOML is UTF-8.
	if got := tomlString("日本語"); got != `"日本語"` {
		t.Fatalf("non-ascii mangled: %s", got)
	}
}

// --- BuildMCPServers with extension contributions ---

func TestBuildMCPServersIncludesExtensionStdio(t *testing.T) {
	t.Cleanup(func() { SetExternalMCPLookup(nil) })
	SetExternalMCPLookup(func(agentID string) []ExternalMCPServer {
		if agentID != "ag_1" {
			return nil
		}
		return []ExternalMCPServer{
			{Name: "demo_api", Command: "/pkg/bin/srv", Args: []string{"--serve"}, Env: map[string]string{"K": "V"}},
			{Name: "", Command: "/pkg/bin/x"},   // dropped: no name
			{Name: "demo_bad"},                  // dropped: no command
			{Name: "slack", Command: "/pkg/hj"}, // dropped: name already taken
		}
	})

	// An empty apiBase still yields nothing: extension servers are
	// handed KOJO_API_BASE, so they cannot run before the listener is
	// up either.
	if got := BuildMCPServers("ag_1", "", true); got != nil {
		t.Fatalf("empty apiBase = %v", got)
	}

	got := BuildMCPServers("ag_1", "http://127.0.0.1:8080", true)
	entry, ok := got["demo_api"]
	if !ok {
		t.Fatalf("extension server missing: %v", got)
	}
	if !entry.isStdio() || entry.Command != "/pkg/bin/srv" || entry.Env["K"] != "V" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Type != "stdio" {
		t.Fatalf("entry.Type = %q", entry.Type)
	}
	if _, bad := got["demo_bad"]; bad {
		t.Fatal("a server with no command was accepted")
	}
	if got["slack"].isStdio() {
		t.Fatal("an extension hijacked the built-in slack server name")
	}
	if len(got) != 2 {
		t.Fatalf("servers = %v", got)
	}
	// Another agent gets nothing.
	if other := BuildMCPServers("ag_2", "http://127.0.0.1:8080", false); len(other) != 0 {
		t.Fatalf("unbound agent got %v", other)
	}
}

// --- skill materialisation ---

func makeSkill(t *testing.T, name string, files map[string]string) ExtensionSkill {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ExtensionSkill{ExtensionID: "demo", Name: name, Dir: dir}
}

func TestSyncExtensionSkillsInstallsAndSweeps(t *testing.T) {
	skills := t.TempDir()
	sk := makeSkill(t, "demo", map[string]string{
		"SKILL.md":       "# demo\n",
		"lib/helper.txt": "x\n",
	})

	syncExtensionSkills(skills, []ExtensionSkill{sk}, extTestLogger())
	for _, rel := range []string{"demo/SKILL.md", "demo/lib/helper.txt", "demo/" + extensionSkillMarker} {
		if _, err := os.Stat(filepath.Join(skills, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s not installed: %v", rel, err)
		}
	}

	// Re-syncing the same set is stable, and picks up package edits.
	if err := os.WriteFile(filepath.Join(sk.Dir, "SKILL.md"), []byte("# updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncExtensionSkills(skills, []ExtensionSkill{sk}, extTestLogger())
	body, err := os.ReadFile(filepath.Join(skills, "demo", "SKILL.md"))
	if err != nil || string(body) != "# updated\n" {
		t.Fatalf("skill not refreshed: %q, %v", body, err)
	}

	// Dropping the contribution sweeps the copy.
	syncExtensionSkills(skills, nil, extTestLogger())
	if _, err := os.Stat(filepath.Join(skills, "demo")); !os.IsNotExist(err) {
		t.Fatalf("stale skill survived: %v", err)
	}
}

func TestSyncExtensionSkillsLeavesHandWrittenSkillsAlone(t *testing.T) {
	skills := t.TempDir()
	mine := filepath.Join(skills, "demo")
	if err := os.MkdirAll(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, "SKILL.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sk := makeSkill(t, "demo", map[string]string{"SKILL.md": "theirs\n"})
	syncExtensionSkills(skills, []ExtensionSkill{sk}, extTestLogger())
	body, err := os.ReadFile(filepath.Join(mine, "SKILL.md"))
	if err != nil || string(body) != "mine\n" {
		t.Fatalf("hand-written skill clobbered: %q, %v", body, err)
	}

	// And an unrelated hand-written skill is never swept.
	syncExtensionSkills(skills, nil, extTestLogger())
	if _, err := os.Stat(filepath.Join(mine, "SKILL.md")); err != nil {
		t.Fatalf("hand-written skill removed: %v", err)
	}
}

func TestSyncExtensionSkillsRejectsUnsafeNames(t *testing.T) {
	skills := t.TempDir()
	for _, bad := range []string{"../escape", "a/b", ".hidden", " ", ".", ".."} {
		sk := makeSkill(t, "src", map[string]string{"SKILL.md": "x\n"})
		sk.Name = bad
		syncExtensionSkills(skills, []ExtensionSkill{sk}, extTestLogger())
	}
	entries, err := os.ReadDir(skills)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsafe skill names installed: %v", entries)
	}
	// Nothing was written next to the skills dir either.
	if _, err := os.Stat(filepath.Join(filepath.Dir(skills), "escape")); err == nil {
		t.Fatal("a skill escaped its directory")
	}
}

func TestSyncExtensionSkillsFirstWriterWinsOnCollision(t *testing.T) {
	skills := t.TempDir()
	a := makeSkill(t, "dup", map[string]string{"SKILL.md": "first\n"})
	b := makeSkill(t, "dup", map[string]string{"SKILL.md": "second\n"})
	b.ExtensionID = "other"
	syncExtensionSkills(skills, []ExtensionSkill{a, b}, extTestLogger())
	body, err := os.ReadFile(filepath.Join(skills, "dup", "SKILL.md"))
	if err != nil || string(body) != "first\n" {
		t.Fatalf("collision resolved to %q (%v)", body, err)
	}
}

func TestCopySkillDirSkipsSymlinksAndOversizeFiles(t *testing.T) {
	skills := t.TempDir()
	sk := makeSkill(t, "demo", map[string]string{"SKILL.md": "# d\n"})
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(sk.Dir, "leak.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	syncExtensionSkills(skills, []ExtensionSkill{sk}, extTestLogger())
	if _, err := os.Stat(filepath.Join(skills, "demo", "leak.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink was materialised: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "demo", "SKILL.md")); err != nil {
		t.Fatalf("the rest of the skill was dropped: %v", err)
	}

	// An oversized file fails the whole copy rather than leaving a
	// half-installed skill behind.
	big := makeSkill(t, "big", map[string]string{"SKILL.md": "# b\n"})
	if err := os.WriteFile(filepath.Join(big.Dir, "blob.bin"), make([]byte, extensionSkillMaxFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	syncExtensionSkills(skills, []ExtensionSkill{big}, extTestLogger())
	if _, err := os.Stat(filepath.Join(skills, "big")); !os.IsNotExist(err) {
		t.Fatalf("oversized skill left debris: %v", err)
	}
}

func TestExtensionSkillRootPerTool(t *testing.T) {
	cases := map[string]string{
		ToolClaude:       ".claude",
		ToolCustomClaude: ".claude",
		ToolGrok:         ".claude",
		ToolCodex:        ".codex",
		ToolCustomCodex:  ".codex",
		ToolCustomBare:   "",
		"nonsense":       "",
	}
	for tool, want := range cases {
		if got := extensionSkillRoot(tool); got != want {
			t.Fatalf("extensionSkillRoot(%q) = %q, want %q", tool, got, want)
		}
	}
}
