package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fileWatchDebounce coalesces a burst of writes (the agent CLI saving a
// file, atomicfile's tmp+rename, an editor's multi-write save) into a
// single disk→DB flush per agent.
const fileWatchDebounce = 750 * time.Millisecond

var errFileWatcherAdd = errors.New("file watcher add failed")

// fileWatcher reflects an agent CLI's direct disk writes (MEMORY.md,
// memory/**/*.md, persona.md, user.md, checkin.md) into the DB promptly,
// instead of waiting for the next prepareChat's lazy, best-effort sync.
// Without it a Web UI poll or a cross-peer proxied read observes content
// the agent already wrote but the DB hasn't caught up to — stale memory,
// a missing diary entry, an old persona.
//
// Design: ONE recursive watch rooted at agentsDir() for the whole
// process — no per-agent start/stop. Each event is mapped back to its
// agentID and a per-agent DEBOUNCED flush runs ONLY when this peer
// currently holds the agent (Manager.holdsLocally).
//
// The holder gate is the load-bearing safety check: flushing a
// non-held agent's stale leftover files would push old bodies into the
// DB and roll back the real holder's state — the exact bug
// ReconcileAgentDiskFromDB guards against on the device-switch arrival
// side. The flush is disk→DB only and every sync is sha-idempotent, so
// the watcher does not feed back on itself (a hydrate-driven disk write
// re-fires the watcher, but the follow-up flush is a no-op). It is a
// freshness OPTIMIZATION layered over the existing prepareChat sync — a
// missed event degrades to the old lazy behaviour, never to data loss.
type fileWatcher struct {
	mgr    *Manager
	logger *slog.Logger
	root   string
	w      *fsnotify.Watcher

	mu     sync.Mutex
	timers map[string]*time.Timer
	closed bool
	wg     sync.WaitGroup // tracks in-flight flush callbacks
}

// newFileWatcher creates the watcher and registers the agents-root
// subtree. The caller must invoke run() in its own goroutine.
func newFileWatcher(mgr *Manager) (*fileWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fileWatcher{
		mgr:    mgr,
		logger: mgr.logger,
		root:   agentsDir(),
		w:      w,
		timers: make(map[string]*time.Timer),
	}
	// Ensure the root exists so the initial Add succeeds on a fresh
	// install with no agents yet; per-agent dirs are added as their
	// CREATE events arrive.
	if err := os.MkdirAll(fw.root, 0o755); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := fw.addTree(fw.root); err != nil {
		_ = w.Close()
		return nil, err
	}
	return fw, nil
}

// addTree registers only directories that can contain canonical workspace
// mirrors: the agents root, each direct agent directory, and memory/**.
// fsnotify is non-recursive, so each relevant directory is added
// individually; new directories are picked up on their CREATE event in
// handle().
//
// Agent directories also contain arbitrary workspaces, repositories,
// node_modules, and CLI state. Recursively watching those trees can exhaust
// Linux's per-user inotify watch limit (ENOSPC) without improving DB
// freshness, because none of their files are mirrored by this watcher.
func (fw *fileWatcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; best-effort
		}
		if !d.IsDir() {
			return nil
		}
		if !fw.shouldWatchDir(path) {
			return filepath.SkipDir
		}
		if aerr := fw.w.Add(path); aerr != nil {
			return fmt.Errorf("%w: %s: %w", errFileWatcherAdd, path, aerr)
		}
		return nil
	})
}

// shouldWatchDir reports whether path is one of the narrow directory classes
// whose contents can affect the DB mirror.
func (fw *fileWatcher) shouldWatchDir(path string) bool {
	rel, err := filepath.Rel(fw.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if rel == "." {
		return true
	}
	segs := strings.Split(rel, string(filepath.Separator))
	return len(segs) == 1 || (len(segs) >= 2 && segs[1] == "memory")
}

// run is the event loop. Exits when the watcher's channels close
// (Close()).
func (fw *fileWatcher) run() {
	for {
		select {
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			fw.handle(ev)
		case err, ok := <-fw.w.Errors:
			if !ok {
				return
			}
			if fw.logger != nil {
				fw.logger.Debug("file-watch: watcher error", "err", err)
			}
		}
	}
}

func (fw *fileWatcher) handle(ev fsnotify.Event) {
	// A newly created directory (an agent dir on create / switch
	// arrival, or a new memory/ subtree) must be added so files inside
	// it are watched too. Files dropped into it BEFORE the Add lands
	// (e.g. a switch-arrival hydrate writes a whole tree at once) would
	// otherwise be missed, so also schedule a flush for the owning
	// agent — the disk→DB sync walks the tree and picks them all up.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if !fw.shouldWatchDir(ev.Name) {
				return
			}
			if err := fw.addTree(ev.Name); err != nil && fw.logger != nil {
				fw.logger.Warn("file-watch: add dir failed", "path", ev.Name, "err", err)
			}
			if aid := fw.agentIDFor(ev.Name); aid != "" {
				fw.schedule(aid)
			}
			return
		}
	}
	// Only canonical markdown (plus the status.json workspace mirror)
	// matters. Skip tmp files (atomicfile writes <name>.<rand>.tmp then
	// renames), the embedding db, and any other churn.
	if !strings.HasSuffix(ev.Name, ".md") && filepath.Base(ev.Name) != "status.json" {
		return
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return
	}
	agentID := fw.agentIDFor(ev.Name)
	if agentID == "" {
		return
	}
	fw.schedule(agentID)
}

// agentIDFor maps an event path to the owning agent ID: the first path
// segment under the watch root. Returns "" for the root itself or a
// path that escapes it.
func (fw *fileWatcher) agentIDFor(path string) string {
	rel, err := filepath.Rel(fw.root, path)
	if err != nil {
		return ""
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

// schedule (re)arms the per-agent debounce timer.
func (fw *fileWatcher) schedule(agentID string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.closed {
		return
	}
	if t, ok := fw.timers[agentID]; ok {
		t.Reset(fileWatchDebounce)
		return
	}
	fw.timers[agentID] = time.AfterFunc(fileWatchDebounce, func() {
		fw.mu.Lock()
		delete(fw.timers, agentID)
		if fw.closed {
			// Close raced ahead between the timer firing and this
			// callback acquiring the lock — don't start a flush that
			// could run against an already-closed store.
			fw.mu.Unlock()
			return
		}
		fw.wg.Add(1)
		fw.mu.Unlock()
		defer fw.wg.Done()
		fw.flush(agentID)
	})
}

// flush runs the disk→DB sync for one agent, gated on local holding.
func (fw *fileWatcher) flush(agentID string) {
	if !fw.mgr.holdsLocally(agentID) {
		return
	}
	st := getGlobalStore()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := SyncAgentMemoryFromDiskBestEffort(ctx, st, agentID, fw.logger); err != nil && fw.logger != nil {
		fw.logger.Debug("file-watch: memory flush failed", "agent", agentID, "err", err)
	}
	if err := SyncAgentPersonaFromDisk(ctx, st, agentID, fw.logger); err != nil && fw.logger != nil {
		fw.logger.Debug("file-watch: persona flush failed", "agent", agentID, "err", err)
	}
}

// Close stops the timers, waits for any in-flight flush to finish, and
// closes the underlying watcher; run() returns once the event channels
// drain. Waiting on wg before returning guarantees no flush is still
// touching the store when the caller (Manager.Close) proceeds to close
// it.
func (fw *fileWatcher) Close() error {
	fw.mu.Lock()
	if fw.closed {
		fw.mu.Unlock()
		return nil
	}
	fw.closed = true
	for _, t := range fw.timers {
		t.Stop()
	}
	fw.timers = nil
	fw.mu.Unlock()
	fw.wg.Wait()
	return fw.w.Close()
}
