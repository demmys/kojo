package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/snapshot"
)

// cleanFlags carries the parsed `--clean*` flag values from main.go.
type cleanFlags struct {
	target        string // "snapshots" | "legacy" | "blobs" | "agents" | "events" | "v0" | "v0-trash" | "all"
	apply         bool
	maxAgeDays    int
	keepLatest    int
	force         bool // --clean-force; only consulted by the v0 target
	minAgeDays    int  // --clean-min-age-days; only consulted by the v0-trash target
	logger        *slog.Logger
	configDirPath string
}

// runCleanCommand drives Phase 6 #18's `kojo --clean ...` housekeeping.
//
// The default mode is DRY-RUN: planned removals are printed but no
// files are touched. Operators add `--clean-apply` to commit. This
// matches the convention of other destructive admin tools (`docker
// system prune`, `gh repo gc`) — the safer default is a preview.
//
// Targets implemented today:
//
//   - "snapshots": drops <configdir>/snapshots/<TS>/ entries that are
//     either (a) older than --clean-max-age-days (default 7) AND not
//     among the --clean-keep-latest most-recent (default 3), or
//     (b) have no manifest.json (a partial / abandoned snapshot
//     dir that snapshot.Take() left behind on a failure)
//
//   - "legacy": drops post-Phase-2c-2 legacy on-disk files
//     (cron_paused, .cron_last, autosummary_marker, owner.token,
//     agent_tokens/<id>) ONLY when their canonical kv row exists.
//     Files without a kv mirror are reported but kept so the
//     runtime's lazy migration can still pick them up. See
//     clean_legacy.go for the inventory and the safety gate.
//
//   - "v0": soft-deletes the entire v0 dir (post-migration rollback
//     fallback) by renaming it to a sibling kojo.deleted-<ts>/.
//     Refuses without migration_complete.json, on manifest
//     divergence (overridable with --clean-force), on a running v0
//     binary, on symlinks, on non-dirs, and on trash-path
//     collisions. See clean_v0.go for the gate inventory. NOT
//     folded into "all" because v0 is the only destructive target
//     and operators must opt in explicitly.
//
//   - "v0-trash": physically removes kojo.deleted-<ts>/ siblings
//     produced by the "v0" target (slice 30). --clean-min-age-days
//     filters by the timestamp encoded in the dir name (CLI
//     default 7; pass --clean-min-age-days=0 explicitly to remove
//     every age, which defeats the recovery window). Anomalous
//     entries — non-dirs, symlinks, unparseable timestamps —
//     are reported but never auto-purged. Future-dated stamps land
//     in "KeepYoung" (after the cutoff) and are also rejected by
//     an apply-time future guard. Also NOT folded into "all"
//     because the trash dirs ARE the soft-delete recovery window.
//
//   - "blobs": removes unreferenced blob files, blob_refs rows marked
//     for GC past --clean-max-age-days, and empty directories under
//     the blob scope trees.
//
//   - "agents": hard-deletes soft-deleted agents past
//     --clean-max-age-days from the sqlite store.
//
//   - "events": prunes events/oplog_applied rows older than
//     --clean-max-age-days and records the deleted event floor so
//     /changes can report truncated cursors.
//
//   - "all": runs "snapshots" + "legacy". Intentionally excludes
//     "v0", "v0-trash", "blobs", "agents", and "events"; the latter
//     targets are explicit because they delete runtime data.
//
// Exit codes:
//
//	0 — nothing to do or apply succeeded
//	1 — error during scan / apply
//	2 — invalid flag combination
func runCleanCommand(f cleanFlags) int {
	if f.configDirPath == "" {
		f.configDirPath = configdir.Path()
	}
	if f.logger == nil {
		f.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if f.maxAgeDays <= 0 {
		f.maxAgeDays = 7
	}
	if f.keepLatest < 0 {
		f.keepLatest = 0
	}

	switch f.target {
	case "", "all", "snapshots", "legacy", "blobs", "agents", "events", "v0", "v0-trash":
		// fall through
	default:
		fmt.Fprintf(os.Stderr, "kojo: --clean target %q not recognized (try 'snapshots', 'legacy', 'blobs', 'agents', 'events', 'v0', 'v0-trash', or 'all')\n", f.target)
		return 2
	}

	runSnapshots := f.target == "" || f.target == "all" || f.target == "snapshots"
	runLegacy := f.target == "all" || f.target == "legacy"
	// v0 is intentionally NOT folded into "all". It is the only
	// destructive target that touches the v0 rollback dir, and an
	// operator running periodic `--clean all` snapshot/legacy
	// housekeeping should never lose their v0 fallback by accident.
	// Require explicit `--clean v0`.
	runV0 := f.target == "v0"
	// v0-trash is also explicit-only. The trash dirs ARE the
	// recovery window the soft-delete pattern preserves; folding
	// them into `all` would defeat the design. The CLI default
	// `--clean-min-age-days=7` keeps the same 7-day window the
	// §5.8 design specified; operators who want to purge every
	// age must pass `--clean-min-age-days=0` explicitly.
	runV0Trash := f.target == "v0-trash"
	runBlobs := f.target == "blobs"
	runAgents := f.target == "agents"
	runEvents := f.target == "events"

	// Build the seven targets behind a common interface. Each wraps
	// its subsystem's plan/print/apply triad; see clean_targets.go.
	// The shared env owns the lazily-opened sqlite handles (read-only
	// for the scan phase, read-write for apply) plus the deferred
	// cleanups, replacing the hand-managed defers the inline dispatch
	// used to carry.
	//
	// The legacy target opens its kv handle read-only (SQLite WAL admits
	// multiple readers alongside a live writer; ReadOnly also skips
	// migrations so clean never silently bumps the schema version) and
	// holds it across scan+apply so apply-time re-validation sees the
	// same connection.
	env := &cleanEnv{f: f}
	defer env.closeAll()

	var (
		snapT    *snapshotTarget
		legacyT  *legacyTarget
		v0TrashT *v0TrashTarget
		v0T      *v0Target
		blobT    *blobTarget
		agentT   *agentTarget
		eventT   *eventTarget
	)
	if runSnapshots {
		snapT = &snapshotTarget{f: f}
	}
	if runLegacy {
		legacyT = &legacyTarget{f: f, env: env}
	}
	if runV0Trash {
		v0TrashT = &v0TrashTarget{f: f}
	}
	if runV0 {
		v0T = &v0Target{f: f}
	}
	if runBlobs {
		blobT = &blobTarget{f: f, env: env}
	}
	if runAgents {
		agentT = &agentTarget{f: f, env: env}
	}
	if runEvents {
		eventT = &eventTarget{f: f, env: env}
	}

	// addTarget appends only constructed (non-nil) concrete pointers,
	// sidestepping the typed-nil-in-interface trap (a nil *snapshotTarget
	// boxed into a cleanTarget is itself non-nil).
	scanTargets := make([]cleanTarget, 0, 7)
	addScan := func(t cleanTarget, set bool) {
		if set {
			scanTargets = append(scanTargets, t)
		}
	}
	applyTargets := make([]cleanTarget, 0, 7)
	addApply := func(t cleanTarget, set bool) {
		if set {
			applyTargets = append(applyTargets, t)
		}
	}

	// Scan order — identical to the original inline sequence:
	// snapshots, legacy, v0-trash, v0, blobs, agents, events.
	addScan(snapT, snapT != nil)
	addScan(legacyT, legacyT != nil)
	addScan(v0TrashT, v0TrashT != nil)
	addScan(v0T, v0T != nil)
	addScan(blobT, blobT != nil)
	addScan(agentT, agentT != nil)
	addScan(eventT, eventT != nil)

	// Apply order differs from scan order: v0 is applied BEFORE v0-trash
	// (the soft-delete rename precedes physical trash removal).
	addApply(snapT, snapT != nil)
	addApply(legacyT, legacyT != nil)
	addApply(v0T, v0T != nil)
	addApply(v0TrashT, v0TrashT != nil)
	addApply(blobT, blobT != nil)
	addApply(agentT, agentT != nil)
	addApply(eventT, eventT != nil)

	for _, t := range scanTargets {
		if err := t.scan(); err != nil {
			return 1
		}
		t.print(f.apply)
	}

	if !f.apply {
		fmt.Fprintln(os.Stderr, "kojo: dry-run; pass --clean-apply to delete the listed entries")
		return 0
	}

	rc := 0
	for _, t := range applyTargets {
		if t.needsRWStore() {
			if err := env.ensureRWStore(); err != nil {
				f.logger.Error("clean: open store (read-write) failed", "err", err)
				return 1
			}
		}
		if errs := t.apply(); len(errs) > 0 {
			for _, e := range errs {
				f.logger.Error(t.applyErrMsg(), "err", e)
			}
			rc = 1
		}
	}
	return rc
}

// snapshotEntry is one candidate snapshot directory considered by the
// cleanup pass. PartialReason is non-empty when the directory has no
// manifest (corrupt / abandoned).
type snapshotEntry struct {
	Path          string
	ModTime       time.Time
	PartialReason string
}

// cleanPlan is the result of a scan: a list of paths that the apply
// step would remove. Categorized so the printout can explain why
// each entry is on the list.
type cleanPlan struct {
	StaleSnapshots   []snapshotEntry
	PartialSnapshots []snapshotEntry
	Kept             []snapshotEntry
}

func planSnapshotCleanup(f cleanFlags) (*cleanPlan, error) {
	dir := filepath.Join(f.configDirPath, "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &cleanPlan{}, nil // nothing to do
		}
		return nil, err
	}

	var all []snapshotEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		entry := snapshotEntry{Path: path, ModTime: info.ModTime()}
		// A snapshot is "complete" iff it has a parsable manifest.
		// We use snapshot.LoadManifest so the validation logic stays
		// in one place — version-bumps that change manifest shape only
		// need to update the package.
		if _, mErr := snapshot.LoadManifest(path); mErr != nil {
			entry.PartialReason = mErr.Error()
		}
		all = append(all, entry)
	}

	// Sort newest-first so the keep-latest cut is easy.
	slices.SortFunc(all, func(a, b snapshotEntry) int { return b.ModTime.Compare(a.ModTime) })

	plan := &cleanPlan{}
	cutoff := cutoffTime(f.maxAgeDays)

	for i, e := range all {
		switch {
		case e.PartialReason != "":
			plan.PartialSnapshots = append(plan.PartialSnapshots, e)
		case i < f.keepLatest:
			// Always keep the N most-recent successful snapshots,
			// regardless of age. Operators rely on at least 1
			// reachable snapshot at all times.
			plan.Kept = append(plan.Kept, e)
		case e.ModTime.Before(cutoff):
			plan.StaleSnapshots = append(plan.StaleSnapshots, e)
		default:
			plan.Kept = append(plan.Kept, e)
		}
	}
	return plan, nil
}

func printCleanPlan(plan *cleanPlan, apply bool) {
	verb := cleanVerb(apply)
	if n := len(plan.PartialSnapshots); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d partial snapshot dir(s):\n", verb, n)
		for _, e := range plan.PartialSnapshots {
			fmt.Fprintf(os.Stderr, "  %s  (no manifest: %s)\n", e.Path, shortReason(e.PartialReason))
		}
	}
	if n := len(plan.StaleSnapshots); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d stale snapshot dir(s):\n", verb, n)
		for _, e := range plan.StaleSnapshots {
			fmt.Fprintf(os.Stderr, "  %s  (mtime=%s)\n", e.Path, e.ModTime.UTC().Format(time.RFC3339))
		}
	}
	if n := len(plan.Kept); n > 0 {
		fmt.Fprintf(os.Stderr, "keeping %d snapshot dir(s)\n", n)
	}
	if len(plan.PartialSnapshots)+len(plan.StaleSnapshots) == 0 {
		fmt.Fprintln(os.Stderr, "no snapshot cleanup needed")
	}
}

func applyCleanPlan(plan *cleanPlan) []error {
	var errs []error
	for _, e := range append(plan.PartialSnapshots, plan.StaleSnapshots...) {
		if err := os.RemoveAll(e.Path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
		}
	}
	return errs
}

// shortReason trims a manifest error to one line so the dry-run output
// stays scannable.
func shortReason(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
