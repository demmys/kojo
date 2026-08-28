//go:build windows

package agent

import (
	"errors"
	"os"
)

// errNonRegular is returned by openStagedAttachment when the path
// resolves to a directory, device, or other non-regular file.
var errNonRegular = errors.New("attach: not a regular file")

// errDanglingSymlink is returned when a staged symlink's target is
// missing.
var errDanglingSymlink = errors.New("attach: symlink target missing")

// openStagedAttachment on Windows: os.Open follows reparse points,
// which is what we now want (see the unix build's doc comment for
// why following is safe here), and the post-open Stat rejects
// anything that did not resolve to a regular file.
func openStagedAttachment(path string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if li, lerr := os.Lstat(path); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
				return nil, nil, errDanglingSymlink
			}
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNonRegular
	}
	return f, info, nil
}
