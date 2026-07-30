package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveLegacyAttachSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	agentID := "ag_legacy_attach_skill"
	for _, root := range []string{".claude", ".codex"} {
		skillDir := filepath.Join(agentDir(agentID), root, "skills", attachSkillDirName)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatalf("mkdir legacy skill: %v", err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("legacy"), 0o644); err != nil {
			t.Fatalf("write legacy skill: %v", err)
		}

		sibling := filepath.Join(agentDir(agentID), root, "skills", "sibling", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}
		if err := os.WriteFile(sibling, []byte("sibling"), 0o644); err != nil {
			t.Fatalf("write sibling: %v", err)
		}
	}

	RemoveLegacyAttachSkills(agentID, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, root := range []string{".claude", ".codex"} {
		legacyDir := filepath.Join(agentDir(agentID), root, "skills", attachSkillDirName)
		if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
			t.Errorf("legacy %s skill still present: %v", root, err)
		}
		sibling := filepath.Join(agentDir(agentID), root, "skills", "sibling", "SKILL.md")
		if _, err := os.Stat(sibling); err != nil {
			t.Errorf("sibling %s skill removed: %v", root, err)
		}
	}
}
