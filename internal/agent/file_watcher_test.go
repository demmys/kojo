package agent

import (
	"context"
	"os"
	"path/filepath"
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

	fw, err := newFileWatcher(mgr)
	if err != nil {
		t.Fatalf("newFileWatcher: %v", err)
	}
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

	fw, err := newFileWatcher(mgr)
	if err != nil {
		t.Fatalf("newFileWatcher: %v", err)
	}
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
