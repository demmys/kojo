package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGeminiPTYArgs(t *testing.T) {
	cases := []struct {
		name               string
		in                 []string
		wantArgs           []string
		wantModel, wantEff string
	}{
		{
			name:      "web model+effort",
			in:        []string{"--effort", "high", "--model", "gemini-3.8-flash"},
			wantArgs:  []string{"--skip-trust", "--model", "gemini-3.8-flash"},
			wantModel: "gemini-3.8-flash", wantEff: "high",
		},
		{
			name:      "joined forms, default model untouched",
			in:        []string{"--effort=low", "-m=flash", "--skip-trust"},
			wantArgs:  []string{"-m=flash", "--skip-trust"},
			wantModel: "flash", wantEff: "low",
		},
		{
			name:      "no model pins default",
			in:        []string{"--session-id", "x"},
			wantArgs:  []string{"--skip-trust", "-m", "gemini-4-argon", "--session-id", "x"},
			wantModel: "gemini-4-argon",
		},
		{
			name:      "dangling --effort dropped without eating a flag",
			in:        []string{"--effort", "--yolo"},
			wantArgs:  []string{"--skip-trust", "-m", "gemini-4-argon", "--yolo"},
			wantModel: "gemini-4-argon",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, model, eff := geminiPTYArgs(c.in, GeminiDefaultModel)
			if !reflect.DeepEqual(got, c.wantArgs) || model != c.wantModel || eff != c.wantEff {
				t.Fatalf("geminiPTYArgs(%q) = %q, %q, %q; want %q, %q, %q", c.in, got, model, eff, c.wantArgs, c.wantModel, c.wantEff)
			}
		})
	}
}

func TestGeminiFixResume(t *testing.T) {
	withGeminiTransferEnv(t)
	home := filepath.Join(t.TempDir(), "home")
	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "0123abcd-1111-4222-8333-444455556666"

	// Nothing on disk: pinned id → --session-id, latest → dropped.
	if got := geminiFixResume([]string{"-m", "x", "--resume", sid}, home, work); !reflect.DeepEqual(got, []string{"-m", "x", "--session-id", sid}) {
		t.Fatalf("unwritten pinned id: %q", got)
	}
	if got := geminiFixResume([]string{"--resume", "latest", "-m", "x"}, home, work); !reflect.DeepEqual(got, []string{"-m", "x"}) {
		t.Fatalf("latest without sessions: %q", got)
	}
	// Joined forms get the same treatment.
	if got := geminiFixResume([]string{"--resume=" + sid, "-m", "x"}, home, work); !reflect.DeepEqual(got, []string{"--session-id=" + sid, "-m", "x"}) {
		t.Fatalf("joined unwritten pinned id: %q", got)
	}
	if got := geminiFixResume([]string{"-m", "x", "-r=latest"}, home, work); !reflect.DeepEqual(got, []string{"-m", "x"}) {
		t.Fatalf("joined latest without sessions: %q", got)
	}
	// Index / unknown values pass through.
	if got := geminiFixResume([]string{"--resume", "3"}, home, work); !reflect.DeepEqual(got, []string{"--resume", "3"}) {
		t.Fatalf("index: %q", got)
	}

	chats := filepath.Join(home, ".gemini", "tmp", geminiSlugify(filepath.Base(resolvedAbs(work))), "chats")
	if err := os.MkdirAll(chats, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chats, "session-2026-10-15T10-00-"+sid[:8]+".jsonl"), []byte(gemSessionBody(sid, "hi")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := geminiFixResume([]string{"--resume", sid}, home, work); !reflect.DeepEqual(got, []string{"--resume", sid}) {
		t.Fatalf("written pinned id: %q", got)
	}
	if got := geminiFixResume([]string{"--resume", "latest"}, home, work); !reflect.DeepEqual(got, []string{"--resume", "latest"}) {
		t.Fatalf("latest with sessions: %q", got)
	}
	if got := geminiFixResume([]string{"--resume=latest"}, home, work); !reflect.DeepEqual(got, []string{"--resume=latest"}) {
		t.Fatalf("joined latest with sessions: %q", got)
	}
}

// TestGeminiPTYWrapper runs the generated wrapper with a fake gemini and
// checks the isolation: every inherited Gemini/Google auth knob is gone,
// the kojo home and key are set, args pass through intact, the wrapper
// is deleted before gemini starts, and gemini's exit status is returned.
func TestGeminiPTYWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake gemini")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n{ env; echo ARGS; for a in \"$@\"; do echo \"[$a]\"; done; } > \""+out+"\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "it's home")
	key := `k'ey"$(x)`
	wrapper := filepath.Join(dir, "w.sh")
	if err := os.WriteFile(wrapper, []byte(geminiPTYWrapper(fake, home, key)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapper, "--skip-trust", "-m", "a b", "it's")
	cmd.Env = append(os.Environ(),
		"GEMINI_API_KEY=parent", "GEMINI_CLI_HOME=/home/x/.gemini", "GEMINI_SYSTEM_MD=/x",
		"GOOGLE_API_KEY=g", "GOOGLE_GEMINI_BASE_URL=http://evil", "GOOGLE_GENAI_USE_VERTEXAI=true", "GOOGLE_GENAI_USE_GCA=true",
		"GOOGLE_VERTEX_BASE_URL=http://evil", "GOOGLE_CLOUD_ACCESS_TOKEN=t",
		"GOOGLE_CLOUD_PROJECT=p", "GOOGLE_APPLICATION_CREDENTIALS=/c", "KOJO_GEMINI_API_KEY=k",
		"KEEP_ME=1",
	)
	b, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("wrapper: want exit 7 from gemini, got %v\n%s", err, b)
	}
	// gemini must run as a child of sh, never exec'd: the relaunching
	// gemini-cli parent ignores SIGHUP, so as session leader it would
	// survive tmux kill-session (see geminiPTYWrapper).
	for _, line := range strings.Split(geminiPTYWrapper(fake, home, key), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "exec ") {
			t.Errorf("wrapper execs gemini: %q", line)
		}
	}
	if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
		t.Fatalf("wrapper not self-deleted: %v", err)
	}
	b, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	envPart, argPart, _ := strings.Cut(string(b), "ARGS\n")
	vars := map[string]string{}
	for _, line := range strings.Split(envPart, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vars[k] = v
		}
	}
	for k := range vars {
		if (strings.HasPrefix(k, "GEMINI_") && k != "GEMINI_CLI_HOME" && k != "GEMINI_API_KEY") ||
			strings.HasPrefix(k, "GOOGLE_") || k == "KOJO_GEMINI_API_KEY" {
			t.Errorf("leaked %s", k)
		}
	}
	if vars["GEMINI_CLI_HOME"] != home || vars["GEMINI_API_KEY"] != key || vars["KEEP_ME"] != "1" {
		t.Errorf("env: home=%q key ok=%v keep=%q", vars["GEMINI_CLI_HOME"], vars["GEMINI_API_KEY"] == key, vars["KEEP_ME"])
	}
	if want := "[--skip-trust]\n[-m]\n[a b]\n[it's]\n"; argPart != want {
		t.Errorf("args = %q, want %q", argPart, want)
	}
}

func TestGeminiPTYLaunch_AgentAndPlain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper")
	}
	m := newTestManager(t)
	t.Setenv("KOJO_GEMINI_API_KEY", "test-key")
	for _, k := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(k, "")
	}

	const id = "ag_gempty"
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Tool: ToolGemini, Model: "gemini-3.8-flash", Effort: "low"}
	m.agents["ag_other"] = &Agent{ID: "ag_other", Tool: ToolClaude, Model: "opus", Effort: "max"}
	m.mu.Unlock()
	for _, a := range []string{id, "ag_other"} {
		if err := os.MkdirAll(filepath.Join(agentDir(a), "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plainDir := t.TempDir()

	readSettings := func(home string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s map[string]any
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	thinking := func(s map[string]any) string {
		mc, _ := s["modelConfigs"].(map[string]any)
		if mc == nil {
			return ""
		}
		b, _ := json.Marshal(mc)
		return string(b)
	}

	// Agent session: agent home, agent model/effort defaults.
	path, args, cleanup, err := m.GeminiPTYLaunch("/usr/bin/false", agentDir(id), []string{"--session-id", "s"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--skip-trust", "-m", "gemini-3.8-flash", "--session-id", "s"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("agent args = %q, want %q", args, want)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o700 {
		t.Fatalf("wrapper mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("launch dir mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(string(body), "GEMINI_CLI_HOME="+shSingleQuote(geminiCLIHome(agentDir(id)))) {
		t.Fatalf("agent wrapper does not point at the agent home:\n%s", body)
	}
	if th := thinking(readSettings(geminiCLIHome(agentDir(id)))); !strings.Contains(th, `"thinkingLevel":"LOW"`) || !strings.Contains(th, "gemini-3.8-flash") {
		t.Fatalf("agent thinking override = %s", th)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup left the wrapper")
	}

	// Explicit --effort (web UI) wins over the agent's and is stripped.
	_, args, cleanup, err = m.GeminiPTYLaunch("/usr/bin/false", filepath.Join(agentDir(id), "sub"), []string{"--effort", "high", "--model", "gemini-4-argon"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if containsArg(args, "--effort") || containsArg(args, "high") {
		t.Fatalf("--effort leaked into gemini argv: %q", args)
	}
	if th := thinking(readSettings(geminiCLIHome(agentDir(id)))); !strings.Contains(th, `"thinkingLevel":"HIGH"`) || !strings.Contains(th, "gemini-4-argon") {
		t.Fatalf("explicit effort override = %s", th)
	}

	// Non-gemini agent dir: its own home, but none of its claude settings.
	_, args, cleanup, err = m.GeminiPTYLaunch("/usr/bin/false", agentDir("ag_other"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if want := []string{"--skip-trust", "-m", GeminiDefaultModel}; !reflect.DeepEqual(args, want) {
		t.Fatalf("other-agent args = %q", args)
	}
	if th := thinking(readSettings(geminiCLIHome(agentDir("ag_other")))); th != "" {
		t.Fatalf("other agent's effort leaked: %s", th)
	}

	// Plain session: shared kojo home, never ~/.gemini.
	path, _, cleanup, err = m.GeminiPTYLaunch("/usr/bin/false", plainDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	cleanup()
	if !strings.Contains(string(body), "GEMINI_CLI_HOME="+shSingleQuote(geminiPlainCLIHome())) {
		t.Fatalf("plain wrapper home:\n%s", body)
	}
	if s := readSettings(geminiPlainCLIHome()); s["mcpServers"] != nil {
		t.Fatalf("plain session got MCP servers: %v", s["mcpServers"])
	}
	if home, _ := os.UserHomeDir(); strings.HasPrefix(geminiPlainCLIHome(), filepath.Join(home, ".gemini")) {
		t.Fatalf("plain home inside ~/.gemini: %s", geminiPlainCLIHome())
	}
}

func TestSweepGeminiPTYWrappers(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "gemini-old.sh")
	fresh := filepath.Join(dir, "gemini-new.sh")
	other := filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)
	_ = os.Chtimes(other, past, past)
	sweepGeminiPTYWrappers(dir, time.Now().Add(-geminiPTYWrapperMaxAge))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale wrapper kept")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
}
