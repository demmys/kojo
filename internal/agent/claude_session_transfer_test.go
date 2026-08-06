package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadClaudeSessionFiles_NewestFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	const agentID = "ag_claude_newest_first"
	absDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		t.Fatal(err)
	}
	dir := claudeProjectDir(absDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatal(err)
	}

	files, skipped, err := ReadClaudeSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(files) != 2 {
		t.Fatalf("files=%+v skipped=%+v", files, skipped)
	}
	if files[0].SessionID != "new" || files[1].SessionID != "old" {
		t.Fatalf("session order = %q, %q; want new, old", files[0].SessionID, files[1].SessionID)
	}
}

func TestStageClaudeSessionSnapshot_RemovesOmittedAndRollsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	const agentID = "ag_claude_snapshot"
	absDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		t.Fatal(err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(projectDir, "stale.jsonl")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit, rollback, err := StageClaudeSessionSnapshot(agentID, []ClaudeSessionFile{
		{SessionID: "new", Content: []byte("new")},
		{SessionID: "old", Content: []byte("old")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("omitted session remains staged: %v", err)
	}
	newInfo, err := os.Stat(filepath.Join(projectDir, "new.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(filepath.Join(projectDir, "old.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !newInfo.ModTime().After(oldInfo.ModTime()) {
		t.Fatalf("activity order lost: new=%v old=%v", newInfo.ModTime(), oldInfo.ModTime())
	}
	rollback()
	if got, err := os.ReadFile(stale); err != nil || string(got) != "stale" {
		t.Fatalf("rollback stale = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "new.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("new session survived rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "old.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("old session survived rollback: %v", err)
	}
	_ = commit

	commit, rollback, err = StageClaudeSessionSnapshot(agentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	_ = rollback
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("empty authoritative snapshot retained stale session: %v", err)
	}
}

// Dangling symlink at the target path (the --migrate-external-cli
// leftover after `--clean v0` removed the link target) must be
// replaced by a real directory instead of failing MkdirAll with
// EEXIST — the failure mode that 500'd agent-sync and stranded a
// device switch.
func TestMkdirAllReplacingDanglingSymlink_Dangling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project")
	if err := os.Symlink(filepath.Join(dir, "gone"), path); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllReplacingDanglingSymlink(path); err != nil {
		t.Fatalf("mkdirAllReplacingDanglingSymlink: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("want real dir, got mode %v", fi.Mode())
	}
}

// A VALID symlink to an existing dir is the migration-era
// indirection and must be preserved — files written through it land
// in the link target.
func TestMkdirAllReplacingDanglingSymlink_ValidSymlinkPreserved(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "project")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllReplacingDanglingSymlink(path); err != nil {
		t.Fatalf("mkdirAllReplacingDanglingSymlink: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("valid symlink was replaced; mode %v", fi.Mode())
	}
}

// Plain missing path and plain existing dir behave like MkdirAll.
func TestMkdirAllReplacingDanglingSymlink_PlainPaths(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "a", "b")
	if err := mkdirAllReplacingDanglingSymlink(fresh); err != nil {
		t.Fatalf("fresh path: %v", err)
	}
	if err := mkdirAllReplacingDanglingSymlink(fresh); err != nil {
		t.Fatalf("existing dir: %v", err)
	}
}
