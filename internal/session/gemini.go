package session

import (
	"regexp"
	"strings"
)

// toolGemini is the PTY tool that runs Gemini models in the OpenCode TUI
// (`opencode`) with a Gemini API key. The kojo tool id is "gemini-api"
// ("gemini" before toolGeminiAgy existed); the binary is OpenCode.
//
// Unlike the other user tools it is never exec'd directly: OpenCode would
// read the operator's ~/.config/opencode, ~/.local/share/opencode (incl.
// auth.json) and any OPENCODE_* / GOOGLE_* knobs in the login shell. A
// ToolLauncher (wired by the server from the agent package) isolates every
// launch (kojo-managed OpenCode home, regenerated config, API key from
// kojo's credential store handed over via the environment) and
// Create/Restart refuse to start gemini without one.
const toolGemini = "gemini-api"

// toolGeminiAgy is the PTY tool that runs the Antigravity CLI (`agy`, the
// successor of Google's gemini-cli) as the operator, like claude or grok:
// it uses the operator's own agy sign-in and settings.
const toolGeminiAgy = "gemini-agy"

// toolBinary maps a tool id to the executable it runs.
func toolBinary(tool string) string {
	switch tool {
	case toolGemini:
		return "opencode"
	case toolGeminiAgy:
		return "agy"
	}
	return tool
}

// ToolLaunchRequest describes one PTY launch handed to a ToolLauncher.
type ToolLaunchRequest struct {
	// ToolPath is the resolved CLI binary.
	ToolPath string
	// WorkDir is the session's working directory.
	WorkDir string
	// Args are the CLI args kojo would otherwise pass verbatim.
	Args []string
}

// ToolLaunch is a ToolLauncher's answer: the executable the tmux shell
// runs in place of the CLI binary, and its args.
type ToolLaunch struct {
	Path string
	Args []string
	// Cleanup, when non-nil, removes whatever the launcher staged for
	// this launch. Called when the session fails to start; a launch that
	// started cleans up after itself.
	Cleanup func()
}

// ToolLauncher prepares an isolated launch for a user tool.
type ToolLauncher func(ToolLaunchRequest) (ToolLaunch, error)

// SetToolLauncher installs the launcher for a user tool. A nil fn removes it.
func (m *Manager) SetToolLauncher(tool string, fn ToolLauncher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.launchers == nil {
		m.launchers = make(map[string]ToolLauncher)
	}
	if fn == nil {
		delete(m.launchers, tool)
		return
	}
	m.launchers[tool] = fn
}

func (m *Manager) toolLauncher(tool string) ToolLauncher {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launchers[tool]
}

// ToolReaper cleans up after a user tool's PTY run: it is handed the
// session's work dir and tool session id once the tool's process tree is
// down (Stop, before Restart, and when the tool exited on its own). For
// gemini it kills the process groups OpenCode's shell commands left
// behind (background jobs that are no longer in the TUI's tree).
type ToolReaper func(workDir, toolSessionID string)

// SetToolReaper installs the reaper for a user tool. A nil fn removes it.
func (m *Manager) SetToolReaper(tool string, fn ToolReaper) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reapers == nil {
		m.reapers = make(map[string]ToolReaper)
	}
	if fn == nil {
		delete(m.reapers, tool)
		return
	}
	m.reapers[tool] = fn
}

// reapTool runs the session tool's reaper, if any. It blocks until the
// reaper returns.
func (m *Manager) reapTool(s *Session) {
	s.mu.Lock()
	tool := NormalizeToolName(s.Tool)
	workDir := s.WorkDir
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()
	m.mu.Lock()
	fn := m.reapers[tool]
	m.mu.Unlock()
	if fn != nil && toolSessionID != "" {
		fn(workDir, toolSessionID)
	}
}

// toolRequiresLauncher reports whether tool must never start without a
// launcher (see toolGemini).
func toolRequiresLauncher(tool string) bool { return tool == toolGemini }

// geminiFlagMatch reports whether a is one of the flag names (split form, value
// in the next arg) or its joined "--flag=value" form.
func geminiFlagMatch(a string, names ...string) (joined string, split, ok bool) {
	for _, n := range names {
		if a == n {
			return "", true, true
		}
		if v, found := strings.CutPrefix(a, n+"="); found {
			return v, false, true
		}
	}
	return "", false, false
}

var (
	geminiSessionFlags  = []string{"--session", "-s"}
	geminiContinueFlags = []string{"--continue", "-c"}
)

// assignGeminiSessionID returns the session the user pinned with
// -s/--session (the tool session id a restart resumes), or "". An
// unpinned session gets its id from the launcher, which pre-creates the
// OpenCode session and passes `-s <id>`; Create/Restart record it with
// launchedGeminiSessionID.
func assignGeminiSessionID(args []string) (string, []string) {
	for i, a := range args {
		if v, split, ok := geminiFlagMatch(a, geminiSessionFlags...); ok {
			if split {
				v = ""
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					v = args[i+1]
				}
			}
			return strings.TrimSpace(v), args
		}
	}
	return "", args
}

// launchedGeminiSessionID returns the OpenCode session the prepared
// launch resumes (the last `-s <id>` the launcher settled on), or "".
func launchedGeminiSessionID(launchArgs []string) string {
	sid := ""
	for i := 0; i+1 < len(launchArgs); i++ {
		if launchArgs[i] == "-s" && openCodeSessionIDPattern.MatchString(launchArgs[i+1]) {
			sid = launchArgs[i+1]
		}
	}
	return sid
}

// buildGeminiRestartArgs strips every session-selection flag (and --fork,
// which would fork the resumed session again on every restart) and
// appends one deterministic selection: `-s <kojo tool session id>` when
// it is an OpenCode session id, else --continue (a session from before
// the launcher pre-created ids, or one whose pre-create failed). The
// launcher drops a `-s` whose session OpenCode never stored and resolves
// --continue to the latest interactive session for the work dir.
func buildGeminiRestartArgs(origArgs []string, toolSessionID string) []string {
	args := make([]string, 0, len(origArgs)+2)
	for i := 0; i < len(origArgs); i++ {
		a := origArgs[i]
		if _, split, ok := geminiFlagMatch(a, geminiSessionFlags...); ok {
			if split && i+1 < len(origArgs) && !strings.HasPrefix(origArgs[i+1], "-") {
				i++
			}
			continue
		}
		if _, _, ok := geminiFlagMatch(a, geminiContinueFlags...); ok {
			continue
		}
		if a == "--fork" || strings.HasPrefix(a, "--fork=") {
			continue
		}
		args = append(args, a)
	}
	if openCodeSessionIDPattern.MatchString(toolSessionID) {
		return append(args, "-s", toolSessionID)
	}
	return append(args, "--continue")
}

// openCodeSessionIDPattern matches OpenCode session ids (mirrors
// agent.geminiSessionIDPattern). A tool session id of another shape is
// the uuid a pre-OpenCode (gemini-cli) session was given; it can never
// resume, so such a session restarts with --continue.
var openCodeSessionIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// geminiHasApprovalMode reports whether args already turn on OpenCode's
// permission auto-approval (--auto, or its hidden aliases).
func geminiHasApprovalMode(args []string) bool {
	for _, a := range args {
		for _, f := range []string{"--auto", "--yolo", "--dangerously-skip-permissions"} {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// buildAgyRestartArgs restarts an gemini-agy session. kojo does not
// capture agy conversation ids from the TUI, so toolSessionID is normally
// empty; an explicit --conversation <id> the user passed is kept so the
// restart reopens that conversation rather than whichever agy touched last
// in the work dir (--continue).
func buildAgyRestartArgs(origArgs []string, toolSessionID string) []string {
	args := make([]string, 0, len(origArgs)+2)
	var pinned string
	for i := 0; i < len(origArgs); i++ {
		a := origArgs[i]
		if a == "--continue" || a == "-c" || a == "--continue=true" || a == "-c=true" {
			continue
		}
		if a == "--conversation" {
			if i+1 < len(origArgs) {
				pinned = origArgs[i+1]
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--conversation="); ok {
			pinned = v
			continue
		}
		args = append(args, a)
	}
	switch {
	case toolSessionID != "":
		return append(args, "--conversation", toolSessionID)
	case pinned != "":
		return append(args, "--conversation", pinned)
	default:
		return append(args, "--continue")
	}
}
