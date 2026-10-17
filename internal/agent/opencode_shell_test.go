//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenCodeRealShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	if got := openCodeRealShell(); got != "/bin/sh" {
		t.Fatalf("SHELL=/bin/sh -> %s", got)
	}
	for _, sh := range []string{"/usr/local/bin/fish", "/opt/nu", "/nonexistent/zsh", ""} {
		t.Setenv("SHELL", sh)
		got := openCodeRealShell()
		if got == sh || got == "" {
			t.Fatalf("SHELL=%q -> %q", sh, got)
		}
	}
}

func TestWriteOpenCodeShellWrapper(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("SHELL", "/bin/sh")
	dir := openCodePTYShellDir(home, "ses_eb8ffdbd0ffe067Bl9pwxaAw03")
	path, err := writeOpenCodeShellWrapper(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "sh") {
		t.Fatalf("path %s", path)
	}
	for _, p := range []string{dir, path} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", p, fi.Mode().Perm())
		}
	}
	// It runs the real shell with OpenCode's args and logs its group.
	out, err := exec.Command(path, "-c", "echo hi $0").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "hi /bin/sh" {
		t.Fatalf("wrapper run: %q %v", out, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, openCodeShellLog))
	if f := strings.Fields(string(b)); len(f) != 2 {
		t.Fatalf("log %q", b)
	}

	// Dirs outside the shell root and linked dirs are refused.
	if _, err := writeOpenCodeShellWrapper(home, filepath.Join(home, "elsewhere")); err == nil {
		t.Fatal("dir outside the shell root accepted")
	}
	target := t.TempDir()
	link := filepath.Join(openCodeShellRoot(home), "chat-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := writeOpenCodeShellWrapper(home, link); err == nil {
		t.Fatal("linked dir accepted")
	}
	reapOpenCodeShellDir(home, link, nil)
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("reap removed a linked dir")
	}
}

// An OpenCode shell-tool command run through the wrapper (detached, as
// OpenCode starts it) that leaves a background job: the reap kills the
// job and removes the dir.
func TestReapOpenCodeShellDir_KillsBackgroundJob(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("SHELL", "/bin/sh")
	dir := newOpenCodeChatShellDir(home)
	path, err := writeOpenCodeShellWrapper(home, dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	job, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if job <= 1 {
		t.Fatalf("job %q", out)
	}
	t.Cleanup(func() { _ = syscall.Kill(job, syscall.SIGKILL) })
	if syscall.Kill(job, 0) != nil {
		t.Fatal("job not running")
	}
	reapOpenCodeShellDir(home, dir, nil)
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(job, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("job %d survived the reap", job)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("reap left the dir")
	}
}

func TestSweepOpenCodeShellDirs(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("SHELL", "/bin/sh")
	old := newOpenCodeChatShellDir(home)
	fresh := newOpenCodeChatShellDir(home)
	busy := openCodePTYShellDir(home, "ses_eb8ffdbd0ffe067Bl9pwxaAw03")
	for _, d := range []string{old, fresh, busy} {
		if _, err := writeOpenCodeShellWrapper(home, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(busy, openCodeShellLog), []byte("1 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * openCodeShellDirMaxAge)
	_ = os.Chtimes(old, past, past)
	_ = os.Chtimes(busy, past, past) // log is recent
	sweepOpenCodeShellDirs(home, time.Now().Add(-openCodeShellDirMaxAge))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale dir kept")
	}
	for _, d := range []string{fresh, busy} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s swept", d)
		}
	}
}

func TestOpenCodeLaunchSessionID(t *testing.T) {
	const sid = "ses_eb8ffdbd0ffe067Bl9pwxaAw03"
	if got := openCodeLaunchSessionID([]string{"-m", "x", "-s", "../../etc", "-s", sid}); got != sid {
		t.Fatalf("got %q", got)
	}
	if got := openCodeLaunchSessionID([]string{"-s", "../x"}); got != "" {
		t.Fatalf("bad id accepted: %q", got)
	}
}
