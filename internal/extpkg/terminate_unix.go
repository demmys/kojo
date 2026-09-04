//go:build !windows

package extpkg

import (
	"os/exec"
	"syscall"
)

// Process-group lifecycle for supervised services.
//
// A service is frequently a wrapper — a shell script, `npx`, a venv
// launcher — that spawns the process doing the actual work. Signalling
// only the direct child leaves that grandchild running: it keeps the
// port bound, keeps writing to the pipe, and survives the restart the
// supervisor thinks it just performed. Putting the child in its own
// process group and signalling the group closes that gap.

// setProcAttr puts the service in its own process group so the whole
// tree can be signalled at once.
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminate asks a supervised service to shut down cleanly. SIGTERM
// gives it a chance to flush state and close its API connections; the
// hard kill follows automatically once cmd.WaitDelay expires.
func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative PID = the whole group. If the group is gone already,
	// fall back to the direct child so a service that never got its
	// own group still gets the signal.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

// killGroup SIGKILLs the whole group. Supervisor.runOnce calls it only
// on the path where cmd.Wait has not yet reported, which is what keeps
// the group ID this service's rather than one the kernel has recycled
// — narrowly, not absolutely: Wait can finish reaping an instant
// before its result is observed, and closing that last gap needs a
// group pidfd the standard library does not offer. Best effort in any
// case; an already-empty group returns ESRCH, the expected outcome.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
