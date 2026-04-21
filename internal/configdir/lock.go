package configdir

import (
	"fmt"
	"os"
	"path/filepath"
)

const lockFileName = "kojo.lock"

// Lock holds an exclusive advisory lock on a kojo config directory. The lock
// is released by the kernel when the holding process exits (including on
// crash) or when Release is called explicitly.
type Lock struct {
	f *os.File
}

// Release closes the lock file, releasing the advisory lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Acquire takes an exclusive, non-blocking advisory lock on <dir>/kojo.lock.
// Returns an error if another process currently holds the lock. The caller
// must call Release on shutdown.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("another kojo instance is using %s: %w", dir, err)
	}
	return &Lock{f: f}, nil
}
