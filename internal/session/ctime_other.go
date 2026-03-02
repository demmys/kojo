//go:build !darwin && !windows

package session

import (
	"os"
	"time"
)

func fileCreationTime(info os.FileInfo) time.Time {
	return info.ModTime()
}
