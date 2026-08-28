package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newRepo creates a git repository containing the given files and
// returns its path. Files are written relative to the repo root.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_NOSYSTEM=1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	writeFiles(t, dir, files)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// commitAll stages and commits the current working tree of repo.
func commitAll(t *testing.T, repo, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_NOSYSTEM=1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

const skillsOnlyManifest = `{
  "id": "demo",
  "name": "Demo",
  "version": "1.0.0",
  "kojoVersion": ">=0.100.0",
  "scopes": ["chat:send"],
  "contributes": { "skills": ["skills/demo"] }
}`

func skillsOnlyRepo(t *testing.T) string {
	return newRepo(t, map[string]string{
		ManifestFilename:       skillsOnlyManifest,
		"skills/demo/SKILL.md": "# demo skill\n",
	})
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "v0.127.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- manifest validation ---

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest([]byte(skillsOnlyManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "demo" || m.Name != "Demo" {
		t.Fatalf("unexpected manifest %+v", m)
	}
	if got := m.ScopeSummaries(); len(got) != 1 || got[0].Description == "" {
		t.Fatalf("scope summaries = %+v", got)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"bad id":            `{"id":"Demo","name":"n","version":"1.0.0"}`,
		"empty name":        `{"id":"demo","name":" ","version":"1.0.0"}`,
		"bad version":       `{"id":"demo","name":"n","version":"1.0"}`,
		"unknown scope":     `{"id":"demo","name":"n","version":"1.0.0","scopes":["root:everything"]}`,
		"duplicate scope":   `{"id":"demo","name":"n","version":"1.0.0","scopes":["chat:send","chat:send"]}`,
		"skill escape":      `{"id":"demo","name":"n","version":"1.0.0","contributes":{"skills":["../etc"]}}`,
		"absolute skill":    `{"id":"demo","name":"n","version":"1.0.0","contributes":{"skills":["/etc"]}}`,
		"bad service scope": `{"id":"demo","name":"n","version":"1.0.0","contributes":{"service":{"scope":"weird","exec":{"linux/amd64":"bin/x"}}}}`,
		"empty exec":        `{"id":"demo","name":"n","version":"1.0.0","contributes":{"service":{"scope":"global","exec":{}}}}`,
		"bad exec key":      `{"id":"demo","name":"n","version":"1.0.0","contributes":{"service":{"scope":"global","exec":{"linux":"bin/x"}}}}`,
		"bad constraint":    `{"id":"demo","name":"n","version":"1.0.0","kojoVersion":"~>1.0"}`,
		"mcp no command":    `{"id":"demo","name":"n","version":"1.0.0","contributes":{"mcpServers":[{"name":"x"}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(body)); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

// --- version constraints ---

func TestSatisfiesKojoVersion(t *testing.T) {
	cases := []struct {
		constraint string
		current    string
		want       bool
	}{
		{"", "v0.1.0", true},
		{">=0.127.0", "v0.127.0", true},
		{">=0.127.0", "v0.126.9", false},
		{">=0.127.0 <1.0.0", "v0.999.0", true},
		{">=0.127.0 <1.0.0", "v1.0.0", false},
		{"=0.127.0", "v0.127.0", true},
		{"=0.127.0", "v0.127.1", false},
		// Unparseable current versions (dev builds) must not block
		// installation or the feature is untestable locally.
		{">=0.127.0", "dev", true},
	}
	for _, tc := range cases {
		m := &Manifest{KojoVersion: tc.constraint}
		got, err := m.SatisfiesKojoVersion(tc.current)
		if err != nil {
			t.Fatalf("%q vs %q: %v", tc.constraint, tc.current, err)
		}
		if got != tc.want {
			t.Fatalf("%q vs %q = %v, want %v", tc.constraint, tc.current, got, tc.want)
		}
	}
}

// --- source URL validation ---

func TestValidateSourceURL(t *testing.T) {
	ok := []string{
		"https://github.com/o/r.git",
		"ssh://git@github.com/o/r.git",
		"git@github.com:o/r.git",
		"/tmp/local-repo",
		"file:///tmp/local-repo",
	}
	for _, u := range ok {
		if err := ValidateSourceURL(u); err != nil {
			t.Fatalf("%q rejected: %v", u, err)
		}
	}
	bad := []string{"", "--upload-pack=touch /tmp/pwn", "ext::sh -c whoami", "repo\nrm -rf /",
		// Unauthenticated transports: a MITM would be installing an
		// executable that kojo then runs.
		"http://example.com/o/r.git", "git://example.com/o/r.git"}
	for _, u := range bad {
		if err := ValidateSourceURL(u); err == nil {
			t.Fatalf("%q accepted", u)
		}
	}
	if err := ValidateRef("--upload-pack=x"); err == nil {
		t.Fatal("option-like ref accepted")
	}
	if err := ValidateRef(""); err != nil {
		t.Fatalf("empty ref rejected: %v", err)
	}
}

// --- install / update / remove ---

func TestInstallSkillsOnlyPackage(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)

	mf, commit, err := m.Preview(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if mf.ID != "demo" {
		t.Fatalf("preview id = %q", mf.ID)
	}
	if commit == "" {
		t.Fatal("preview returned no commit")
	}
	// A commit that no longer matches is refused, so an install can
	// never land code the operator did not just approve.
	if _, err := m.Install(context.Background(), InstallRequest{
		URL: repo, Commit: "0000000000000000000000000000000000000000",
		AckScopes: []string{"chat:send"}, Enabled: true,
	}); err == nil {
		t.Fatal("install accepted a stale commit pin")
	}
	// Preview must not register anything.
	if got := m.List(); len(got) != 0 {
		t.Fatalf("preview installed %d extensions", len(got))
	}

	row, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.Commit == "" {
		t.Fatal("commit not recorded")
	}
	if _, err := os.Stat(filepath.Join(m.Dir("demo"), "skills", "demo", "SKILL.md")); err != nil {
		t.Fatalf("skill not checked out: %v", err)
	}
	if got := m.List(); len(got) != 1 || got[0].ID != "demo" {
		t.Fatalf("List() = %+v", got)
	}

	// A second install of the same ID must be refused rather than
	// silently clobbering the operator's configuration.
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}}); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("duplicate install err = %v", err)
	}
}

func TestInstallStatePersistsAcrossManagers(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, "v0.127.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	m2, err := NewManager(root, "v0.127.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := m2.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !row.Enabled || !row.Agents["ag_1"].Enabled {
		t.Fatalf("state not persisted: %+v", row)
	}
}

func TestInstallScopeAcknowledgement(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	_, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: nil})
	var mismatch *ScopeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ScopeMismatchError", err)
	}
	if len(mismatch.Missing) != 1 || mismatch.Missing[0] != "chat:send" {
		t.Fatalf("missing = %v", mismatch.Missing)
	}
	if mismatch.Manifest == nil {
		t.Fatal("manifest not attached to mismatch error")
	}
	// Acknowledging something the package never asked for is also a
	// mismatch: the consent dialog and the manifest disagree.
	_, err = m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send", "agents:write"}})
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ScopeMismatchError", err)
	}
	if len(mismatch.Extra) != 1 || mismatch.Extra[0] != "agents:write" {
		t.Fatalf("extra = %v", mismatch.Extra)
	}
	if got := m.List(); len(got) != 0 {
		t.Fatalf("failed install left %d rows", len(got))
	}
}

func TestInstallRejectsIncompatibleKojoVersion(t *testing.T) {
	m, err := NewManager(t.TempDir(), "v0.50.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}}); err == nil ||
		!strings.Contains(err.Error(), "requires kojo") {
		t.Fatalf("err = %v, want version incompatibility", err)
	}
}

func TestInstallRejectsMissingContributionFiles(t *testing.T) {
	// Manifest promises a skill directory the repository does not have.
	repo := newRepo(t, map[string]string{ManifestFilename: skillsOnlyManifest})
	m := newManager(t)
	_, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}})
	if err == nil || !strings.Contains(err.Error(), "contributes.skills") {
		t.Fatalf("err = %v, want missing-skill rejection", err)
	}
}

func TestInstallRejectsMissingManifest(t *testing.T) {
	repo := newRepo(t, map[string]string{"README.md": "nothing here"})
	m := newManager(t)
	_, err := m.Install(context.Background(), InstallRequest{URL: repo})
	if err == nil || !strings.Contains(err.Error(), ManifestFilename) {
		t.Fatalf("err = %v, want missing-manifest rejection", err)
	}
}

func TestInstallRejectsForeignPlatformService(t *testing.T) {
	manifest := `{"id":"demo","name":"Demo","version":"1.0.0",
	  "contributes":{"service":{"scope":"global","exec":{"plan9/mips":"bin/x"}}}}`
	repo := newRepo(t, map[string]string{ManifestFilename: manifest, "bin/x": "#!/bin/sh\n"})
	m := newManager(t)
	_, err := m.Install(context.Background(), InstallRequest{URL: repo})
	if err == nil || !strings.Contains(err.Error(), runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("err = %v, want platform rejection", err)
	}
}

func TestInstallServiceMakesEntrypointExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	key := runtime.GOOS + "/" + runtime.GOARCH
	manifest := `{"id":"demo","name":"Demo","version":"1.0.0",
	  "contributes":{"service":{"scope":"global","exec":{"` + key + `":"bin/x"}}}}`
	repo := newRepo(t, map[string]string{ManifestFilename: manifest, "bin/x": "#!/bin/sh\nexit 0\n"})
	m := newManager(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(m.Dir("demo"), "bin", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("entry point not executable: %v", info.Mode())
	}
}

func TestUpdatePreservesConfiguration(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"channel": "#general"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Get("demo")

	writeFiles(t, repo, map[string]string{
		ManifestFilename:       strings.Replace(skillsOnlyManifest, `"version": "1.0.0"`, `"version": "1.1.0"`, 1),
		"skills/demo/SKILL.md": "# demo skill v2\n",
	})
	commitAll(t, repo, "v1.1.0")

	after, err := m.Update(context.Background(), "demo", UpdateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Manifest.Version != "1.1.0" {
		t.Fatalf("version = %q, want 1.1.0", after.Manifest.Version)
	}
	if after.Commit == before.Commit {
		t.Fatal("commit unchanged after update")
	}
	if !after.Enabled || !after.Agents["ag_1"].Enabled || after.Agents["ag_1"].Config["channel"] != "#general" {
		t.Fatalf("configuration lost: %+v", after)
	}
	body, err := os.ReadFile(filepath.Join(m.Dir("demo"), "skills", "demo", "SKILL.md"))
	if err != nil || !strings.Contains(string(body), "v2") {
		t.Fatalf("checkout not swapped: %q / %v", body, err)
	}
	// The swap must not leave a backup directory behind.
	if _, err := os.Stat(m.Dir("demo") + ".old"); !os.IsNotExist(err) {
		t.Fatalf("backup directory survived: %v", err)
	}
}

func TestUpdateRequiresConsentForNewScopes(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{
		ManifestFilename: strings.Replace(skillsOnlyManifest, `"scopes": ["chat:send"]`, `"scopes": ["chat:send","agents:write"]`, 1),
	})
	commitAll(t, repo, "widen scopes")

	_, err := m.Update(context.Background(), "demo", UpdateRequest{})
	var mismatch *ScopeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want ScopeMismatchError", err)
	}
	// The old checkout must survive a refused update.
	row, _ := m.Get("demo")
	if len(row.GrantedScopes) != 1 {
		t.Fatalf("granted scopes mutated: %v", row.GrantedScopes)
	}

	after, err := m.Update(context.Background(), "demo", UpdateRequest{AckScopes: []string{"chat:send", "agents:write"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GrantedScopes) != 2 {
		t.Fatalf("granted scopes = %v", after.GrantedScopes)
	}
}

func TestUpdateRejectsIdChange(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{
		ManifestFilename: strings.Replace(skillsOnlyManifest, `"id": "demo"`, `"id": "evil"`, 1),
	})
	commitAll(t, repo, "rename")
	if _, err := m.Update(context.Background(), "demo", UpdateRequest{}); err == nil ||
		!strings.Contains(err.Error(), "declares id") {
		t.Fatalf("err = %v, want id-change rejection", err)
	}
}

func TestRemoveDeletesCheckout(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}}); err != nil {
		t.Fatal(err)
	}
	dir := m.Dir("demo")
	if err := m.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout survived removal: %v", err)
	}
	if _, err := m.Get("demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove = %v", err)
	}
	if err := m.Remove("demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Remove = %v", err)
	}
}

// --- bindings and lookups ---

func TestSkillsForAgentRespectsBothGates(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := m.SkillsForAgent("ag_1"); len(got) != 0 {
		t.Fatalf("unbound agent got skills: %+v", got)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got := m.SkillsForAgent("ag_1")
	if len(got) != 1 || got[0].Name != "demo" || got[0].ExtensionID != "demo" {
		t.Fatalf("SkillsForAgent = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(got[0].Dir, "SKILL.md")); err != nil {
		t.Fatalf("skill dir wrong: %v", err)
	}
	// Disabling the package as a whole must override the binding.
	if _, err := m.SetEnabled("demo", false); err != nil {
		t.Fatal(err)
	}
	if got := m.SkillsForAgent("ag_1"); len(got) != 0 {
		t.Fatalf("disabled package still contributes: %+v", got)
	}
}

func TestForgetAgentDropsBindings(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.ForgetAgent("ag_1"); err != nil {
		t.Fatal(err)
	}
	row, _ := m.Get("demo")
	if _, ok := row.Agents["ag_1"]; ok {
		t.Fatalf("binding survived: %+v", row.Agents)
	}
	// Forgetting an unknown agent is a no-op, not an error.
	if err := m.ForgetAgent("ag_missing"); err != nil {
		t.Fatal(err)
	}
}

func TestSetConfigRequiresGlobalSettings(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetConfig("demo", map[string]any{"a": 1}); err == nil {
		t.Fatal("SetConfig accepted on a package with no global settings")
	}
}

func TestSettingsSchemaRoundTrip(t *testing.T) {
	manifest := `{"id":"demo","name":"Demo","version":"1.0.0",
	  "contributes":{"settings":{"scope":"global","schema":"settings.schema.json"}}}`
	schema := `{"type":"object","properties":{"channel":{"type":"string"}}}`
	repo := newRepo(t, map[string]string{ManifestFilename: manifest, "settings.schema.json": schema})
	m := newManager(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo}); err != nil {
		t.Fatal(err)
	}
	data, err := m.SettingsSchema("demo")
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if probe["type"] != "object" {
		t.Fatalf("schema = %s", data)
	}
	if _, err := m.SetConfig("demo", map[string]any{"channel": "#ops"}); err != nil {
		t.Fatal(err)
	}
	row, _ := m.Get("demo")
	if row.Config["channel"] != "#ops" {
		t.Fatalf("config = %+v", row.Config)
	}
}

func TestReturnedRowsAreCopies(t *testing.T) {
	m := newManager(t)
	repo := skillsOnlyRepo(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo, AckScopes: []string{"chat:send"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetAgentBinding("demo", "ag_1", AgentBinding{Enabled: true, Config: map[string]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	row, _ := m.Get("demo")
	row.Agents["ag_1"].Config["k"] = "tampered"
	row.GrantedScopes[0] = "tampered"
	fresh, _ := m.Get("demo")
	if fresh.Agents["ag_1"].Config["k"] != "v" || fresh.GrantedScopes[0] != "chat:send" {
		t.Fatalf("registry mutated through a returned row: %+v", fresh)
	}
}

func TestNewManagerSweepsStagingDebris(t *testing.T) {
	root := t.TempDir()
	debris := filepath.Join(root, tmpDirName, "stage-leftover")
	if err := os.MkdirAll(debris, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root, "v0.127.0", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(debris); !os.IsNotExist(err) {
		t.Fatalf("staging debris survived: %v", err)
	}
}

func TestLoadStateRejectsNewerRegistryVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stateFilename), []byte(`{"version":99,"extensions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root, "v0.127.0", nil); err == nil ||
		!strings.Contains(err.Error(), "newer than this kojo supports") {
		t.Fatalf("err = %v, want version guard", err)
	}
}
