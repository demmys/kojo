//go:build !windows

package agent

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPsHasUserCommandChild(t *testing.T) {
	ps := []byte(`    1 /sbin/launchd
24649 /Users/x/kojo-ext-discord mcp
24649 /bin/zsh -c IFS= builtin read -r -d '' __grok_user_cmd <&5; exec 5<&-
  999 /bin/zsh -c __grok_user_cmd
`)
	if !psHasUserCommandChild(ps, 24649) {
		t.Fatal("want user command child")
	}
	if psHasUserCommandChild(ps, 2464) {
		t.Fatal("pid prefix must not match")
	}
	mcpOnly := []byte("24649 /Users/x/kojo-ext-discord mcp\n  999 /bin/zsh -c __grok_user_cmd\n")
	if psHasUserCommandChild(mcpOnly, 24649) {
		t.Fatal("MCP server alone is not a command")
	}
}

func TestActivityReaderTracksReads(t *testing.T) {
	a := newActivityReader(strings.NewReader("abc"))
	a.last.Store(time.Now().Add(-time.Hour).UnixNano())
	buf := make([]byte, 8)
	if _, err := a.Read(buf); err != nil {
		t.Fatal(err)
	}
	if a.idleFor(time.Now()) > time.Minute {
		t.Fatal("read did not refresh activity")
	}
}

func TestGrokHasUserCommandLive(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	has, ok := grokHasUserCommand(cmd.Process.Pid)
	if !ok {
		t.Skip("ps unavailable")
	}
	if has {
		t.Fatal("process without children reported a command")
	}
}

func TestWatchGrokIdleTerminatesSilentProcess(t *testing.T) {
	oldAfter, oldPoll := grokIdleKillAfter, grokIdlePollInterval
	grokIdleKillAfter, grokIdlePollInterval = 100*time.Millisecond, 50*time.Millisecond
	defer func() { grokIdleKillAfter, grokIdlePollInterval = oldAfter, oldPoll }()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	done := make(chan struct{})
	defer close(done)
	fired := watchGrokIdle(cmd, newActivityReader(strings.NewReader("")), done, testLogger(), "ag_test")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not terminate the idle process")
	}
	if !fired.Load() {
		t.Fatal("fired flag not set")
	}
}

func TestWatchGrokIdleSparesProcessWithCommand(t *testing.T) {
	oldAfter, oldPoll := grokIdleKillAfter, grokIdlePollInterval
	grokIdleKillAfter, grokIdlePollInterval = 100*time.Millisecond, 50*time.Millisecond
	defer func() { grokIdleKillAfter, grokIdlePollInterval = oldAfter, oldPoll }()

	// A parent whose child shell carries the grok command marker.
	cmd := exec.Command("sh", "-c", `sh -c ': __grok_user_cmd; sleep 30' & wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	done := make(chan struct{})
	fired := watchGrokIdle(cmd, newActivityReader(strings.NewReader("")), done, testLogger(), "ag_test")
	time.Sleep(600 * time.Millisecond)
	close(done)
	if fired.Load() {
		t.Fatal("watchdog killed a process that was running a command")
	}
}
