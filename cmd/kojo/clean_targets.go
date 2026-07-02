package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// cleanTarget unifies the plan / print / apply lifecycle of the seven
// `--clean` targets so runCleanCommand can loop instead of hand-
// dispatching each one. Each concrete target wraps the existing
// plan*/print*/apply* triad for its subsystem; the wrappers preserve
// the exact log messages, store lifecycle, and per-target quirks the
// hand-written dispatch used to encode.
//
// scan is self-logging: it emits the same slog.Error line the original
// inline code did and returns a non-nil error to signal "runCleanCommand
// should return 1". apply returns accumulated errors; the caller logs
// each with applyErrMsg (the single-error targets — v0, agents, events —
// return a one-element slice so their one message is logged once, exactly
// as before).
type cleanTarget interface {
	scan() error
	print(apply bool)
	needsRWStore() bool
	apply() []error
	applyErrMsg() string
}

// cleanVerb folds the `verb := "would remove" / "removing"` idiom used
// by the snapshot, legacy, blob, and v0-trash printers. Targets whose
// verb differs (agents "hard-delete", events "prune", v0 "soft-delete")
// keep their own strings.
func cleanVerb(apply bool) string {
	if apply {
		return "removing"
	}
	return "would remove"
}

// cutoffMillis folds the `store.NowMillis() - days*24h` cutoff used by
// the sqlite-backed targets (blobs, agents, events).
func cutoffMillis(maxAgeDays int) int64 {
	return store.NowMillis() - int64(maxAgeDays)*24*60*60*1000
}

// cutoffTime folds the wall-clock `time.Now() - days*24h` cutoff used by
// the filesystem-mtime / name-stamp targets (snapshots, v0-trash). The
// returned instant is only ever fed to time.Time comparison methods,
// which are location-independent, so callers that previously used
// time.Now().UTC() are unaffected.
func cutoffTime(days int) time.Time {
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

// removeIfExists folds the `os.Remove(p); err != nil && !ErrNotExist`
// idiom the apply loops repeat. It returns the raw os.Remove error so
// callers can wrap it in their per-target message; a missing file is
// folded into success (a concurrent runtime migration / operator rm may
// have unlinked it already).
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// cleanEnv holds the shared store handles the store-backed targets open
// lazily, plus the deferred cleanups runCleanCommand runs on return. It
// preserves the original single-open-per-group semantics: the read-only
// store is opened once on the first store target's scan, the read-write
// store once on the first store target's apply.
type cleanEnv struct {
	f       cleanFlags
	closers []func()

	roStore  *store.Store
	roOpened bool
	roErr    error

	rwStore  *store.Store
	rwCtx    context.Context
	rwOpened bool
}

// onClose registers a cleanup to run at runCleanCommand return.
func (e *cleanEnv) onClose(fn func()) { e.closers = append(e.closers, fn) }

// closeAll runs the registered cleanups LIFO, matching the defer order
// of the original inline opens.
func (e *cleanEnv) closeAll() {
	for i := len(e.closers) - 1; i >= 0; i-- {
		e.closers[i]()
	}
}

// ensureROStore opens the shared read-only store on first use. It logs
// the original "clean: open store (read-only) failed" line itself so the
// caller only has to propagate the error and return 1.
func (e *cleanEnv) ensureROStore() (*store.Store, error) {
	if e.roOpened {
		return e.roStore, e.roErr
	}
	e.roOpened = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	e.onClose(cancel)
	st, err := openStore(ctx, e.f.configDirPath, true)
	if err != nil {
		e.f.logger.Error("clean: open store (read-only) failed", "err", err)
		e.roErr = err
		return nil, err
	}
	e.onClose(func() { _ = st.Close() })
	e.roStore = st
	return st, nil
}

// ensureRWStore opens the shared read-write store on first use. Unlike
// the read-only open it does NOT log — the apply loop logs
// "clean: open store (read-write) failed" and returns 1 on error, matching
// the original.
func (e *cleanEnv) ensureRWStore() error {
	if e.rwOpened {
		return nil
	}
	e.rwOpened = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	e.onClose(cancel)
	st, err := openStore(ctx, e.f.configDirPath, false)
	if err != nil {
		return err
	}
	e.onClose(func() { _ = st.Close() })
	e.rwStore = st
	e.rwCtx = ctx
	return nil
}

// --- snapshots (clean_cmd.go) ---

type snapshotTarget struct {
	f    cleanFlags
	plan *cleanPlan
}

func (t *snapshotTarget) scan() error {
	p, err := planSnapshotCleanup(t.f)
	if err != nil {
		t.f.logger.Error("clean: snapshot scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *snapshotTarget) print(apply bool)    { printCleanPlan(t.plan, apply) }
func (t *snapshotTarget) needsRWStore() bool  { return false }
func (t *snapshotTarget) apply() []error      { return applyCleanPlan(t.plan) }
func (t *snapshotTarget) applyErrMsg() string { return "clean: remove snapshot failed" }

// --- legacy (clean_legacy.go) ---
//
// The read-only kv handle is opened here and held on the target across
// scan+apply so apply-time re-validation sees the same connection.

type legacyTarget struct {
	f    cleanFlags
	env  *cleanEnv
	kv   *store.Store
	plan *legacyCleanPlan
}

func (t *legacyTarget) scan() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.env.onClose(cancel)
	st, err := openStore(ctx, t.f.configDirPath, true)
	if err != nil {
		t.f.logger.Error("clean: open kv store (read-only) failed", "err", err)
		return err
	}
	t.env.onClose(func() { _ = st.Close() })
	t.kv = st
	p, err := planLegacyCleanup(ctx, st, t.f.configDirPath)
	if err != nil {
		t.f.logger.Error("clean: legacy scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *legacyTarget) print(apply bool)    { printLegacyCleanPlan(t.plan, apply) }
func (t *legacyTarget) needsRWStore() bool  { return false }
func (t *legacyTarget) apply() []error      { return applyLegacyCleanPlan(t.plan, t.kv) }
func (t *legacyTarget) applyErrMsg() string { return "clean: remove legacy file failed" }

// --- v0-trash (clean_v0_trash.go) ---

type v0TrashTarget struct {
	f    cleanFlags
	plan *v0TrashCleanPlan
}

func (t *v0TrashTarget) scan() error {
	p, err := planV0TrashCleanup(t.f.minAgeDays, t.f.logger)
	if err != nil {
		t.f.logger.Error("clean: v0-trash scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *v0TrashTarget) print(apply bool)    { printV0TrashCleanPlan(t.plan, apply) }
func (t *v0TrashTarget) needsRWStore() bool  { return false }
func (t *v0TrashTarget) apply() []error      { return applyV0TrashCleanPlan(t.plan, t.f.logger) }
func (t *v0TrashTarget) applyErrMsg() string { return "clean: remove v0 trash dir failed" }

// --- v0 (clean_v0.go) ---
//
// --clean-force only matters when the plan flagged a ForceableReason;
// recording ForceUsed at scan time keeps the dry-run printout honest.

type v0Target struct {
	f    cleanFlags
	plan *v0CleanPlan
}

func (t *v0Target) scan() error {
	p, err := planV0Cleanup(t.f.configDirPath, t.f.logger)
	if err != nil {
		t.f.logger.Error("clean: v0 scan failed", "err", err)
		return err
	}
	if p != nil && t.f.force {
		p.ForceUsed = true
	}
	t.plan = p
	return nil
}
func (t *v0Target) print(apply bool)   { printV0CleanPlan(t.plan, apply) }
func (t *v0Target) needsRWStore() bool { return false }
func (t *v0Target) apply() []error {
	if err := applyV0CleanPlan(t.plan, t.f.logger); err != nil {
		return []error{err}
	}
	return nil
}
func (t *v0Target) applyErrMsg() string { return "clean: v0 soft-delete failed" }

// --- blobs (clean_store_targets.go) ---

type blobTarget struct {
	f    cleanFlags
	env  *cleanEnv
	plan *blobCleanPlan
}

func (t *blobTarget) scan() error {
	st, err := t.env.ensureROStore()
	if err != nil {
		return err
	}
	p, err := planBlobCleanup(context.Background(), st, t.f.configDirPath, t.f.maxAgeDays)
	if err != nil {
		t.f.logger.Error("clean: blob scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *blobTarget) print(apply bool)    { printBlobCleanPlan(t.plan, apply) }
func (t *blobTarget) needsRWStore() bool  { return true }
func (t *blobTarget) apply() []error      { return applyBlobCleanPlan(t.env.rwCtx, t.plan, t.env.rwStore) }
func (t *blobTarget) applyErrMsg() string { return "clean: remove blob entry failed" }

// --- agents (clean_store_targets.go) ---

type agentTarget struct {
	f    cleanFlags
	env  *cleanEnv
	plan *agentCleanPlan
}

func (t *agentTarget) scan() error {
	st, err := t.env.ensureROStore()
	if err != nil {
		return err
	}
	p, err := planAgentCleanup(context.Background(), st, t.f.maxAgeDays)
	if err != nil {
		t.f.logger.Error("clean: agent scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *agentTarget) print(apply bool)   { printAgentCleanPlan(t.plan, apply) }
func (t *agentTarget) needsRWStore() bool { return true }
func (t *agentTarget) apply() []error {
	if err := applyAgentCleanPlan(t.env.rwCtx, t.plan, t.env.rwStore); err != nil {
		return []error{err}
	}
	return nil
}
func (t *agentTarget) applyErrMsg() string { return "clean: hard-delete agents failed" }

// --- events (clean_store_targets.go) ---

type eventTarget struct {
	f    cleanFlags
	env  *cleanEnv
	plan *eventCleanPlan
}

func (t *eventTarget) scan() error {
	st, err := t.env.ensureROStore()
	if err != nil {
		return err
	}
	p, err := planEventCleanup(context.Background(), st, t.f.maxAgeDays)
	if err != nil {
		t.f.logger.Error("clean: event scan failed", "err", err)
		return err
	}
	t.plan = p
	return nil
}
func (t *eventTarget) print(apply bool)   { printEventCleanPlan(t.plan, apply) }
func (t *eventTarget) needsRWStore() bool { return true }
func (t *eventTarget) apply() []error {
	if err := applyEventCleanPlan(t.env.rwCtx, t.plan, t.env.rwStore); err != nil {
		return []error{err}
	}
	return nil
}
func (t *eventTarget) applyErrMsg() string { return "clean: prune event rows failed" }
