package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// Interactive (PTY) gemini sessions.
//
// A kojo PTY session runs the CLI inside tmux through the operator's login
// shell (`<shell> -lic '<cmd>'`), so neither the process env nor argv is a
// safe channel: the rc files can export their own GEMINI_* / GOOGLE_*
// knobs after kojo's env, and argv is world-readable in ps. kojo
// therefore launches a one-shot wrapper script instead of the gemini
// binary. The wrapper (0700, in a 0700 dir) deletes itself first, drops
// every inherited Gemini/Google auth variable, exports the isolated
// GEMINI_CLI_HOME and the API key from kojo's credential store, and execs
// the CLI. The key only ever lives in that file, for the few hundred
// milliseconds between tmux new-session and the shell running it.
//
// Isolation matches the chat backend:
//   - A session whose workDir is (inside) an agent directory uses that
//     agent's GEMINI_CLI_HOME (<agentDir>/.gemini-cli-home), with the
//     agent's model, effort and MCP servers as defaults.
//   - Any other session uses one kojo-managed home shared by plain
//     sessions, <configdir>/gemini-cli-home — never the operator's
//     ~/.gemini.
//
// settings.json is regenerated on every launch (auth type, MCP, thinking
// override). No GEMINI_SYSTEM_MD: the PTY path hands no system prompt to
// any tool other than claude.

// geminiPlainCLIHome is the GEMINI_CLI_HOME shared by PTY sessions that
// do not belong to an agent.
func geminiPlainCLIHome() string {
	return filepath.Join(configdir.Path(), "gemini-cli-home")
}

// geminiPTYLaunchDir holds the one-shot wrapper scripts.
func geminiPTYLaunchDir() string {
	return filepath.Join(geminiPlainCLIHome(), "kojo", "pty-launch")
}

// geminiPTYWrapperMaxAge bounds how long an unconsumed wrapper (tmux never
// ran it) may keep the key on disk; the next launch sweeps anything
// older, and NewManager sweeps every leftover at startup.
const geminiPTYWrapperMaxAge = 10 * time.Minute

// geminiPTYEnvUnset lists the variables the wrapper unsets by name; every
// GEMINI_*, GOOGLE_GEMINI_* and GOOGLE_GENAI_* variable is dropped by
// pattern as well. Mirrors geminiStripEnvPrefixes.
var geminiPTYEnvUnset = []string{
	"GOOGLE_API_KEY",
	"GOOGLE_VERTEX_BASE_URL",
	"GOOGLE_CLOUD_ACCESS_TOKEN",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"KOJO_GEMINI_API_KEY",
}

// geminiPTYTarget is what a PTY workDir resolves to.
type geminiPTYTarget struct {
	home    string
	agentID string // "" for a plain session
	model   string // default when args carry none
	effort  string // default when args carry none
}

// geminiPTYTargetFor resolves workDir to an agent's home or the shared
// plain home. Symlinks are resolved on both sides so an alias of the
// agent dir still lands in the agent's home.
func (m *Manager) geminiPTYTargetFor(workDir string) geminiPTYTarget {
	plain := geminiPTYTarget{home: geminiPlainCLIHome(), model: GeminiDefaultModel}
	real := resolvedAbs(workDir)
	root := resolvedAbs(agentsDir())
	if real == "" || root == "" {
		return plain
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return plain
	}
	id := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	m.mu.Lock()
	a, ok := m.agents[id]
	var tool, model, effort string
	if ok {
		tool, model, effort = a.Tool, a.Model, a.Effort
	}
	m.mu.Unlock()
	if !ok {
		return plain
	}
	t := geminiPTYTarget{home: geminiCLIHome(agentDir(id)), agentID: id, model: GeminiDefaultModel}
	if NormalizeToolName(tool) == ToolGemini {
		if model != "" {
			t.model = model
		}
		t.effort = effort
	}
	return t
}

func resolvedAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// geminiPTYArgs rewrites the session args for gemini: --effort (sent by
// the web UI for every effort-capable tool) is not a gemini flag, so it
// is removed and returned for the settings.json thinking override; the
// model is pinned with -m and --skip-trust added when absent.
func geminiPTYArgs(args []string, defaultModel string) (out []string, model, effort string) {
	out = make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--effort" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				effort = args[i+1]
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--effort="); ok {
			effort = v
			continue
		}
		switch {
		case a == "--model" || a == "-m":
			if i+1 < len(args) {
				model = args[i+1]
			}
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "-m="):
			model = strings.TrimPrefix(a, "-m=")
		}
		out = append(out, a)
	}
	var prefix []string
	if !containsArg(out, "--skip-trust") {
		prefix = append(prefix, "--skip-trust")
	}
	if model == "" {
		model = defaultModel
		prefix = append(prefix, "-m", model)
	}
	return append(prefix, out...), model, effort
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// geminiFixResume adapts `--resume <id>` (split or joined, from a restart
// or the user's own args) to what is on disk:
// a kojo-pinned session the CLI never wrote (no message was sent) is
// started fresh under the same id with --session-id, since --resume of
// an unknown id is fatal; `--resume latest` with no session for this
// project is dropped for the same reason.
func geminiFixResume(args []string, home, workDir string) []string {
	for i, a := range args {
		var v string
		joined := false
		switch {
		case a == "--resume" || a == "-r":
			if i+1 >= len(args) {
				return args
			}
			v = args[i+1]
		case strings.HasPrefix(a, "--resume="):
			v, joined = strings.TrimPrefix(a, "--resume="), true
		case strings.HasPrefix(a, "-r="):
			v, joined = strings.TrimPrefix(a, "-r="), true
		default:
			continue
		}
		width := 2 // flag + value tokens to drop
		if joined {
			width = 1
		}
		out := append([]string(nil), args...)
		switch {
		case isGeminiSessionID(v):
			if len(geminiSessionFilesInHome(home, v)) == 0 {
				if joined {
					out[i] = "--session-id=" + v
				} else {
					out[i] = "--session-id"
				}
			}
		case v == "latest":
			if !geminiProjectHasSessions(home, workDir) {
				out = append(out[:i], out[i+width:]...)
			}
		}
		return out
	}
	return args
}

// geminiProjectHasSessions reports whether the CLI has any session file
// for workDir's project in home.
func geminiProjectHasSessions(home, workDir string) bool {
	abs := resolvedAbs(workDir)
	if abs == "" {
		return true // can't tell; let the CLI decide
	}
	slug := geminiProjectSlug(home, abs, false)
	if slug == "" {
		return true
	}
	chats := filepath.Join(home, ".gemini", "tmp", slug, "chats")
	for _, pat := range []string{"session-*.jsonl", "session-*.json"} {
		if m, _ := filepath.Glob(filepath.Join(chats, pat)); len(m) > 0 {
			return true
		}
	}
	return false
}

// GeminiPTYLaunch prepares one interactive gemini launch for a kojo PTY
// session. It returns the wrapper to run in place of geminiPath, its
// args, and a cleanup that removes the wrapper if the session never
// starts.
func (m *Manager) GeminiPTYLaunch(geminiPath, workDir string, args []string) (string, []string, func(), error) {
	if runtime.GOOS == "windows" {
		return "", nil, nil, errors.New("gemini PTY sessions are not supported on Windows")
	}
	apiKey, err := loadGeminiAPIKey(m.creds)
	if err != nil {
		return "", nil, nil, fmt.Errorf("gemini API key: %w", err)
	}
	if strings.ContainsAny(apiKey, "\x00\n\r") {
		return "", nil, nil, errors.New("gemini API key contains control characters")
	}

	t := m.geminiPTYTargetFor(workDir)
	runArgs, model, effort := geminiPTYArgs(args, t.model)
	if effort == "" {
		effort = t.effort
	}
	var mcp map[string]mcpServerEntry
	if t.agentID != "" {
		apiBase := ""
		if m.groupdms != nil {
			apiBase = m.groupdms.APIBase()
		}
		mcp = BuildMCPServers(t.agentID, apiBase)
	}
	if err := writeGeminiSettingsFile(filepath.Join(t.home, ".gemini", "settings.json"), model, effort, mcp); err != nil {
		return "", nil, nil, fmt.Errorf("write gemini settings: %w", err)
	}
	runArgs = geminiFixResume(runArgs, t.home, workDir)

	dir := geminiPTYLaunchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, nil, fmt.Errorf("create gemini launch dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	sweepGeminiPTYWrappers(dir, time.Now().Add(-geminiPTYWrapperMaxAge))

	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, nil, err
	}
	path := filepath.Join(dir, "gemini-"+hex.EncodeToString(rnd[:])+".sh")
	body := geminiPTYWrapper(geminiPath, t.home, apiKey)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create gemini wrapper: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", nil, nil, fmt.Errorf("write gemini wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, nil, fmt.Errorf("write gemini wrapper: %w", err)
	}
	if m.logger != nil {
		m.logger.Info("gemini PTY launch prepared", "agent", t.agentID, "home", t.home, "model", model)
	}
	return path, runArgs, func() { _ = os.Remove(path) }, nil
}

// geminiPTYWrapper renders the one-shot launcher script.
func geminiPTYWrapper(geminiPath, home, apiKey string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# kojo one-shot gemini launcher; removes itself on start.\n")
	b.WriteString("rm -f -- \"$0\"\n")
	b.WriteString("for _kojo_v in $(env | sed -n 's/^\\(GEMINI_[A-Za-z0-9_]*\\)=.*/\\1/p; s/^\\(GOOGLE_GEMINI_[A-Za-z0-9_]*\\)=.*/\\1/p; s/^\\(GOOGLE_GENAI_[A-Za-z0-9_]*\\)=.*/\\1/p'); do unset \"$_kojo_v\"; done\n")
	b.WriteString("unset _kojo_v " + strings.Join(geminiPTYEnvUnset, " ") + "\n")
	b.WriteString("GEMINI_CLI_HOME=" + shSingleQuote(home) + "\n")
	b.WriteString("GEMINI_API_KEY=" + shSingleQuote(apiKey) + "\n")
	b.WriteString("export GEMINI_CLI_HOME GEMINI_API_KEY\n")
	// Deliberately NOT exec: gemini-cli relaunches itself as a child
	// node and the parent ignores SIGHUP, so if it became the session
	// leader, tmux kill-session (which on macOS HUPs only the controlling
	// process) would leave both nodes orphaned. Keeping sh as the
	// controlling process means it dies on HUP and the kernel then HUPs
	// the whole foreground process group, child node included.
	b.WriteString(shSingleQuote(geminiPath) + " \"$@\"\n")
	b.WriteString("_kojo_s=$?\n")
	b.WriteString("exit \"$_kojo_s\"\n")
	return b.String()
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sweepGeminiPTYWrappers removes wrappers older than cutoff.
func sweepGeminiPTYWrappers(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "gemini-") || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
