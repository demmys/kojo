package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGeminiPTYArgs(t *testing.T) {
	cases := []struct {
		name         string
		in           []string
		wantArgs     []string
		model, effor string
	}{
		{"default model", nil, []string{"-m", "google/gemini-4-argon"}, "gemini-4-argon", ""},
		{"bare id", []string{"--model", "gemini-3.8-flash"}, []string{"--model", "google/gemini-3.8-flash"}, "gemini-3.8-flash", ""},
		{"legacy alias", []string{"-m", "flash"}, []string{"-m", "google/gemini-3.8-flash"}, "gemini-3.8-flash", ""},
		{"prefixed", []string{"-m=google/gemini-4-argon"}, []string{"-m=google/gemini-4-argon"}, "gemini-4-argon", ""},
		{"joined", []string{"--model=gemini-4-argon"}, []string{"--model=google/gemini-4-argon"}, "gemini-4-argon", ""},
		{"other provider", []string{"-m", "anthropic/claude"}, []string{"-m", "anthropic/claude"}, "", ""},
		{"effort split", []string{"--effort", "high", "--auto"}, []string{"-m", "google/gemini-4-argon", "--auto"}, "gemini-4-argon", "high"},
		{"effort joined", []string{"--effort=low"}, []string{"-m", "google/gemini-4-argon"}, "gemini-4-argon", "low"},
		{"legacy flags", []string{"--skip-trust", "--approval-mode", "yolo", "--session-id", "x", "-r", "latest"}, []string{"-m", "google/gemini-4-argon", "--auto"}, "gemini-4-argon", ""},
		{"legacy -y with --auto", []string{"-y", "--auto"}, []string{"-m", "google/gemini-4-argon", "--auto"}, "gemini-4-argon", ""},
		{"legacy approval default", []string{"--approval-mode=default"}, []string{"-m", "google/gemini-4-argon"}, "gemini-4-argon", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, model, effort := geminiPTYArgs(c.in, GeminiDefaultModel)
			if !reflect.DeepEqual(got, c.wantArgs) || model != c.model || effort != c.effor {
				t.Fatalf("geminiPTYArgs(%q) = %q %q %q, want %q %q %q", c.in, got, model, effort, c.wantArgs, c.model, c.effor)
			}
		})
	}
}

func TestGeminiFixSession(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	plantOpenCodeDB(t, home,
		[3]string{ocSID1, work, "tui"},
		[3]string{ocSID2, work, openCodeChatTitle},
	)
	pending := func(id string) bool { return id == ocSID3 }
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no selection", []string{"-m", "google/x"}, []string{"-m", "google/x"}},
		{"known session", []string{"-s", ocSID1}, []string{"-s", ocSID1}},
		{"joined session", []string{"--session=" + ocSID1, "--fork"}, []string{"-s", ocSID1, "--fork"}},
		{"unknown session dropped", []string{"--session", "ses_000000000000AAAAAAAAAAAAAA", "--fork"}, []string{}},
		{"pending import kept", []string{"-s", ocSID3}, []string{"-s", ocSID3}},
		{"legacy sandbox -s dropped", []string{"-s", "--auto"}, []string{"--auto"}},
		{"invalid id dropped", []string{"-s", "abc"}, []string{}},
		{"continue -> latest interactive", []string{"--continue"}, []string{"-s", ocSID1}},
		{"-c", []string{"-c", "--auto"}, []string{"--auto", "-s", ocSID1}},
		{"continue=false", []string{"--continue=false"}, []string{}},
		{"bad session falls back to continue", []string{"-s", "ses_000000000000AAAAAAAAAAAAAA", "-c"}, []string{"-s", ocSID1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := geminiFixSession(c.in, home, work, pending)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("geminiFixSession(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// No interactive session in another dir: continue is just dropped.
	if got := geminiFixSession([]string{"--continue"}, home, t.TempDir(), nil); len(got) != 0 {
		t.Fatalf("continue elsewhere = %q", got)
	}
}

// fakeEnvOpenCode prints its argv and environment, standing in for the
// OpenCode TUI in wrapper tests.
const fakeEnvOpenCode = "#!/bin/sh\necho \"ARGV:$*\"\necho \"CWD:$(pwd -P)\"\nenv\n"

// runWrapper runs a wrapper the way tmux would (through sh, with an
// inherited environment) while kojo serves the key on its FIFO.
func runWrapper(t *testing.T, wrapper string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{wrapper}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestGeminiPTYWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	dir := t.TempDir()
	oc := filepath.Join(dir, "opencode")
	if err := os.WriteFile(oc, []byte(fakeEnvOpenCode), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "it's home") // quoting
	wrapper := filepath.Join(dir, "oc-test.sh")
	fifo := filepath.Join(dir, "oc-test.key")
	if err := makeKeyFIFO(fifo); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(dir, "work 'dir'") // quoting
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := geminiPTYWrapper(oc, workDir, home, "/cfg/pty.json", fifo, 4096)
	const key = "AIza-secret-key-value"
	if strings.Contains(body, key) {
		t.Fatal("wrapper contains the key")
	}
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := serveKeyFIFO(fifo, key, time.Minute, silentLogger())
	defer stop()

	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + dir,
		"XDG_CONFIG_HOME=/operator/config",
		"OPENCODE_CONFIG_CONTENT={}",
		"OPENCODE_SERVER_PASSWORD=x",
		"GOOGLE_API_KEY=other",
		"GEMINI_API_KEY=other",
		"GOOGLE_GENAI_USE_VERTEXAI=1",
		"KOJO_GEMINI_API_KEY=kojo",
		"KOJO_OC_ORIG_XDG_DATA_HOME=/planted",
		openCodeStreamEnv + "=1",
		"KOJO_OWNER_TOKEN=owner-secret",
		"KOJO_AGENT_TOKEN=agent-secret",
		"BUN_OPTIONS=--x",
		"PWD=/stale/launch/dir",
	}
	out := runWrapper(t, wrapper, env, "-m", "google/gemini-4-argon", "--auto")
	realWork, _ := filepath.EvalSymlinks(workDir)
	if !strings.Contains(out, "CWD:"+realWork+"\n") {
		t.Fatalf("wrapper did not enter %s:\n%s", realWork, out)
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	if !strings.Contains(out, "ARGV:-m google/gemini-4-argon --auto\n") {
		t.Fatalf("argv not passed through:\n%s", out)
	}
	want := map[string]string{
		"GOOGLE_GENERATIVE_AI_API_KEY":           key,
		"XDG_CONFIG_HOME":                        filepath.Join(home, "config"),
		"XDG_DATA_HOME":                          filepath.Join(home, "data"),
		"OPENCODE_TEST_HOME":                     home,
		"OPENCODE_CONFIG":                        "/cfg/pty.json",
		"OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX": "4096",
		"OPENCODE_DISABLE_AUTOUPDATE":            "1",
		"KOJO_OC_ORIG_XDG_CONFIG_HOME":           "/operator/config",
		"KOJO_OC_ORIG_XDG_DATA_HOME":             openCodeUnset,
		"PWD":                                    workDir,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"OPENCODE_CONFIG_CONTENT", "OPENCODE_SERVER_PASSWORD", "GOOGLE_API_KEY", "GEMINI_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "KOJO_GEMINI_API_KEY", "BUN_OPTIONS", "_kojo_fifo", "_kojo_v", openCodeStreamEnv, "KOJO_OWNER_TOKEN", "KOJO_AGENT_TOKEN"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s survived the wrapper", k)
		}
	}
	for _, p := range []string{wrapper, fifo} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed after start", p)
		}
	}
}

func TestServeKeyFIFO_StopAndReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "k.key")
	if err := makeKeyFIFO(fifo); err != nil {
		t.Fatal(err)
	}
	stop := serveKeyFIFO(fifo, "k", time.Minute, silentLogger())
	stop()
	stop() // idempotent
	waitGone := func(p string) {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Lstat(p); os.IsNotExist(err) {
				return
			}
		}
		t.Fatalf("%s not removed", p)
	}
	waitGone(fifo)

	// A FIFO replaced by a regular file is never written to.
	if err := makeKeyFIFO(fifo); err != nil {
		t.Fatal(err)
	}
	stop = serveKeyFIFO(fifo, "secret", time.Minute, silentLogger())
	defer stop()
	_ = os.Remove(fifo)
	if err := os.WriteFile(fifo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(fifo); len(b) != 0 {
		t.Fatalf("key written into a replaced path: %q", b)
	}
}

func readPTYConfig(t *testing.T, wrapper string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	mt := regexp.MustCompile(`(?m)^OPENCODE_CONFIG='([^']+)'$`).FindStringSubmatch(string(body))
	if mt == nil {
		t.Fatalf("no OPENCODE_CONFIG in wrapper:\n%s", body)
	}
	raw, err := os.ReadFile(mt[1])
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGeminiPTYLaunch_AgentAndPlain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	m := newTestManager(t)
	const key = "AIza-pty-key"
	t.Setenv("KOJO_GEMINI_API_KEY", key)
	for _, k := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	oc := filepath.Join(bin, "opencode")
	if err := os.WriteFile(oc, []byte(fakeEnvOpenCode), 0o755); err != nil {
		t.Fatal(err)
	}

	const tuiSID = "ses_eb8ffdbd0ffe067Bl9pwxaAw03"
	origCreate := createOpenCodeTUISessionFn
	createOpenCodeTUISessionFn = func(context.Context, string, string, string, string) (string, error) { return tuiSID, nil }
	t.Cleanup(func() { createOpenCodeTUISessionFn = origCreate })

	const id = "ag_ocpty"
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Tool: ToolGeminiAPI, Model: "gemini-3.8-flash", Effort: "low", MaxOutputTokens: 4096}
	m.agents["ag_other"] = &Agent{ID: "ag_other", Tool: ToolClaude, Model: "opus", Effort: "max", MaxOutputTokens: 777}
	m.mu.Unlock()
	for _, a := range []string{id, "ag_other"} {
		if err := os.MkdirAll(filepath.Join(agentDir(a), "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Agent session: agent home, agent model/effort/maxOutputTokens.
	path, args, cleanup, err := m.GeminiPTYLaunch(oc, agentDir(id), []string{"--skip-trust", "--approval-mode", "yolo", "--session-id", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-m", "google/gemini-3.8-flash", "--auto", "-s", tuiSID}; !reflect.DeepEqual(args, want) {
		t.Fatalf("agent args = %q, want %q", args, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o700 {
		t.Fatalf("wrapper mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("launch dir mode %v", fi.Mode().Perm())
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), key) {
		t.Fatal("wrapper contains the key")
	}
	if !strings.Contains(string(body), "OPENCODE_TEST_HOME="+shSingleQuote(openCodeHome(agentDir(id)))) {
		t.Fatalf("agent wrapper does not use the agent home:\n%s", body)
	}
	cfg := readPTYConfig(t, path)
	if jsonPath(cfg, "agent", "build", "variant") != "low" || cfg["model"] != "google/gemini-3.8-flash" ||
		jsonPath(cfg, "provider", "google", "models", "gemini-3.8-flash", "limit", "output") != float64(4096) {
		t.Fatalf("agent config = %v", cfg)
	}
	if jsonPath(cfg, "permission", "*") != "ask" || jsonPath(cfg, "permission", "question") != "allow" || jsonPath(cfg, "permission", "external_directory") != "ask" {
		t.Fatalf("TUI permissions = %v", cfg["permission"])
	}
	if !reflect.DeepEqual(jsonPath(cfg, "skills", "paths"), []any{geminiSkillsDir(agentDir(id))}) {
		t.Fatalf("agent config lacks the skills dir: %v", cfg)
	}
	// Shell commands go through the per-session group-logging wrapper.
	wantShell := filepath.Join(openCodePTYShellDir(openCodeHome(agentDir(id)), tuiSID), filepath.Base(openCodeRealShell()))
	if cfg["shell"] != wantShell {
		t.Fatalf("shell = %v, want %s", cfg["shell"], wantShell)
	}
	if fi, err := os.Stat(wantShell); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("shell wrapper: %v %v", fi, err)
	}
	out := runWrapper(t, path, []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GOOGLE_API_KEY=x"}, args...)
	if !strings.Contains(out, "\nGOOGLE_GENERATIVE_AI_API_KEY="+key+"\n") || !strings.Contains(out, "\nOPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX=4096\n") || strings.Contains(out, "GOOGLE_API_KEY=") {
		t.Fatalf("launched env:\n%s", out)
	}
	cleanup()

	// Explicit --effort (web UI) wins over the agent's and is stripped.
	path, args, cleanup, err = m.GeminiPTYLaunch(oc, filepath.Join(agentDir(id), "sub"), []string{"--effort", "high", "--model", "gemini-4-argon"})
	if err != nil {
		t.Fatal(err)
	}
	if containsArg(args, "--effort") || containsArg(args, "high") {
		t.Fatalf("--effort leaked into argv: %q", args)
	}
	cfg = readPTYConfig(t, path)
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup left the wrapper")
	}
	if jsonPath(cfg, "agent", "build", "variant") != "high" || cfg["model"] != "google/gemini-4-argon" {
		t.Fatalf("explicit effort config = %v", cfg)
	}

	// Non-gemini agent dir: its own home, none of its claude settings.
	path, args, cleanup, err = m.GeminiPTYLaunch(oc, agentDir("ag_other"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg = readPTYConfig(t, path)
	cleanup()
	if want := []string{"-m", "google/" + GeminiDefaultModel, "-s", tuiSID}; !reflect.DeepEqual(args, want) {
		t.Fatalf("other-agent args = %q", args)
	}
	if jsonPath(cfg, "agent", "build", "variant") != "medium" ||
		jsonPath(cfg, "provider", "google", "models", GeminiDefaultModel, "limit", "output") != float64(65536) {
		t.Fatalf("other agent's settings leaked: %v", cfg)
	}

	// Plain session: the shared kojo home, never ~/.config/opencode.
	path, _, cleanup, err = m.GeminiPTYLaunch(oc, t.TempDir(), []string{"--continue"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	cfg = readPTYConfig(t, path)
	cleanup()
	if !strings.Contains(string(body), "OPENCODE_TEST_HOME="+shSingleQuote(geminiPlainHome())) {
		t.Fatalf("plain wrapper home:\n%s", body)
	}
	if cfg["mcp"] != nil {
		t.Fatalf("plain session got MCP servers: %v", cfg["mcp"])
	}
	if home, _ := os.UserHomeDir(); strings.HasPrefix(geminiPlainHome(), filepath.Join(home, ".config", "opencode")) || strings.HasPrefix(geminiPlainHome(), filepath.Join(home, ".local")) {
		t.Fatalf("plain home inside the operator's OpenCode dirs: %s", geminiPlainHome())
	}
	entries, _ := os.ReadDir(geminiPTYLaunchDir())
	if len(entries) != 0 {
		t.Fatalf("launch dir not empty after cleanups: %v", entries)
	}
}

func TestGeminiPTYLaunch_NoKey(t *testing.T) {
	m := newTestManager(t)
	for _, k := range []string{"KOJO_GEMINI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"} {
		t.Setenv(k, "")
	}
	if _, _, _, err := m.GeminiPTYLaunch("/bin/false", t.TempDir(), nil); err == nil {
		t.Fatal("launched without a key")
	}
}

func TestSweepGeminiPTYWrappers(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "oc-old.sh")
	oldKey := filepath.Join(dir, "oc-old.key")
	legacy := filepath.Join(dir, "gemini-old.sh")
	fresh := filepath.Join(dir, "oc-new.sh")
	other := filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, oldKey, legacy, fresh, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	for _, p := range []string{old, oldKey, legacy, other} {
		_ = os.Chtimes(p, past, past)
	}
	sweepGeminiPTYWrappers(dir, time.Now().Add(-geminiPTYWrapperMaxAge))
	for _, p := range []string{old, oldKey, legacy} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale %s kept", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
}

func TestWriteGeminiPTYConfig_ContentAddressedAndSwept(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(openCodeKojoDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(openCodeKojoDir(home), "pty-0000000000000000.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-geminiPTYConfigMaxAge - time.Hour)
	_ = os.Chtimes(stale, past, past)
	a, err := writeGeminiPTYConfig(home, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := writeGeminiPTYConfig(home, []byte(`{"a":1}`))
	c, _ := writeGeminiPTYConfig(home, []byte(`{"a":2}`))
	if a != b || a == c {
		t.Fatalf("paths %s %s %s", a, b, c)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale config kept")
	}
}

func TestGeminiPTYLaunch_PreCreatesSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	m := newTestManager(t)
	t.Setenv("KOJO_GEMINI_API_KEY", "AIza-pty-key")
	const sid = "ses_eb91c4d4affeT4dyPAqbS63Ya0"
	var gotHome, gotDir string
	fail := false
	orig := createOpenCodeTUISessionFn
	createOpenCodeTUISessionFn = func(_ context.Context, _, _, home, workDir string) (string, error) {
		gotHome, gotDir = home, workDir
		if fail {
			return "", errors.New("boom")
		}
		return sid, nil
	}
	t.Cleanup(func() { createOpenCodeTUISessionFn = orig })

	work := t.TempDir()
	_, args, cleanup, err := m.GeminiPTYLaunch("/bin/true", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if want := []string{"-m", "google/" + GeminiDefaultModel, "-s", sid}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	if gotHome != geminiPlainHome() || gotDir != work {
		t.Fatalf("created in home=%q dir=%q", gotHome, gotDir)
	}

	// Pre-create failure fails the launch (no untracked session) and
	// leaves no wrapper behind.
	fail = true
	if _, _, _, err = m.GeminiPTYLaunch("/bin/true", work, nil); err == nil {
		t.Fatal("launch succeeded without a session id")
	}
	if entries, _ := os.ReadDir(geminiPTYLaunchDir()); len(entries) != 0 {
		t.Fatalf("launch dir not empty: %v", entries)
	}
}

func TestGeminiPTYLaunch_PendingImportBlocksResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	m := newTestManager(t)
	t.Setenv("KOJO_GEMINI_API_KEY", "AIza-pty-key")
	t.Setenv("PATH", t.TempDir()) // no opencode on PATH: the import cannot run
	bin := t.TempDir()
	oc := filepath.Join(bin, "opencode")
	if err := os.WriteFile(oc, []byte(fakeEnvOpenCode), 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "ag_ocpty_pending"
	const sid = "ses_eb8ffdbd0ffe067Bl9pwxaAw03"
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Tool: ToolGeminiAPI, Model: "gemini-3.8-flash"}
	m.mu.Unlock()
	dir := agentDir(id)
	pending := filepath.Join(geminiPendingImportDir(dir), sid+".json")
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := m.GeminiPTYLaunch(oc, dir, []string{"-s", sid})
	if err == nil || !strings.Contains(err.Error(), "not imported yet") {
		t.Fatalf("launch err = %v, want a not-imported-yet error", err)
	}
}

// --fork gives the TUI a session id kojo never sees, so a Restart would
// reopen the original: the launch refuses it instead.
func TestGeminiPTYLaunch_RefusesFork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	m := newTestManager(t)
	t.Setenv("KOJO_GEMINI_API_KEY", "AIza-pty-key")
	bin := t.TempDir()
	oc := filepath.Join(bin, "opencode")
	if err := os.WriteFile(oc, []byte(fakeEnvOpenCode), 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "ag_ocpty_fork"
	const sid = "ses_eb8ffdbd0ffe067Bl9pwxaAw04"
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Tool: ToolGeminiAPI, Model: "gemini-3.8-flash"}
	m.mu.Unlock()
	// A pending device-switch export keeps the -s (the empty DB would
	// otherwise drop it, and --fork with it); the fork check comes first.
	pending := filepath.Join(geminiPendingImportDir(agentDir(id)), sid+".json")
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no opencode on PATH: the import cannot run
	for _, args := range [][]string{{"-s", sid, "--fork"}, {"--session=" + sid, "--fork"}} {
		_, _, cleanup, err := m.GeminiPTYLaunch(oc, agentDir(id), args)
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "--fork is not supported") {
			t.Fatalf("%v: launch err = %v, want a --fork refusal", args, err)
		}
	}
}
