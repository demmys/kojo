package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// Interactive (PTY) gemini sessions run the OpenCode TUI.
//
// A kojo PTY session runs the CLI inside tmux through the operator's login
// shell (`<shell> -lic '<cmd>'`) on the shared tmux server, so neither the
// process env (tmux takes it from the server, and the rc files can export
// their own OPENCODE_* / GOOGLE_* knobs afterwards) nor argv (world-readable
// in ps) can carry the isolation or the key. kojo therefore launches a
// one-shot wrapper script in place of the opencode binary. The wrapper
// (0700, in a 0700 dir, no secrets in it) deletes itself, drops every
// inherited OpenCode/Google variable, exports the same isolation env a
// chat turn gets, reads the API key from a one-shot FIFO kojo writes it
// to, and execs OpenCode. The key is never written to a file or argv: it
// goes from kojo's memory through the pipe into the wrapper's environment.
//
// Isolation matches the chat backend:
//   - A session whose workDir is (inside) an agent directory uses that
//     agent's OpenCode home (<agentDir>/.opencode-home) and, for a gemini
//     agent, its model, effort, maxOutputTokens and MCP servers.
//   - Any other session uses one kojo-managed home shared by plain
//     sessions, <configdir>/opencode-home: never ~/.config/opencode,
//     ~/.local/share/opencode or ~/.opencode.
//
// No system prompt: the PTY path hands none to any tool other than claude.

// geminiPlainHome is the OpenCode home shared by PTY sessions that do not
// belong to an agent.
func geminiPlainHome() string {
	return filepath.Join(configdir.Path(), "opencode-home")
}

// geminiPTYLaunchDir holds the one-shot wrapper scripts and key FIFOs.
func geminiPTYLaunchDir() string {
	return filepath.Join(openCodeKojoDir(geminiPlainHome()), "pty-launch")
}

// geminiLegacyPTYLaunchDir is where the pre-OpenCode (gemini-cli) launcher
// staged its wrappers; only swept, never written.
func geminiLegacyPTYLaunchDir() string {
	return filepath.Join(configdir.Path(), "gemini-cli-home", "kojo", "pty-launch")
}

// geminiPTYWrapperMaxAge bounds how long kojo waits for a wrapper to read
// its key FIFO (tmux never ran it); the next launch sweeps anything older,
// and NewManager sweeps every leftover at startup.
const geminiPTYWrapperMaxAge = 10 * time.Minute

// geminiPTYConfigMaxAge bounds how long an unused per-launch config file
// is kept. Configs are content-addressed and touched on every launch, so
// only configs no recent session used are removed.
const geminiPTYConfigMaxAge = 30 * 24 * time.Hour

// geminiPTYEnvUnset lists the variables the wrapper unsets by name; every
// variable matching geminiPTYEnvUnsetPatterns is dropped as well. Mirrors
// geminiStripEnvPrefixes.
var geminiPTYEnvUnset = []string{
	"GOOGLE_API_KEY",
	"GOOGLE_CLOUD_ACCESS_TOKEN",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_LOCATION",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"AGENT_BROWSER_SESSION",
	"AGENT_BROWSER_COOKIE_DIR",
	"KOJO_GEMINI_API_KEY",
}

var geminiPTYEnvUnsetPatterns = []string{
	"OPENCODE", "GEMINI_", "GOOGLE_GENERATIVE_AI_", "GOOGLE_GEMINI_",
	"GOOGLE_GENAI_", "GOOGLE_VERTEX_", "OTEL_EXPORTER_OTLP_", "BUN_",
	// Every KOJO_* var (like filterEnv for chat turns): the tmux server
	// may carry kojo's own env, e.g. KOJO_OWNER_TOKEN. The wrapper sets
	// its KOJO_OC_ORIG_* values only after this unset.
	"KOJO_",
}

// geminiPTYTarget is what a PTY workDir resolves to.
type geminiPTYTarget struct {
	home    string
	agentID string // "" for a plain session
	model   string // default when args carry none
	effort  string // default when args carry none
	// maxOutputTokens is the agent's setting (0 = default).
	maxOutputTokens int
}

// geminiPTYTargetFor resolves workDir to an agent's home or the shared
// plain home. Symlinks are resolved on both sides so an alias of the
// agent dir still lands in the agent's home.
func (m *Manager) geminiPTYTargetFor(workDir string) geminiPTYTarget {
	plain := geminiPTYTarget{home: geminiPlainHome(), model: GeminiDefaultModel}
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
	var maxOut int
	if ok {
		tool, model, effort, maxOut = a.Tool, a.Model, a.Effort, a.MaxOutputTokens
	}
	m.mu.Unlock()
	if !ok {
		return plain
	}
	t := geminiPTYTarget{home: openCodeHome(agentDir(id)), agentID: id, model: GeminiDefaultModel}
	if NormalizeToolName(tool) == ToolGemini {
		t.model = resolveGeminiModel(model)
		t.effort = effort
		t.maxOutputTokens = maxOut
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

// geminiPTYArgs rewrites the session args for the OpenCode TUI:
//   - --effort (sent by the web UI for every effort-capable tool) is not
//     an OpenCode flag; it is removed and returned for the config's
//     agent.build.variant (the TUI has no --variant flag);
//   - the model is always pinned with -m google/<model>; a bare model id
//     (or the legacy "flash" alias) gets the google/ prefix.
//
// model is the concrete google model id the config must describe ("" when
// the user picked another provider's model, which the google-only config
// then rejects).
func geminiPTYArgs(args []string, defaultModel string) (out []string, model, effort string) {
	out = make([]string, 0, len(args)+2)
	found := false
	pin := func(v string) string {
		found = true
		v = strings.TrimSpace(v)
		if rest, ok := strings.CutPrefix(v, "google/"); ok {
			model = resolveGeminiModel(rest)
		} else if !strings.Contains(v, "/") {
			model = resolveGeminiModel(v)
		} else {
			return v
		}
		return "google/" + model
	}
	args = geminiDropLegacyArgs(args)
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
			out = append(out, a)
			if i+1 < len(args) {
				out = append(out, pin(args[i+1]))
				i++
			}
			continue
		case strings.HasPrefix(a, "--model="):
			out = append(out, "--model="+pin(strings.TrimPrefix(a, "--model=")))
			continue
		case strings.HasPrefix(a, "-m="):
			out = append(out, "-m="+pin(strings.TrimPrefix(a, "-m=")))
			continue
		}
		out = append(out, a)
	}
	if !found {
		model = resolveGeminiModel(defaultModel)
		out = append([]string{"-m", "google/" + model}, out...)
	}
	return out, model, effort
}

// geminiLegacyValueFlags are gemini-cli flags (taking a value) that a PTY
// session created before the switch to OpenCode may still carry in its
// persisted args; OpenCode parses argv strictly and would refuse to start.
var geminiLegacyValueFlags = []string{"--approval-mode", "--resume", "-r", "--session-id", "--session-file"}

// geminiDropLegacyArgs removes gemini-cli-only flags. An approval mode of
// yolo (or -y) becomes OpenCode's --auto.
func geminiDropLegacyArgs(args []string) []string {
	out := make([]string, 0, len(args))
	auto := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-y" || a == "--skip-trust" {
			auto = auto || a == "-y"
			continue
		}
		dropped := false
		for _, f := range geminiLegacyValueFlags {
			if a == f {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					auto = auto || (f == "--approval-mode" && args[i+1] == "yolo")
					i++
				}
				dropped = true
				break
			}
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				auto = auto || (f == "--approval-mode" && v == "yolo")
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, a)
		}
	}
	if auto && !containsArg(out, "--auto") && !containsArg(out, "--yolo") && !containsArg(out, "--dangerously-skip-permissions") {
		out = append(out, "--auto")
	}
	return out
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// geminiTUISessionCreateTimeout bounds the session pre-create a PTY launch
// waits for (an `opencode import` normally takes about a second).
const geminiTUISessionCreateTimeout = 30 * time.Second

// geminiArgsHaveSession reports whether geminiFixSession's output names
// a session to resume.
func geminiArgsHaveSession(args []string) bool {
	for i, a := range args {
		if a == "-s" && i+1 < len(args) {
			return true
		}
	}
	return false
}

// geminiFixSession adapts the session selection (from a restart or the
// user's own args) to the agent's OpenCode DB:
//   - --continue/-c would resume OpenCode's most recent session, which can
//     be one of the agent's chat sessions (resumed concurrently by the
//     next chat turn); it is replaced by `-s <latest interactive session
//     for workDir>`, or dropped when there is none;
//   - `-s <id>` of a session the DB is known not to hold (none was ever
//     sent, or a reset removed it) is dropped so the TUI starts fresh;
//   - --fork is dropped together with the selection it applied to (a
//     kept one is refused by GeminiPTYLaunch).
func geminiFixSession(args []string, home, workDir string, pendingImport func(string) bool) []string {
	out := make([]string, 0, len(args))
	var session string
	sawContinue, sawFork := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--continue" || a == "-c":
			sawContinue = true
			continue
		case strings.HasPrefix(a, "--continue="):
			sawContinue = strings.TrimPrefix(a, "--continue=") != "false"
			continue
		case a == "--fork":
			sawFork = true
			continue
		case a == "--session" || a == "-s":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				session = strings.TrimSpace(args[i+1])
				i++
			}
			continue
		case strings.HasPrefix(a, "--session="):
			session = strings.TrimSpace(strings.TrimPrefix(a, "--session="))
			continue
		case strings.HasPrefix(a, "-s="):
			session = strings.TrimSpace(strings.TrimPrefix(a, "-s="))
			continue
		}
		out = append(out, a)
	}
	if session != "" {
		if !isGeminiSessionID(session) {
			session = ""
		} else if exists, known := openCodeSessionExists(home, session); known && !exists && (pendingImport == nil || !pendingImport(session)) {
			session = ""
		}
	}
	if session == "" && sawContinue {
		session = openCodeLatestInteractiveSession(home, workDir)
	}
	if session == "" {
		return out
	}
	out = append(out, "-s", session)
	if sawFork {
		out = append(out, "--fork")
	}
	return out
}

// buildGeminiPTYConfig is the OPENCODE_CONFIG of a PTY launch: the chat
// turn config minus the system prompt.
func buildGeminiPTYConfig(t geminiPTYTarget, model, effort string, mcp map[string]mcpServerEntry, skillsDir string) map[string]any {
	cfg := buildOpenCodeTurnConfig(openCodeTurnConfig{
		model:           model,
		variant:         geminiVariant(model, effort),
		maxOutputTokens: t.maxOutputTokens,
		mcpServers:      mcp,
		skillsDir:       skillsDir,
	})
	// The home's global config allows everything (a headless chat turn
	// cannot answer a prompt). A TUI can, so it asks before anything that
	// changes state; a yolo session passes --auto, which approves every
	// "ask" (and nothing the config denies).
	cfg["permission"] = openCodeInteractivePermissions()
	return cfg
}

// openCodeInteractivePermissions is the TUI's permission set (merged over
// openCodePermissions): read-only tools run, everything else asks, and
// .env reads ask the way OpenCode's own defaults do.
func openCodeInteractivePermissions() map[string]any {
	return map[string]any{
		"*": "ask",
		"read": map[string]any{
			"*":             "allow",
			"*.env":         "ask",
			"*.env.*":       "ask",
			"*.env.example": "allow",
		},
		"glob":               "allow",
		"grep":               "allow",
		"lsp":                "allow",
		"todowrite":          "allow",
		"skill":              "allow",
		"external_directory": "ask",
		"doom_loop":          "ask",
		"question":           "allow",
	}
}

// writeGeminiPTYConfig stores body content-addressed in the home's kojo
// dir (so concurrent sessions with different settings never overwrite
// each other's config and a restart reuses the same file) and returns
// its path. Configs unused for geminiPTYConfigMaxAge are swept.
func writeGeminiPTYConfig(home string, body []byte) (string, error) {
	dir := openCodeKojoDir(home)
	sum := sha256.Sum256(body)
	path := filepath.Join(dir, "pty-"+hex.EncodeToString(sum[:8])+".json")
	if err := writeFileIfChanged(path, body); err != nil {
		return "", err
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	sweepKojoFiles(dir, "pty-", ".json", now.Add(-geminiPTYConfigMaxAge))
	return path, nil
}

// sweepKojoFiles removes regular files named prefix*suffix in dir whose
// mtime is before cutoff.
func sweepKojoFiles(dir, prefix, suffix string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// GeminiPTYLaunch prepares one interactive OpenCode launch for a kojo PTY
// session. It returns the wrapper to run in place of opencodePath, its
// args, and a cleanup that removes the wrapper and its key FIFO if the
// session never starts.
func (m *Manager) GeminiPTYLaunch(opencodePath, workDir string, args []string) (string, []string, func(), error) {
	if runtime.GOOS == "windows" {
		return "", nil, nil, errors.New("gemini (OpenCode) PTY sessions are not supported on Windows")
	}
	apiKey, err := loadGeminiAPIKey(m.creds)
	apiKey = strings.TrimSpace(apiKey)
	if err != nil || apiKey == "" {
		return "", nil, nil, errors.New("gemini: no Gemini API key configured (set it in Settings > API keys)")
	}
	if strings.ContainsAny(apiKey, "\x00\n\r") {
		return "", nil, nil, errors.New("gemini API key contains control characters")
	}

	t := m.geminiPTYTargetFor(workDir)
	if err := ensureOpenCodeHome(t.home); err != nil {
		return "", nil, nil, fmt.Errorf("prepare opencode home: %w", err)
	}
	var mcp map[string]mcpServerEntry
	skills := ""
	var pending func(string) bool
	if t.agentID != "" {
		importPendingOpenCodeSessions(context.Background(), t.agentID, m.logger)
		apiBase := ""
		if m.groupdms != nil {
			apiBase = m.groupdms.APIBase()
		}
		mcp = BuildMCPServers(t.agentID, apiBase)
		skills = geminiSkillsDir(agentDir(t.agentID))
		dir := agentDir(t.agentID)
		pending = func(id string) bool { return hasPendingOpenCodeImport(dir, id) }
	}
	runArgs, model, effort := geminiPTYArgs(args, t.model)
	if effort == "" {
		effort = t.effort
	}
	if model == "" {
		model = t.model
	}
	runArgs = geminiFixSession(runArgs, t.home, workDir, pending)
	if slices.Contains(runArgs, "--fork") {
		// OpenCode gives the fork a new id that only the TUI knows: kojo
		// records the -s id, so a Restart would reopen the original
		// session and the forked conversation would be lost.
		return "", nil, nil, errors.New("gemini: --fork is not supported in kojo terminal sessions (a Restart could not find the forked session); resume with -s or start a new session")
	}
	if sid := openCodeLaunchSessionID(runArgs); sid != "" && pending != nil && pending(sid) {
		// The import above was postponed: resuming now would open the
		// stale local copy (or none), and the TUI's writes would then be
		// lost when the export is imported over it.
		return "", nil, nil, fmt.Errorf("gemini: session %s from the device switch is not imported yet; try again shortly", sid)
	}
	if !geminiArgsHaveSession(runArgs) {
		// A fresh TUI: create its session up front so the launch (and the
		// session manager, which records the -s id) knows exactly which
		// session a Restart resumes.
		ctx, cancel := context.WithTimeout(context.Background(), geminiTUISessionCreateTimeout)
		id, cerr := createOpenCodeTUISessionFn(ctx, opencodePath, t.agentID, t.home, workDir)
		cancel()
		if cerr != nil {
			// Launching anyway would leave kojo without the id, and a
			// later Restart could resume another terminal's session.
			return "", nil, nil, fmt.Errorf("gemini: create the opencode session: %w", cerr)
		}
		runArgs = append(runArgs, "-s", id)
	}
	ptyCfg := buildGeminiPTYConfig(t, model, effort, mcp, skills)
	if sid := openCodeLaunchSessionID(runArgs); sid != "" {
		// Shell commands log their process groups per session so Stop /
		// Restart can reap the background jobs they leave (GeminiPTYReap).
		sweepOpenCodeShellDirs(t.home, time.Now().Add(-openCodeShellDirMaxAge))
		sh, werr := writeOpenCodeShellWrapper(t.home, openCodePTYShellDir(t.home, sid))
		if werr != nil {
			// A planted symlink or an I/O error: without the wrapper,
			// Stop could not reap the session's background jobs.
			return "", nil, nil, fmt.Errorf("gemini: prepare shell wrapper: %w", werr)
		}
		ptyCfg["shell"] = sh
	}
	cfgBody, err := marshalOpenCodeConfig(ptyCfg)
	if err != nil {
		return "", nil, nil, fmt.Errorf("build opencode config: %w", err)
	}
	cfgPath, err := writeGeminiPTYConfig(t.home, cfgBody)
	if err != nil {
		return "", nil, nil, fmt.Errorf("write opencode config: %w", err)
	}

	dir := geminiPTYLaunchDir()
	if err := geminiRefuseSymlinks(geminiPlainHome(), openCodeKojoDir(geminiPlainHome()), dir); err != nil {
		return "", nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, nil, fmt.Errorf("create opencode launch dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	sweepGeminiPTYWrappers(dir, time.Now().Add(-geminiPTYWrapperMaxAge))

	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, nil, err
	}
	base := "oc-" + hex.EncodeToString(rnd[:])
	path := filepath.Join(dir, base+".sh")
	fifo := filepath.Join(dir, base+".key")
	if err := makeKeyFIFO(fifo); err != nil {
		return "", nil, nil, fmt.Errorf("create key fifo: %w", err)
	}
	body := geminiPTYWrapper(opencodePath, workDir, t.home, cfgPath, fifo, t.maxOutputTokens)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		_ = os.Remove(fifo)
		return "", nil, nil, fmt.Errorf("create opencode wrapper: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		_ = os.Remove(path)
		_ = os.Remove(fifo)
		return "", nil, nil, fmt.Errorf("write opencode wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(fifo)
		return "", nil, nil, fmt.Errorf("write opencode wrapper: %w", err)
	}
	stop := serveKeyFIFO(fifo, apiKey, geminiPTYWrapperMaxAge, m.logger)
	if m.logger != nil {
		m.logger.Info("gemini PTY launch prepared", "agent", t.agentID, "home", t.home, "model", model, "variant", geminiVariant(model, effort))
	}
	return path, runArgs, func() {
		stop()
		_ = os.Remove(path)
		_ = os.Remove(fifo)
	}, nil
}

// geminiPTYWrapper renders the one-shot launcher script. It contains no
// secret: the key arrives through the FIFO. It enters workDir and sets
// PWD itself: OpenCode resolves its project directory from $PWD before
// the cwd, and the login shell tmux runs the wrapper from may have
// changed either (rc files that cd, a stale inherited PWD).
func geminiPTYWrapper(opencodePath, workDir, home, cfgPath, fifo string, maxOutputTokens int) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# kojo one-shot opencode launcher; removes itself on start.\n")
	b.WriteString("rm -f -- \"$0\"\n")
	if workDir != "" {
		q := shSingleQuote(workDir)
		b.WriteString("cd -- " + q + " || { echo 'kojo: cannot enter the working directory' >&2; exit 1; }\n")
		b.WriteString("PWD=" + q + "\nexport PWD\n")
	}
	b.WriteString("_kojo_fifo=" + shSingleQuote(fifo) + "\n")
	var sedExprs []string
	for _, p := range geminiPTYEnvUnsetPatterns {
		sedExprs = append(sedExprs, "s/^\\("+p+"[A-Za-z0-9_]*\\)=.*/\\1/p")
	}
	b.WriteString("for _kojo_v in $(env | sed -n '" + strings.Join(sedExprs, "; ") + "'); do unset \"$_kojo_v\"; done\n")
	b.WriteString("unset _kojo_v " + strings.Join(geminiPTYEnvUnset, " ") + "\n")
	// The operator's own XDG values, captured before isolation replaces
	// them, for the env-restoring plugin (shell tool / PTY commands).
	for _, k := range openCodeXDGVars {
		b.WriteString("KOJO_OC_ORIG_" + k + "=\"${" + k + "-" + openCodeUnset + "}\"\n")
	}
	var names []string
	for _, k := range openCodeXDGVars {
		names = append(names, "KOJO_OC_ORIG_"+k)
	}
	for _, kv := range openCodeIsolationBaseEnv(home, maxOutputTokens) {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString(k + "=" + shSingleQuote(v) + "\n")
		names = append(names, k)
	}
	b.WriteString("OPENCODE_CONFIG=" + shSingleQuote(cfgPath) + "\n")
	names = append(names, "OPENCODE_CONFIG")
	b.WriteString("export " + strings.Join(names, " ") + "\n")
	b.WriteString("IFS= read -r GOOGLE_GENERATIVE_AI_API_KEY < \"$_kojo_fifo\" || { echo 'kojo: could not read the Gemini API key' >&2; exit 1; }\n")
	b.WriteString("rm -f -- \"$_kojo_fifo\"\n")
	b.WriteString("unset _kojo_fifo\n")
	b.WriteString("export GOOGLE_GENERATIVE_AI_API_KEY\n")
	b.WriteString("exec " + shSingleQuote(opencodePath) + " \"$@\"\n")
	return b.String()
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sweepGeminiPTYWrappers removes wrappers and key FIFOs older than cutoff
// (also the pre-OpenCode "gemini-*.sh" wrappers).
// sweepGeminiPTYLaunchDirs sweeps the current and the legacy (gemini-cli)
// launch dirs, each only when it and its parents under the config dir are
// real directories (a planted link must not point the sweep elsewhere).
func sweepGeminiPTYLaunchDirs(cutoff time.Time) {
	plain := geminiPlainHome()
	if geminiRefuseSymlinks(plain, openCodeKojoDir(plain), geminiPTYLaunchDir()) == nil {
		sweepGeminiPTYWrappers(geminiPTYLaunchDir(), cutoff)
	}
	legacy := geminiLegacyPTYLaunchDir()
	if geminiRefuseSymlinks(filepath.Dir(filepath.Dir(legacy)), filepath.Dir(legacy), legacy) == nil {
		sweepGeminiPTYWrappers(legacy, cutoff)
	}
}

func sweepGeminiPTYWrappers(dir string, cutoff time.Time) {
	sweepKojoFiles(dir, "oc-", ".sh", cutoff)
	sweepKojoFiles(dir, "oc-", ".key", cutoff)
	sweepKojoFiles(dir, "gemini-", ".sh", cutoff)
}
