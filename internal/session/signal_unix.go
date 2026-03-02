//go:build !windows

package session

import (
	"os"
	"syscall"
)

// shutdownSignals are the OS signals that trigger graceful shutdown.
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// ShutdownSignals returns the OS signals for graceful shutdown.
func ShutdownSignals() []os.Signal { return shutdownSignals }

// sendTermSignal sends SIGTERM to the process.
func sendTermSignal(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
