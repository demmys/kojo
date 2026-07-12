package agent

import (
	"os"
	"path/filepath"
	"testing"
)

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
