// Package fsyncdir centralizes kojo's directory-fsync recipe: open a
// directory read-only and Sync it so a preceding rename/unlink is durable
// across a crash. It replaces four near-identical copies that previously
// lived in internal/{blob,oplog,agent,migrate}.
package fsyncdir

import "os"

// Dir fsyncs the directory at path. It opens the directory (O_RDONLY) and
// calls Sync. Filesystems / platforms that do not support fsync on a
// directory handle (Windows, some network filesystems) report an
// "unsupported" error, which is treated as a non-fatal best-effort success:
// the preceding rename is already on disk. Real errors are propagated.
//
// This matches the semantics historically used by internal/blob,
// internal/oplog and internal/agent.
func Dir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Windows / some network filesystems refuse fsync on directory
		// handles — non-fatal; the rename is still on disk.
		if isUnsupported(err) {
			return nil
		}
		return err
	}
	return nil
}

// DirLenient fsyncs the directory at path like Dir, but swallows every Sync
// error unconditionally (not just "unsupported" ones). Failure to open the
// directory is still propagated. This matches the semantics historically
// used by internal/oplog and internal/agent, where a directory fsync is
// best-effort but a missing/unreadable directory is a real error.
// Propagating real Sync errors here is a pending proposal, not yet applied.
func DirLenient(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}
