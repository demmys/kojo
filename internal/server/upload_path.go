package server

import (
	"os"
	"path/filepath"
	"strings"
)

// upload path validation error kinds. These are constants (not formatted
// errors) so they can be safely logged without leaking absolute paths or
// other host details.
const (
	uploadErrEmpty    = "empty"
	uploadErrInvalid  = "invalid_path"
	uploadErrNotFound = "not_found"
	uploadErrOutside  = "outside_upload_dir"
	uploadErrIsDir    = "is_directory"
	uploadErrNotFile  = "not_regular_file"
	uploadErrOpenFail = "open_failed"
	uploadErrStatFail = "stat_failed"
	uploadErrSwapped  = "swapped_during_validation"
)

// uploadPathUserMessage maps an internal error kind to a fixed,
// path-free user-facing message.
func uploadPathUserMessage(kind string) string {
	switch kind {
	case uploadErrEmpty:
		return "'file_path' must not be empty"
	case uploadErrOutside:
		return "'file_path' must be inside the kojo upload directory"
	case uploadErrIsDir:
		return "'file_path' must be a regular file, not a directory"
	case uploadErrNotFile:
		return "'file_path' must be a regular file"
	case uploadErrNotFound:
		return "'file_path' does not exist or is not accessible"
	case uploadErrSwapped:
		return "'file_path' changed during validation; refusing to upload"
	default:
		return "'file_path' is invalid"
	}
}

// openUploadPath validates that p points to a regular file inside the
// kojo upload directory and returns an *os.File already opened on the
// validated inode. Callers MUST Close the returned file.
//
// Opening up-front (rather than just resolving a path string) eliminates
// the TOCTOU window between validation and the caller's read: even if a
// symlink/rename races our check, the caller reads from the fd we
// already pinned to the verified inode.
//
// On error, returns (nil, 0, kind) where kind is one of the uploadErr*
// constants — never a formatted error containing a host path.
func openUploadPath(p string) (*os.File, int64, string) {
	if p == "" {
		return nil, 0, uploadErrEmpty
	}

	// Canonicalize the upload root once (handles /tmp → /private/tmp on macOS).
	canonicalRoot, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		canonicalRoot = uploadDir
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, 0, uploadErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, 0, uploadErrNotFound
	}

	// Must be inside the upload directory (canonical-prefix check).
	if resolved != canonicalRoot &&
		!strings.HasPrefix(resolved, canonicalRoot+string(filepath.Separator)) {
		return nil, 0, uploadErrOutside
	}

	// Open the resolved (canonical) path.
	f, err := os.Open(resolved)
	if err != nil {
		return nil, 0, uploadErrOpenFail
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, uploadErrStatFail
	}
	if info.IsDir() {
		f.Close()
		return nil, 0, uploadErrIsDir
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrNotFile
	}

	// Close the residual TOCTOU window between EvalSymlinks and Open: if
	// the path (or any of its parents) was swapped to a symlink or to a
	// different inode in that gap, reject the upload.
	//
	// Three checks together cover the realistic attack vectors:
	//  1. Re-resolve symlinks: if any component was made a symlink after
	//     the first EvalSymlinks, the canonical form changes.
	//  2. Lstat regular-file: reject if the final component is now a
	//     symlink or non-regular type.
	//  3. SameFile inode match: reject if the path now points to a
	//     different inode than the open fd.
	reResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil || reResolved != resolved {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	lstatInfo, err := os.Lstat(resolved)
	if err != nil {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !lstatInfo.Mode().IsRegular() {
		f.Close()
		return nil, 0, uploadErrSwapped
	}
	if !os.SameFile(info, lstatInfo) {
		f.Close()
		return nil, 0, uploadErrSwapped
	}

	return f, info.Size(), ""
}
