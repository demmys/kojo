//go:build windows

package session

import "os"

// shutdownSignals are the OS signals that trigger graceful shutdown.
var shutdownSignals = []os.Signal{os.Interrupt}

// ShutdownSignals returns the OS signals for graceful shutdown.
func ShutdownSignals() []os.Signal { return shutdownSignals }

// sendTermSignal terminates the process on Windows.
// Windows does not support SIGTERM; we use Kill as the primary mechanism.
func sendTermSignal(p *os.Process) error {
	return p.Kill()
}
