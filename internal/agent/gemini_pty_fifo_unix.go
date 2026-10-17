//go:build !windows

package agent

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// makeKeyFIFO creates the one-shot key FIFO (0600, in the 0700 launch dir).
func makeKeyFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

// serveKeyFIFO hands key to the first reader of the FIFO at path: it polls
// a non-blocking write open (ENXIO until the wrapper opens its read end),
// writes key + "\n" (well under PIPE_BUF, so a single atomic write),
// closes and removes the FIFO. It gives up after maxWait or when the
// returned stop func is called. The key never touches the filesystem: a
// FIFO's data lives only in the kernel pipe buffer.
func serveKeyFIFO(path, key string, maxWait time.Duration, logger *slog.Logger) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }
	go func() {
		defer os.Remove(path)
		deadline := time.Now().Add(maxWait)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			fi, err := os.Lstat(path)
			if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				return // removed or replaced: nothing to serve
			}
			// O_NOFOLLOW + an fstat of the opened descriptor: the path
			// could be swapped for a link or a regular file between the
			// Lstat above and the open, and the key must only ever go
			// into the FIFO kojo created.
			f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if err == nil {
				if ofi, serr := f.Stat(); serr != nil || ofi.Mode()&os.ModeNamedPipe == 0 || !os.SameFile(fi, ofi) {
					f.Close()
					if logger != nil {
						logger.Warn("gemini PTY: key fifo replaced before handoff; not serving the key")
					}
					return
				}
				_, werr := f.WriteString(key + "\n")
				f.Close()
				if werr != nil && logger != nil {
					logger.Warn("gemini PTY: key handoff failed", "err", werr)
				}
				return
			}
			if !errors.Is(err, syscall.ENXIO) {
				if logger != nil {
					logger.Warn("gemini PTY: key fifo open failed", "err", err)
				}
				return
			}
			if time.Now().After(deadline) {
				return
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return stop
}
