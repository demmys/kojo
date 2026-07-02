//go:build windows

package fsyncdir

// DirStrict is a no-op on Windows, matching internal/migrate's historical
// behavior: NTFS exposes no portable directory fsync, and Windows
// guarantees that MoveFile(Ex) is durable to the parent volume's metadata
// journal once it returns. This deliberately differs from Dir (which opens
// and Syncs the handle) to preserve migrate's byte-for-byte prior
// semantics — migrate never opened the directory on Windows.
func DirStrict(string) error { return nil }
