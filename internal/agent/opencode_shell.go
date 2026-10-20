package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/proctree"
)

// OpenCode runs every shell-tool command through a shell it starts
// detached (setsid: the shell leads its own process group). A command
// that backgrounds a job (`server &`) leaves that job in the group after
// the shell exits; the job is reparented to init and no longer found by
// walking OpenCode's process tree, so Stop / an aborted turn would orphan
// it. kojo therefore points OpenCode's `shell` setting at a tiny wrapper
// that appends "<pid> <unix seconds>" (its pid is the group id, exec keeps
// it) to a log and execs the real shell; the reaper kills the logged
// groups that still verify (proctree.KillGroups).
//
// The wrapper has the real shell's basename: OpenCode picks the argument
// style (zsh/bash login wrappers, posix quoting) from it.

const (
	openCodeShellLog          = "pgids"
	openCodeShellLogMaxBytes  = 1 << 20
	openCodeShellReapGrace    = 3 * time.Second
	openCodeShellDirMaxAge    = 30 * 24 * time.Hour
	openCodeShellChatDirStart = "chat-"
)

// openCodeShellRoot holds the per-session (PTY) and per-turn (chat) shell
// wrapper dirs of an OpenCode home.
func openCodeShellRoot(home string) string { return filepath.Join(openCodeKojoDir(home), "shell") }

// openCodeRealShell mirrors OpenCode's own choice (Shell.acceptable with
// no configured shell): $SHELL unless it is fish or nu (which OpenCode
// refuses for the tool) and resolves to a file, else /bin/zsh on macOS,
// bash on PATH, /bin/sh.
func openCodeRealShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		switch strings.ToLower(filepath.Base(sh)) {
		case "fish", "nu":
		default:
			if filepath.IsAbs(sh) {
				if fi, err := os.Stat(sh); err == nil && fi.Mode().IsRegular() {
					return sh
				}
			} else if p, err := exec.LookPath(sh); err == nil {
				return p
			}
		}
	}
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

// openCodeShellWrapperBody renders the wrapper. It holds no secret.
func openCodeShellWrapperBody(realShell, logPath string) string {
	date := "date"
	if fi, err := os.Stat("/bin/date"); err == nil && fi.Mode().IsRegular() {
		date = "/bin/date"
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Managed by kojo: logs this shell's process group for reaping, then runs the real shell.\n")
	b.WriteString("printf '%s %s\\n' \"$$\" \"$(" + date + " +%s 2>/dev/null)\" >> " + shSingleQuote(logPath) + " 2>/dev/null\n")
	b.WriteString("exec " + shSingleQuote(realShell) + " \"$@\"\n")
	return b.String()
}

// writeOpenCodeShellWrapper (re)creates dir (a real 0700 directory under
// the home's kojo dir) with the wrapper for the real shell and returns
// the wrapper's path. Unsupported on Windows (no process groups to log).
func writeOpenCodeShellWrapper(home, dir string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", errors.New("shell wrapper: not supported on Windows")
	}
	root := openCodeShellRoot(home)
	if err := geminiRefuseSymlinks(home, openCodeKojoDir(home), root, dir); err != nil {
		return "", err
	}
	if filepath.Dir(dir) != root {
		return "", fmt.Errorf("shell wrapper dir %s is not under %s", dir, root)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	real := openCodeRealShell()
	path := filepath.Join(dir, filepath.Base(real))
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to write the shell wrapper: %s is not a regular file", path)
	}
	body := openCodeShellWrapperBody(real, filepath.Join(dir, openCodeShellLog))
	if err := writeFileIfChanged(path, []byte(body)); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// openCodePTYShellDir is the shell wrapper dir of a PTY session.
func openCodePTYShellDir(home, sessionID string) string {
	return filepath.Join(openCodeShellRoot(home), sessionID)
}

// newOpenCodeChatShellDir returns a fresh per-turn shell wrapper dir path.
func newOpenCodeChatShellDir(home string) string {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	return filepath.Join(openCodeShellRoot(home), openCodeShellChatDirStart+hex.EncodeToString(rnd[:]))
}

// reapOpenCodeShellDir kills the process groups dir's log records that
// still verify, then removes dir. Blocks for at most
// openCodeShellReapGrace plus the process listings.
func reapOpenCodeShellDir(home, dir string, logger *slog.Logger) {
	if dir == "" || filepath.Dir(dir) != openCodeShellRoot(home) {
		return
	}
	if err := geminiRefuseSymlinks(home, openCodeKojoDir(home), openCodeShellRoot(home), dir); err != nil {
		if logger != nil {
			logger.Warn("gemini: not reaping shell groups", "dir", dir, "err", err)
		}
		return
	}
	if b := readOpenCodeShellLog(filepath.Join(dir, openCodeShellLog)); len(b) > 0 {
		proctree.KillGroups(proctree.ParseGroupLog(b), openCodeShellReapGrace)
	}
	_ = os.RemoveAll(dir)
}

// readOpenCodeShellLog reads the (regular-file) log, its last
// openCodeShellLogMaxBytes at most.
func readOpenCodeShellLog(path string) []byte {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi.Size() > openCodeShellLogMaxBytes {
		if _, err := f.Seek(fi.Size()-openCodeShellLogMaxBytes, io.SeekStart); err != nil {
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, openCodeShellLogMaxBytes))
	return b
}

// sweepOpenCodeShellDirs removes wrapper dirs untouched (dir and log
// mtime) since cutoff: chat-turn dirs a crash left behind and PTY
// sessions that were deleted without a Stop.
func sweepOpenCodeShellDirs(home string, cutoff time.Time) {
	root := openCodeShellRoot(home)
	if geminiRefuseSymlinks(home, openCodeKojoDir(home), root) != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !fi.ModTime().Before(cutoff) {
			continue
		}
		if li, err := os.Lstat(filepath.Join(dir, openCodeShellLog)); err == nil && !li.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// openCodeLaunchSessionID returns the session a PTY launch's args select
// (the last `-s <id>`), or "".
func openCodeLaunchSessionID(args []string) string {
	sid := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-s" && geminiSessionIDPattern.MatchString(args[i+1]) {
			sid = args[i+1]
		}
	}
	return sid
}

// GeminiPTYReap kills the process groups the gemini PTY session's shell
// commands left behind (background jobs no longer in the TUI's process
// tree) and removes the session's wrapper dir. The session manager calls
// it after Stop / before Restart has torn the TUI down, and when the TUI
// exited on its own.
func (m *Manager) GeminiPTYReap(workDir, toolSessionID string) {
	if runtime.GOOS == "windows" || !geminiSessionIDPattern.MatchString(toolSessionID) {
		return
	}
	t := m.geminiPTYTargetFor(workDir)
	reapOpenCodeShellDir(t.home, openCodePTYShellDir(t.home, toolSessionID), m.logger)
}
