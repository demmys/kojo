//go:build !windows

package fsyncdir

import "os"

// DirStrict fsyncs the directory at path on POSIX and propagates every Sync
// error, including the "unsupported" errors that Dir deliberately swallows.
// internal/migrate uses this: its atomicWrite treats a directory-fsync
// failure as a hard error so migration crash recovery has well-defined
// semantics. Unlike Dir, no error is filtered.
func DirStrict(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
