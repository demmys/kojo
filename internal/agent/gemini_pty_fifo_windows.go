//go:build windows

package agent

import (
	"errors"
	"log/slog"
	"time"
)

// gemini PTY sessions are refused on Windows before these are reached.
func makeKeyFIFO(string) error {
	return errors.New("key fifo is not supported on Windows")
}

func serveKeyFIFO(string, string, time.Duration, *slog.Logger) func() { return func() {} }
