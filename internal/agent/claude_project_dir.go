package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var claudeProjectDirRepairLocks keyedMutex

// repairDanglingClaudeProjectDir replaces only a dangling symlink at the
// Claude project directory derived from agentDir. Valid symlinks are retained
// so migration-era session indirection keeps working.
func repairDanglingClaudeProjectDir(agentDir string) (bool, error) {
	projectDir, err := claudeProjectPath(agentDir)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(projectDir); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lstat project dir: %w", err)
	}
	return ensureDirReplacingDanglingSymlink(projectDir, 0o700)
}

// ensureClaudeProjectDir makes the per-agent Claude project directory usable.
// In addition to ordinary creation it heals the v0→v1 migration failure mode
// where --clean v0 removed the target of the v1-path symlink.
func ensureClaudeProjectDir(agentDir string) error {
	projectDir, err := claudeProjectPath(agentDir)
	if err != nil {
		return err
	}
	_, err = ensureDirReplacingDanglingSymlink(projectDir, 0o700)
	return err
}

func claudeProjectPath(agentDir string) (string, error) {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return "", fmt.Errorf("resolve agent dir: %w", err)
	}
	return claudeProjectDir(absDir), nil
}

// ensureDirReplacingDanglingSymlink removes a symlink only when following it
// proves the target does not exist, then creates the requested directory.
// ENOENT is deliberately the sole repair condition: EACCES, ELOOP, and I/O
// failures may describe a valid indirection and must not cause destructive
// cleanup.
func ensureDirReplacingDanglingSymlink(path string, mode fs.FileMode) (bool, error) {
	defer claudeProjectDirRepairLocks.Lock(path)()

	// A process outside Kojo can still change this entry despite the keyed
	// mutex. Restart inspection when that happens rather than failing a chat
	// for a harmless concurrent repair.
	for attempt := 0; attempt < 8; attempt++ {
		fi, err := os.Lstat(path)
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path, mode); err == nil {
				return false, nil
			} else if _, lerr := os.Lstat(path); lerr == nil {
				continue
			} else {
				return false, err
			}
		}
		if err != nil {
			return false, fmt.Errorf("lstat project dir: %w", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			if err := os.MkdirAll(path, mode); err == nil {
				return false, nil
			} else if current, lerr := os.Lstat(path); lerr == nil && !os.SameFile(fi, current) {
				continue
			} else {
				return false, err
			}
		}
		originalTarget, err := os.Readlink(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if current, lerr := os.Lstat(path); os.IsNotExist(lerr) || (lerr == nil && (current.Mode()&os.ModeSymlink == 0 || !os.SameFile(fi, current))) {
				continue
			}
			return false, fmt.Errorf("read project dir symlink: %w", err)
		}

		if _, err := os.Stat(path); err == nil {
			if err := os.MkdirAll(path, mode); err == nil {
				return false, nil
			} else if target, rerr := os.Readlink(path); os.IsNotExist(rerr) || (rerr == nil && target != originalTarget) {
				continue
			} else {
				if current, lerr := os.Lstat(path); os.IsNotExist(lerr) || (lerr == nil && (current.Mode()&os.ModeSymlink == 0 || !os.SameFile(fi, current))) {
					continue
				}
				return false, err
			}
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat project dir symlink target: %w", err)
		}
		// Revalidate immediately before unlinking. Another process may have
		// replaced the link or recreated its target since the first Stat.
		currentInfo, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("re-lstat project dir: %w", err)
		}
		if currentInfo.Mode()&os.ModeSymlink == 0 {
			continue
		}
		currentTarget, err := os.Readlink(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if latest, lerr := os.Lstat(path); os.IsNotExist(lerr) || (lerr == nil && (latest.Mode()&os.ModeSymlink == 0 || !os.SameFile(currentInfo, latest))) {
				continue
			}
			return false, fmt.Errorf("re-read project dir symlink: %w", err)
		}
		if currentTarget != originalTarget {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			if err := os.MkdirAll(path, mode); err == nil {
				return false, nil
			} else if target, rerr := os.Readlink(path); os.IsNotExist(rerr) || (rerr == nil && target != originalTarget) {
				continue
			} else {
				if latest, lerr := os.Lstat(path); os.IsNotExist(lerr) || (lerr == nil && (latest.Mode()&os.ModeSymlink == 0 || !os.SameFile(currentInfo, latest))) {
					continue
				}
				return false, err
			}
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("re-stat project dir symlink target: %w", err)
		}

		// Move the symlink aside before replacement. Validity can change when
		// the old migration target is recreated; retaining the link object lets
		// us restore it rather than unlinking a now-valid session path.
		scratch, err := os.MkdirTemp(filepath.Dir(path), ".kojo-claude-project-repair-")
		if err != nil {
			return false, fmt.Errorf("create project dir repair scratch: %w", err)
		}
		backup := filepath.Join(scratch, "project-link")
		if err := os.Rename(path, backup); os.IsNotExist(err) {
			_ = os.Remove(scratch)
			continue
		} else if err != nil {
			_ = os.Remove(scratch)
			return false, fmt.Errorf("quarantine dangling project dir symlink: %w", err)
		}
		if _, err := os.Stat(backup); err == nil {
			if err := restoreClaudeProjectSymlink(path, backup, scratch); err != nil {
				return false, err
			}
			return false, nil
		} else if !os.IsNotExist(err) {
			_ = restoreClaudeProjectSymlink(path, backup, scratch)
			return false, fmt.Errorf("stat quarantined project dir symlink: %w", err)
		}
		if err := os.MkdirAll(path, mode); err != nil {
			if _, lerr := os.Lstat(path); os.IsNotExist(lerr) {
				_ = restoreClaudeProjectSymlink(path, backup, scratch)
			}
			return false, fmt.Errorf("create replacement project dir: %w", err)
		}
		// Target recovery after the move but before creation must also win.
		if _, err := os.Stat(backup); err == nil {
			if err := os.Remove(path); err != nil {
				return false, fmt.Errorf("old project target recovered; symlink preserved at %s; remove empty replacement: %w", backup, err)
			}
			if err := restoreClaudeProjectSymlink(path, backup, scratch); err != nil {
				return false, err
			}
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("re-stat quarantined project dir symlink: %w", err)
		}
		if err := os.Remove(backup); err != nil {
			return false, fmt.Errorf("remove quarantined dangling symlink: %w", err)
		}
		if err := os.Remove(scratch); err != nil {
			return false, fmt.Errorf("remove project dir repair scratch: %w", err)
		}
		return true, nil
	}
	return false, fmt.Errorf("project dir changed repeatedly during repair")
}

func restoreClaudeProjectSymlink(path, backup, scratch string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("project path changed during repair; original symlink preserved at %s", backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect project path before restore; original symlink preserved at %s: %w", backup, err)
	}
	if err := os.Rename(backup, path); err != nil {
		return fmt.Errorf("restore project symlink from %s: %w", backup, err)
	}
	if err := os.Remove(scratch); err != nil {
		return fmt.Errorf("remove project dir repair scratch: %w", err)
	}
	return nil
}

// mkdirAllReplacingDanglingSymlink preserves the historical 0755 mode used
// for the agent root during session transfer. Claude project directories call
// ensureDirReplacingDanglingSymlink directly with 0700.
func mkdirAllReplacingDanglingSymlink(path string) error {
	_, err := ensureDirReplacingDanglingSymlink(path, 0o755)
	return err
}
