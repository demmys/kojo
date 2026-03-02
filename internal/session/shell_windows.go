//go:build windows

package session

import (
	"os"
	"os/exec"
)

// defaultShell returns the default shell on Windows.
// Checks ComSpec env, then looks for powershell.exe, falls back to cmd.exe.
func defaultShell() string {
	if comspec := os.Getenv("ComSpec"); comspec != "" {
		// Prefer PowerShell over cmd.exe if available
		if ps, err := exec.LookPath("powershell.exe"); err == nil {
			return ps
		}
		return comspec
	}
	if ps, err := exec.LookPath("powershell.exe"); err == nil {
		return ps
	}
	return "cmd.exe"
}
