//go:build windows

package extpkg

import "os/exec"

// setProcAttr is a no-op on Windows. Containing a process tree here
// needs a Job Object, which means pulling in x/sys/windows for a
// platform kojo does not ship supervised services on yet; the direct
// child is still killed below.
func setProcAttr(cmd *exec.Cmd) {}

// terminate stops a supervised service. Windows has no SIGTERM for a
// process kojo did not create a console group for, so the kill is
// immediate; cmd.WaitDelay still bounds the pipe drain.
func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killGroup has no Windows equivalent without a Job Object; a
// grandchild outliving its parent is a known gap on this platform.
func killGroup(cmd *exec.Cmd) {}
