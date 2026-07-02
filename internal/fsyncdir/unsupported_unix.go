//go:build !windows

package fsyncdir

import (
	"errors"
	"syscall"
)

// isUnsupported reports whether err indicates directory fsync is not
// implemented for this filesystem / OS — used by Dir to swallow
// "directories cannot be fsync'd" on filesystems that refuse it.
func isUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
