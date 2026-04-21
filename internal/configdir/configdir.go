package configdir

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	setOnce  sync.Once
	override string
)

// Set overrides the config directory path. Must be called at most once,
// before any subsystem accesses Path(). Subsequent calls are ignored so the
// resolved directory cannot change under a running process.
func Set(path string) {
	setOnce.Do(func() {
		override = path
	})
}

// Path returns the platform-appropriate configuration directory for kojo.
//   - Override set via Set(): that path
//   - Windows:                %APPDATA%\kojo
//   - Others:                 ~/.config/kojo
func Path() string {
	if override != "" {
		return override
	}
	return defaultPath()
}

// DefaultPath returns the platform-default config directory, ignoring any
// override. Exposed so callers (e.g. --dev mode) can derive a sibling dir.
func DefaultPath() string {
	return defaultPath()
}

func defaultPath() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "kojo")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kojo")
	}
	return filepath.Join(home, ".config", "kojo")
}
