//go:build windows

package configdir

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}
