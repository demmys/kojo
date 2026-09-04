//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// errNonRegular is returned by openStagedAttachment when the path
// resolves to a FIFO, socket, device, or directory. Callers map it
// to a debug-level "skip" log rather than a hard error.
var errNonRegular = errors.New("attach: not a regular file")

// errDanglingSymlink is returned when a staged symlink's target is
// missing. Kept distinct from errNonRegular so the caller can say
// which of the two happened; the agent staged something it meant to
// attach, so a dead link deserves a louder log than a FIFO.
var errDanglingSymlink = errors.New("attach: symlink target missing")

// openStagedAttachment opens a file staged in .kojo/attach and
// returns the *os.File together with the FileInfo fstat'd from that
// same fd, so the size / regular-file gate is bound to the bytes the
// caller will actually read.
//
// A final-segment symlink IS followed: staging a 2 GiB artifact with
// `ln -s` instead of copying it is the whole point of the request,
// and it costs nothing in confinement — the agent runs as kojo's own
// uid and could always have copied the target's bytes into the
// staging dir itself. What is NOT relaxed is the staging directory's
// own containment (safeStageDirAt still refuses a symlinked
// .kojo/attach), and cleanup unlinks the link, never the target.
//
// Anything that is neither a regular file nor a symlink to one is
// still refused. O_NONBLOCK is set so a staged FIFO fails the
// regular-file gate instead of blocking the open until a writer
// appears; on a regular file the flag is a no-op.
func openStagedAttachment(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// A dangling symlink is the one failure worth naming: the
		// open reports ENOENT even though the entry exists, which
		// would otherwise be logged as a confusing "file vanished".
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
