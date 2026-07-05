//go:build windows

package server

import "os/exec"

// setupBuildProcessGroup is a no-op on Windows; CommandContext's
// default Kill only reaches the direct child there.
func setupBuildProcessGroup(cmd *exec.Cmd) {}
