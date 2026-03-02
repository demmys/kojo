package session

import (
	"os"
	"path/filepath"
	"runtime"
)

// configDirPath returns the platform-appropriate configuration directory for kojo.
//   - Windows: %APPDATA%\kojo
//   - Others:  ~/.config/kojo
func configDirPath() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "kojo")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fallback to temp directory to avoid relative paths
		return filepath.Join(os.TempDir(), "kojo", "config")
	}
	return filepath.Join(home, ".config", "kojo")
}
