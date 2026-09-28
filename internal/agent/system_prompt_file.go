package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// claudeSystemPromptFlag / claudeSystemPromptFileFlag are the Claude CLI
// flags for the session system prompt. buildClaudeInvocation keeps the
// logical `--system-prompt <text>` pair in its args so fingerprintArgs stays
// a pure function of the prompt content; materializeClaudeSystemPrompt
// rewrites that pair to `--system-prompt-file <path>` right before exec.
const (
	claudeSystemPromptFlag     = "--system-prompt"
	claudeSystemPromptFileFlag = "--system-prompt-file"
)

// systemPromptDirName is the directory (under the kojo config dir) holding
// per-process system prompt files. It is private to the daemon (0700) and
// swept at startup; see sweepSystemPromptFiles.
const systemPromptDirName = "run/system-prompts"

// systemPromptDir is overridable in tests.
var systemPromptDir = func() string {
	return filepath.Join(configdir.Path(), systemPromptDirName)
}

// materializeClaudeSystemPrompt moves the system prompt out of argv.
//
// Passing the prompt (persona + MEMORY.md + user.md + guides, typically
// 15-30KB) as an argv value makes every word the agent has ever written to
// its memory part of /proc/<pid>/cmdline. `pkill -f <word>` / `pgrep -f`
// match the full command line, so an agent that kills e.g. `pkill -f
// "ci:local"` to clean up a test run also SIGTERMs its own Claude CLI (and
// every other live session of the same agent) whenever that word appears in
// its memory. It also exposes the whole prompt to anyone who can run `ps`.
//
// The returned args replace the first `--system-prompt <text>` pair with
// `--system-prompt-file <path>`, where path is a 0600 file inside a 0700
// directory. cleanup removes the file; call it only after the process has
// exited (the CLI may read the file lazily). When args carry no system
// prompt they are returned unchanged with a no-op cleanup.
func materializeClaudeSystemPrompt(args []string) ([]string, func(), error) {
	idx := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == claudeSystemPromptFlag {
			idx = i
			break
		}
	}
	if idx < 0 {
		return args, func() {}, nil
	}
	path, err := writeSystemPromptFile("claude", args[idx+1])
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, len(args))
	out = append(out, args[:idx]...)
	out = append(out, claudeSystemPromptFileFlag, path)
	out = append(out, args[idx+2:]...)
	var once sync.Once
	cleanup := func() {
		once.Do(func() { _ = os.Remove(path) })
	}
	return out, cleanup, nil
}

func writeSystemPromptFile(prefix, prompt string) (string, error) {
	dir := systemPromptDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create system prompt dir: %w", err)
	}
	// MkdirAll leaves an existing dir's mode alone; tighten it in case an
	// older/other process created it with a looser umask.
	_ = os.Chmod(dir, 0o700)
	f, err := os.CreateTemp(dir, prefix+"-*.txt") // CreateTemp uses 0600
	if err != nil {
		return "", fmt.Errorf("create system prompt file: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(prompt); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write system prompt file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close system prompt file: %w", err)
	}
	return path, nil
}

// systemPromptSweepAge is how old a leftover prompt file must be before the
// startup sweep removes it. The CLI reads the file at startup, but a CLI
// orphaned by an abruptly killed daemon may still be starting up when the
// next daemon comes up, so recent files are left alone.
const systemPromptSweepAge = time.Hour

// SweepSystemPromptFiles removes stale system prompt files left behind by a
// previous daemon that exited without running process cleanups (crash,
// SIGKILL). Call once at startup.
func SweepSystemPromptFiles() error {
	dir := systemPromptDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < systemPromptSweepAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}
