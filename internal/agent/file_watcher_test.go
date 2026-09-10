package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestFileWatcher_FlushesHeldAgentWrite asserts a disk write to a
// held agent's MEMORY.md is reflected into the DB by the watcher
// without waiting for a prepareChat sync.
func TestFileWatcher_FlushesHeldAgentWrite(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_watch")
	mgr := &Manager{
		agents: map[string]*Agent{"ag_watch": {ID: "ag_watch"}},
		logger: quietLogger(),
	}

	fw := newFileWatcherForTest(t, mgr)
	go fw.run()
	t.Cleanup(func() { _ = fw.Close() })

	memPath := filepath.Join(agentDir("ag_watch"), "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("# watched\n"), 0o644); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}

	// Poll the DB until the watcher's debounced flush lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, gerr := st.GetAgentMemory(context.Background(), "ag_watch")
		if gerr == nil && rec.Body == "# watched\n" {
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not flush MEMORY.md to DB within deadline (last err=%v)", gerr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFileWatcher_SkipsNonHeldAgent asserts the watcher does NOT push a
// non-held agent's stale local file into the DB — the holder gate that
// prevents a device-switch rollback.
func TestFileWatcher_SkipsNonHeldAgent(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_nonheld")
	// Manager with NO local agents → holdsLocally is always false.
	mgr := &Manager{
		agents: map[string]*Agent{},
		logger: quietLogger(),
	}

	// Seed a canonical DB row, then write a DIFFERENT (stale) body to
	// disk. A correct watcher must not overwrite the DB row.
	if _, err := st.UpsertAgentMemory(context.Background(), "ag_nonheld", "db-canonical\n", "",
		store.AgentMemoryInsertOptions{AllowOverwrite: true}); err != nil {
		t.Fatalf("seed DB row: %v", err)
	}

	fw := newFileWatcherForTest(t, mgr)
	go fw.run()
	t.Cleanup(func() { _ = fw.Close() })

	memPath := filepath.Join(agentDir("ag_nonheld"), "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("stale-disk\n"), 0o644); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}

	// Give the debounce + flush ample time to (incorrectly) fire.
	time.Sleep(2 * time.Second)

	rec, err := st.GetAgentMemory(context.Background(), "ag_nonheld")
	if err != nil {
		t.Fatalf("GetAgentMemory: %v", err)
	}
	if rec.Body != "db-canonical\n" {
		t.Errorf("non-held agent flushed into DB: body=%q, want db-canonical", rec.Body)
	}
}

func newFileWatcherForTest(t *testing.T, mgr *Manager) *fileWatcher {
	t.Helper()
	fw, err := newFileWatcher(mgr)
	if err == nil {
		return fw
	}
	// Linux's inotify watch count is shared by every process for the current
	// OS user. A busy developer host can exhaust it independently of this
	// test. Skip only an Add failure caused by that shared limit; EMFILE and
	// unrelated ENOSPC failures remain hard failures so local leaks and disk
	// exhaustion are not hidden.
	if errors.Is(err, errFileWatcherAdd) && errors.Is(err, syscall.ENOSPC) {
		t.Skipf("fsnotify resources exhausted: %v", err)
	}
	t.Fatalf("newFileWatcher: %v", err)
	return nil
}

func TestFileWatcher_WatchListIsScopedAndTracksNewMemoryDirs(t *testing.T) {
	_ = memorySyncTestEnv(t, "ag_scope")
	agentRoot := agentDir("ag_scope")
	memoryRoot := filepath.Join(agentRoot, "memory")
	existingMemoryDir := filepath.Join(memoryRoot, "existing", "deep")
	repositoryDir := filepath.Join(agentRoot, "repo", "node_modules", "pkg")
	for _, dir := range []string{existingMemoryDir, repositoryDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir fixture %s: %v", dir, err)
		}
	}

	mgr := &Manager{
		agents: map[string]*Agent{"ag_scope": {ID: "ag_scope"}},
		logger: quietLogger(),
	}
	fw := newFileWatcherForTest(t, mgr)
	go fw.run()
	t.Cleanup(func() { _ = fw.Close() })

	wantInitial := []string{
		agentsDir(),
		agentRoot,
		memoryRoot,
		filepath.Join(memoryRoot, "existing"),
		existingMemoryDir,
	}
	assertWatchListEventually(t, fw, wantInitial)
	assertNotWatched(t, fw, filepath.Join(agentRoot, "repo"), repositoryDir)

	newMemoryDir := filepath.Join(memoryRoot, "new", "deep")
	if err := os.MkdirAll(newMemoryDir, 0o755); err != nil {
		t.Fatalf("mkdir new memory subtree: %v", err)
	}
	assertWatchListEventually(t, fw, append(wantInitial,
		filepath.Join(memoryRoot, "new"), newMemoryDir))

	newWorkspaceDir := filepath.Join(agentRoot, "workspace", "vendor")
	if err := os.MkdirAll(newWorkspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir new workspace subtree: %v", err)
	}
	// Give the event loop time to process the agent-root CREATE event, then
	// verify it did not expand the watch set into the unrelated subtree.
	time.Sleep(200 * time.Millisecond)
	assertNotWatched(t, fw, filepath.Join(agentRoot, "workspace"), newWorkspaceDir)
}

func assertWatchListEventually(t *testing.T, fw *fileWatcher, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := make(map[string]bool)
		for _, path := range fw.w.WatchList() {
			got[filepath.Clean(path)] = true
		}
		missing := ""
		for _, path := range want {
			if !got[filepath.Clean(path)] {
				missing = path
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch list never included %q; got=%v", missing, fw.w.WatchList())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertNotWatched(t *testing.T, fw *fileWatcher, paths ...string) {
	t.Helper()
	got := make(map[string]bool)
	for _, path := range fw.w.WatchList() {
		got[filepath.Clean(path)] = true
	}
	for _, path := range paths {
		if got[filepath.Clean(path)] {
			t.Fatalf("unrelated directory is watched: %q; watch list=%v", path, fw.w.WatchList())
		}
	}
}

func TestFileWatcher_ShouldWatchOnlyCanonicalMirrorDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agents")
	fw := &fileWatcher{root: root}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "agents root", path: root, want: true},
		{name: "agent root", path: filepath.Join(root, "ag_one"), want: true},
		{name: "memory root", path: filepath.Join(root, "ag_one", "memory"), want: true},
		{name: "nested memory", path: filepath.Join(root, "ag_one", "memory", "topic"), want: true},
		{name: "repository", path: filepath.Join(root, "ag_one", "kojo-fork"), want: false},
		{name: "node modules", path: filepath.Join(root, "ag_one", "kojo-fork", "node_modules"), want: false},
		{name: "cli state", path: filepath.Join(root, "ag_one", ".codex"), want: false},
		{name: "outside", path: filepath.Join(filepath.Dir(root), "elsewhere"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fw.shouldWatchDir(tt.path); got != tt.want {
				t.Fatalf("shouldWatchDir(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
