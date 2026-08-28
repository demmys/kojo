package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Extension-contributed skills.
//
// An installed extension package can ship skill directories. Making
// them visible to a backend means materialising them inside the
// agent's own project config — `<agentDir>/.claude/skills/<name>` for
// claude/grok, `<agentDir>/.codex/skills/<name>` for codex — which is
// the same placement kojo already uses for its built-in
// kojo-switch-device skill.
//
// The directories are COPIED rather than symlinked. A symlink would
// track package updates for free, but it also makes ownership
// undecidable once the package is gone (the target no longer exists to
// compare against), and Windows needs a privilege kojo does not
// require anywhere else. Copies are re-synced on every prepareChat, so
// an updated package lands on the agent's next turn either way.
//
// Every copied directory gets an extensionSkillMarker file naming the
// package that owns it. That marker is the ONLY thing that authorises
// removal: a hand-written skill sitting next to it is never touched,
// no matter what it is called.

const extensionSkillMarker = ".kojo-extension"

// extensionSkillMaxFile caps a single copied file. Skills are prose and
// small scripts; anything larger is a packaging mistake, and refusing
// it keeps a malformed package from filling the agent's disk.
const extensionSkillMaxFile = 4 << 20

// ExtensionSkill is one skill directory an extension contributes.
// Mirrors extpkg.SkillContribution without importing it — the backend
// layer stays independent of the package registry, exactly as it does
// for ExternalMCPServer.
type ExtensionSkill struct {
	ExtensionID string
	Name        string
	Dir         string
}

// extensionSkillLookup, when set, returns the skills contributed to an
// agent by enabled packages bound to it.
var extensionSkillLookup func(agentID string) []ExtensionSkill

// SetExtensionSkillLookup wires the extension skill lookup. May be nil.
func SetExtensionSkillLookup(fn func(string) []ExtensionSkill) { extensionSkillLookup = fn }

// extensionSkillRoot maps a backend tool onto the project config
// directory whose skills/ subtree it reads. Codex has its own; claude,
// custom-claude and grok all walk `.claude`. custom-bare runs no CLI
// with a skill concept, so it gets nothing.
func extensionSkillRoot(tool string) string {
	switch NormalizeToolName(tool) {
	case ToolClaude, ToolCustomClaude, ToolGrok:
		return ".claude"
	case ToolCodex, ToolCustomCodex:
		return ".codex"
	}
	return ""
}

// SyncExtensionSkillsForTool materialises the agent's contributed
// skills under the directory its backend reads, and removes the ones
// that no longer apply. Called from prepareChat alongside the
// device-switch skill sync.
//
// Failures are logged, never fatal: a broken skill copy must not stop
// the agent from taking its turn.
func SyncExtensionSkillsForTool(agentID, tool string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	root := extensionSkillRoot(tool)
	if root == "" {
		return
	}
	var want []ExtensionSkill
	if extensionSkillLookup != nil {
		want = extensionSkillLookup(agentID)
	}
	// The device-switch skill writer already serialises writes under
	// `<agentDir>/<root>/skills`; share its per-agent lock so a
	// concurrent tool switch cannot interleave with this sweep.
	unlock := lockDeviceSwitchSkill(agentID)
	defer unlock()
	syncExtensionSkills(filepath.Join(agentDir(agentID), root, "skills"), want, logger)
}

// syncExtensionSkills is the lock-free core, split out so tests can
// drive it against a temp directory.
func syncExtensionSkills(skillsDir string, want []ExtensionSkill, logger *slog.Logger) {
	keep := make(map[string]ExtensionSkill, len(want))
	for _, sk := range want {
		name := sanitizeSkillDirName(sk.Name)
		if name == "" {
			logger.Warn("extension skill has an unusable name",
				"extension", sk.ExtensionID, "name", sk.Name)
			continue
		}
		// First writer wins on a collision. Reporting it matters more
		// than resolving it: two packages claiming one skill name is a
		// conflict the operator has to settle.
		if prev, dup := keep[name]; dup {
			logger.Warn("extension skill name collision; keeping the first",
				"name", name, "kept", prev.ExtensionID, "dropped", sk.ExtensionID)
			continue
		}
		keep[name] = sk
	}

	// Sweep stale copies before installing, so a rename inside one
	// package cannot leave both the old and the new name in place.
	entries, err := os.ReadDir(skillsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("read agent skills dir failed", "path", skillsDir, "err", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(skillsDir, e.Name())
		if !isExtensionSkillDir(dir) {
			continue
		}
		if _, ok := keep[e.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			logger.Warn("remove stale extension skill failed", "path", dir, "err", err)
		}
	}

	if len(keep) == 0 {
		return
	}
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		logger.Warn("create agent skills dir failed", "path", skillsDir, "err", err)
		return
	}
	for name, sk := range keep {
		dst := filepath.Join(skillsDir, name)
		// Refuse to overwrite a directory kojo does not own — a
		// hand-written skill with the same name outranks a package.
		if info, err := os.Lstat(dst); err == nil {
			if !info.IsDir() || !isExtensionSkillDir(dst) {
				logger.Warn("extension skill would overwrite an existing skill; skipping",
					"extension", sk.ExtensionID, "name", name)
				continue
			}
			if err := os.RemoveAll(dst); err != nil {
				logger.Warn("replace extension skill failed", "path", dst, "err", err)
				continue
			}
		}
		if err := copySkillDir(sk.Dir, dst); err != nil {
			logger.Warn("install extension skill failed",
				"extension", sk.ExtensionID, "name", name, "err", err)
			_ = os.RemoveAll(dst)
			continue
		}
		marker := filepath.Join(dst, extensionSkillMarker)
		if err := os.WriteFile(marker, []byte(sk.ExtensionID+"\n"), 0o644); err != nil {
			// Without the marker the copy is unremovable by a later
			// sweep, so drop it rather than leaking an orphan.
			logger.Warn("mark extension skill failed", "path", dst, "err", err)
			_ = os.RemoveAll(dst)
		}
	}
}

// isExtensionSkillDir reports whether kojo installed this directory on
// an extension's behalf.
func isExtensionSkillDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, extensionSkillMarker))
	return err == nil
}

// sanitizeSkillDirName reduces a contributed skill name to a single
// safe path segment, or "" when nothing usable is left.
func sanitizeSkillDirName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return ""
	}
	return name
}

// copySkillDir copies a skill directory tree. Symlinks are skipped
// rather than followed: a package could otherwise point one at
// /etc/passwd and have kojo place a readable copy inside the agent's
// own directory.
func copySkillDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > extensionSkillMaxFile {
			return fmt.Errorf("%s is %d bytes, over the %d-byte skill file limit",
				rel, info.Size(), extensionSkillMaxFile)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
