package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/fsyncdir"
)

// atomicStage streams src into a temp file alongside dst, verifying
// the digest against expectedSHA256 (when non-empty) before any
// rename. The returned tmpPath is the staged file; the caller MUST
// either invoke atomicCommit(tmpPath, dst) to publish it or
// atomicRollback(tmpPath) to discard it. Leaving the temp on disk
// after a failed commit is the responsibility of the caller — the
// scrubber's .blob-* sweep cleans up any leaks.
//
// Separating stage from commit closes the body-orphan race that
// Codex review flagged: blob.Store.Put can now hold the staged
// body, run the ref.Put preflight against blob_refs, and only
// rename if the row update succeeds. A refused row leaves dst
// untouched.
func atomicStage(dst string, src io.Reader, mode os.FileMode, expectedSHA256 string) (tmpPath string, size int64, hexDigest string, err error) {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("blob: mkdir parent: %w", err)
	}
	f, err := os.CreateTemp(parent, ".blob-*.tmp")
	if err != nil {
		return "", 0, "", fmt.Errorf("blob: create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	if err := f.Chmod(mode); err != nil {
		cleanup()
		return "", 0, "", fmt.Errorf("blob: chmod temp: %w", err)
	}

	h := sha256.New()
	mw := io.MultiWriter(f, h)
	n, copyErr := io.Copy(mw, src)
	if copyErr != nil {
		cleanup()
		return "", 0, "", fmt.Errorf("blob: copy: %w", copyErr)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	if expectedSHA256 != "" && expectedSHA256 != digest {
		cleanup()
		return "", 0, "", fmt.Errorf("blob: sha256 mismatch: got %s want %s: %w",
			digest, expectedSHA256, ErrExpectedSHA256Mismatch)
	}

	// fsync the data before the caller renames — without this a
	// rename-then-crash can resurrect a zero-length file when the
	// metadata commits before the data (commonly observed on ext4
	// / btrfs without data=journal).
	if err := f.Sync(); err != nil {
		cleanup()
		return "", 0, "", fmt.Errorf("blob: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", 0, "", fmt.Errorf("blob: close temp: %w", err)
	}
	return tmp, n, digest, nil
}

// atomicCommit renames a staged temp into its final location and
// fsyncs the parent directory so the rename itself is durable.
// Returns (renamed, err): renamed reports whether the inode swap
// completed (the body is at dst), regardless of any subsequent
// fsyncDir failure. The caller uses this to decide whether to
// roll back upstream state (when renamed=false, dst is untouched
// and a ref restore is sound; when renamed=true with err set, the
// new body is in place and restoring the old ref would create an
// inconsistency between disk and the row).
//
// The temp file is NOT cleaned up here on failure — the caller
// invokes atomicRollback so a transient rename error doesn't leak
// the body twice.
func atomicCommit(tmp, dst string) (renamed bool, err error) {
	if err := os.Rename(tmp, dst); err != nil {
		return false, fmt.Errorf("blob: rename: %w", err)
	}
	if err := fsyncdir.Dir(filepath.Dir(dst)); err != nil {
		// The rename SUCCEEDED; the new body is at dst. The
		// only thing missing is the directory metadata
		// commit. Surface the error but tell the caller not
		// to roll back the row — the body and row are
		// consistent, only the durability guarantee for the
		// directory entry is degraded.
		return true, fmt.Errorf("blob: fsync dir: %w", err)
	}
	return true, nil
}

// atomicRollback removes a staged temp file. Best-effort: errors
// are ignored because the caller is already on a failure path and
// the scrubber's .blob-* sweep would catch any leak anyway.
func atomicRollback(tmp string) {
	if tmp == "" {
		return
	}
	_ = os.Remove(tmp)
}
