package session

import (
	"strings"

	"github.com/google/uuid"
)

// toolGemini is the PTY tool that runs the interactive Gemini CLI.
//
// Unlike the other user tools it is never exec'd directly: the CLI would
// read the operator's ~/.gemini (OAuth login, memory, sessions) and any
// GEMINI_* / GOOGLE_* auth knobs in the login shell. A ToolLauncher
// (wired by the server from the agent package) isolates every launch —
// kojo-managed GEMINI_CLI_HOME, regenerated settings.json, API key from
// kojo's credential store — and Create/Restart refuse to start gemini
// without one.
const toolGemini = "gemini"

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
	geminiResumeFlags      = []string{"--resume", "-r"}
	geminiSessionIDFlags   = []string{"--session-id"}
	geminiSessionFileFlags = []string{"--session-file"}
)

// assignGeminiSessionID pins the session's gemini session UUID so a
// restart resumes exactly it (the PTY layer can't read the id back from
// the TUI). gemini-cli treats --resume, --session-id and --session-file as
// mutually exclusive, so an explicit user choice of any of them wins:
// --session-id X becomes the tool session id, --resume / --session-file
// leave it empty (buildRestartArgs then replays the user's own target).
func assignGeminiSessionID(args []string) (string, []string) {
	for i, a := range args {
		if v, split, ok := geminiFlagMatch(a, geminiSessionIDFlags...); ok {
			if split {
				if i+1 < len(args) {
					v = args[i+1]
				}
			}
			return strings.TrimSpace(v), args
		}
		if _, _, ok := geminiFlagMatch(a, geminiResumeFlags...); ok {
			return "", args
		}
		if _, _, ok := geminiFlagMatch(a, geminiSessionFileFlags...); ok {
			return "", args
		}
	}
	id := uuid.New().String()
	runArgs := make([]string, len(args), len(args)+2)
	copy(runArgs, args)
	return id, append(runArgs, "--session-id", id)
}

// buildGeminiRestartArgs strips every session-selection flag and appends
// one deterministic --resume: the kojo-assigned id, else the target the
// user originally resumed, else "latest". The launcher turns a --resume
// of a session the CLI never wrote (no message was sent before exit)
// back into --session-id, since --resume of an unknown id is fatal.
func buildGeminiRestartArgs(origArgs []string, toolSessionID string) []string {
	args := make([]string, 0, len(origArgs)+2)
	var preservedResume string
	for i := 0; i < len(origArgs); i++ {
		a := origArgs[i]
		if v, split, ok := geminiFlagMatch(a, geminiResumeFlags...); ok {
			if split {
				// yargs string option: the next token is the value
				// unless it is another flag.
				if i+1 < len(origArgs) && !strings.HasPrefix(origArgs[i+1], "-") {
					v = origArgs[i+1]
					i++
				}
			}
			if v = strings.TrimSpace(v); v != "" {
				preservedResume = v
			}
			continue
		}
		if _, split, ok := geminiFlagMatch(a, append(geminiSessionIDFlags, geminiSessionFileFlags...)...); ok {
			if split && i+1 < len(origArgs) {
				i++
			}
			continue
		}
		args = append(args, a)
	}
	switch {
	case toolSessionID != "":
		return append(args, "--resume", toolSessionID)
	case preservedResume != "":
		return append(args, "--resume", preservedResume)
	default:
		return append(args, "--resume", "latest")
	}
}

// geminiHasApprovalMode reports whether args already choose an approval
// mode; gemini-cli rejects --yolo together with --approval-mode.
func geminiHasApprovalMode(args []string) bool {
	for _, a := range args {
		if a == "-y" || a == "--yolo" || a == "--approval-mode" || strings.HasPrefix(a, "--approval-mode=") || strings.HasPrefix(a, "--yolo=") {
			return true
		}
	}
	return false
}
