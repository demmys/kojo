//go:build windows

package session

import (
	"os"
	"syscall"
	"time"
)

func fileCreationTime(info os.FileInfo) time.Time {
	if d, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.CreationTime.Nanoseconds())
	}
	return info.ModTime()
}
