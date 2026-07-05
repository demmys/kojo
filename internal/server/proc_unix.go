//go:build !windows

package server

import (
	"os/exec"
	"syscall"
)

// setupBuildProcessGroup puts cmd in its own process group and makes
// context cancellation signal the whole group, so grandchildren
// (go build, npm) die with make on timeout.
func setupBuildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
