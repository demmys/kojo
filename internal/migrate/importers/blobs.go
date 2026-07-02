package importers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// errSkipInvalidPath signals that publishBlob refused to publish the
// leaf because v1's blob layer rejected the logical path (NFC,
// reserved chars, etc.). The Run loop catches this so a single
// pathological leaf or agent-id-derived path doesn't take down the
// whole migration.
var errSkipInvalidPath = errors.New("blobs: v1 path validation refused leaf")

// blobsImporter migrates the per-agent file artefacts that are allowed
// to live in the v1 native blob store. Domain key: "blobs". Runs after
// agents so blob_refs rows can FK against the agents table indirectly
// via URI convention (the schema does not enforce that FK because cas /
// handoff / orphan repair all expect blob_refs to outlive a deleted
// agent).
//
// Mapping:
//
//	v0 agents/<id>/avatar.{png,svg,jpg,jpeg,webp} → kojo://global/agents/<id>/avatar.<ext>
//
// Hidden CLI workspace dirs (.claude/, .codex/) are NOT blob_refs
// rows. They may be preserved by agentsImporter's local CWD copy, but
// they must never become multi-device blob payloads: these dirs are
// local CLI scratch that is either kojo-managed and regenerated on
// first Chat (backend_claude.go writes .claude/settings.local.json) or
// CLI-owned and outside kojo's write path (backend_codex.go only sets
// cmd.Dir — any .codex/ that appears under agentDir is whatever the
// codex binary chose to drop). The cross-path continuity that matters
// for CLI transcripts is handled separately by `--migrate-external-cli`
// (internal/migrate/externalcli) via symlink.
//
// credentials.{json,key} are NOT blob_refs rows. agentsImporter may
// preserve them as local CWD files during v0→v1 migration, but runtime
// credentials are owned by internal/agent/credential.go's canonical
// encrypted store at <configdir>/credentials.db (with its host-bound
// encryption key at <configdir>/credentials.key). Importing per-agent
// credential leaves as blobs would create machine-bound secret
// artefacts that device switch must never replicate.
//
// .cron_last is also NOT a blob_refs row, but for a different reason:
// after Phase 2c-2 slice 12 the cron throttle moved to the kv table
// at (namespace="scheduler", key="cron_last/<agentID>", scope=
// machine). The v0 dotfile is no longer canonical state on either
// side of the cutover — its 50s mtime would be useless after the
// upgrade transient — and runtime acquireCronLock best-effort
// unlinks it on each tick. Nothing imports its value; a stray file
// surviving the migration is harmless cruft.
type blobsImporter struct{}

const blobsDomain = "blobs"

func (blobsImporter) Domain() string { return blobsDomain }

// blobMapping captures one (v0 leaf → v1 blob URI) edge. relPath is the
// V0Dir-relative form used for the source-path checksum; absPath is
// the on-disk leaf opened by openV0 / streamPutBlob; scope/path is the
// destination address inside the v1 blob store.
type blobMapping struct {
	relPath string
	absPath string
	scope   blob.Scope
	path    string // logical path under scope (forward slashes)
}

func (blobsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	return runDomain(ctx, st, blobsDomain, func(logger *slog.Logger) (int, string, error) {
		// Scan v0 first. The checksum (and the per-leaf op list) are both
		// derived from the same walk so source_checksum can never claim a
		// file the importer didn't actually consider. Pulls the known-agent
		// set from the v1 store rather than re-parsing agents.json so the
		// filter inherits whatever skip policy the agents importer applied
		// (empty name, schema-rejection rows, etc.).
		mappings, err := collectBlobMappings(ctx, st, opts.V0Dir)
		if err != nil {
			return 0, "", fmt.Errorf("collect blob mappings: %w", err)
		}
		relPaths := make([]string, 0, len(mappings))
		for _, m := range mappings {
			relPaths = append(relPaths, m.relPath)
		}
		checksum, err := domainChecksum(opts.V0Dir, relPaths)
		if err != nil {
			return 0, "", fmt.Errorf("checksum blobs sources: %w", err)
		}

		if len(mappings) == 0 {
			// Empty v0 dir / no agents — still record the domain so a re-run
			// can early-exit via alreadyImported.
			return 0, checksum, nil
		}

		// Build a Store for the v1 blob tree. We construct it locally rather
		// than threading a *blob.Store through migrate.Options so internal/
		// migrate stays free of an internal/blob import; the importer is
		// the only consumer that cares.
		homePeer := opts.HomePeer
		if homePeer == "" {
			h, _ := os.Hostname()
			if h == "" {
				h = "kojo-local"
			}
			homePeer = h
		}
		bs := blob.New(opts.V1Dir,
			blob.WithRefs(blob.NewStoreRefs(st, homePeer)),
			blob.WithHomePeer(homePeer),
		)

		imported := 0
		skipped := 0
		skippedInvalid := 0
		for _, m := range mappings {
			// Resume decision: skip iff (a) the blob_refs row is present
			// AND (b) the on-disk body matches what the row claims AND
			// (c) the body matches the v0 source. Any divergence — row
			// missing, row ↔ fs drift, fs ↔ src drift — re-publishes so
			// the partial-run hole closes.
			decision, err := blobResumeDecision(opts.V0Dir, bs, m, logger)
			switch {
			case errors.Is(err, errSkipInvalidPath):
				logger.Warn("skipping v0 blob with v1-invalid path",
					"v0_path", m.absPath, "logical", m.path, "err", err)
				skippedInvalid++
				continue
			case err != nil:
				return 0, "", err
			case decision == decisionSkipFresh:
				skipped++
				continue
			}

			if err := publishBlob(opts.V0Dir, bs, m); err != nil {
				if errors.Is(err, errSkipInvalidPath) {
					logger.Warn("skipping v0 blob with v1-invalid path",
						"v0_path", m.absPath, "logical", m.path, "err", err)
					skippedInvalid++
					continue
				}
				return 0, "", fmt.Errorf("publish %s: %w", m.path, err)
			}
			imported++
		}

		if skipped > 0 || skippedInvalid > 0 {
			logger.Info("blobs partial-run resume",
				"imported", imported,
				"skipped_already_published", skipped,
				"skipped_invalid_path", skippedInvalid)
		}
		return imported, checksum, nil
	})
}

// blobResumeDecision encapsulates the resume policy for one mapping.
// Returns decisionSkipFresh when the v1 state is fully aligned with
// v0 (ref row, fs body, and source all agree); decisionPublish when
// any divergence requires a re-publish; or errSkipInvalidPath when
// either the path or the scope is rejected by v1's blob layer.
//
// Each branch logs at warn level so resume drift is visible in the
// migration log without re-walking the dir afterwards.
type resumeDecision int

const (
	decisionPublish   resumeDecision = 0
	decisionSkipFresh resumeDecision = 1
)

func blobResumeDecision(v0Dir string, bs *blob.Store, m blobMapping, logger *slog.Logger) (resumeDecision, error) {
	obj, hErr := bs.Head(m.scope, m.path)
	switch {
	case errors.Is(hErr, blob.ErrInvalidPath), errors.Is(hErr, blob.ErrInvalidScope):
		// Path validation surfaced the same v1 rejection publishBlob
		// would hit — bubble through the skip channel so the migration
		// doesn't abort on a single pathological leaf.
		return decisionPublish, fmt.Errorf("%w: %v", errSkipInvalidPath, hErr)
	case errors.Is(hErr, blob.ErrNotFound):
		// fs body missing — straightforward "first publish" case.
		return decisionPublish, nil
	case hErr != nil:
		return decisionPublish, fmt.Errorf("head %s: %w", m.path, hErr)
	}
	// Object exists on disk. obj.SHA256 comes from the blob_refs cache
	// (populateDigestFromRefs); empty means the row is missing — the
	// classic "fs publish committed, ref insert crashed" partial-run
	// case. Re-publish so Put rewrites both halves.
	if obj.SHA256 == "" {
		logger.Warn("blob fs body present but ref row missing — re-publishing",
			"uri", "kojo://"+string(m.scope)+"/"+m.path)
		return decisionPublish, nil
	}
	// Verify the on-disk body actually hashes to what the ref claims —
	// a row whose SHA256 disagrees with the file is exactly the scrub-
	// repairs case, and the cheapest repair is a fresh Put.
	actual, vErr := bs.Verify(m.scope, m.path)
	switch {
	case errors.Is(vErr, blob.ErrInvalidPath), errors.Is(vErr, blob.ErrInvalidScope):
		return decisionPublish, fmt.Errorf("%w: %v", errSkipInvalidPath, vErr)
	case errors.Is(vErr, blob.ErrNotFound):
		// Race against another writer between Head and Verify. Treat
		// like a missing fs body and re-publish.
		return decisionPublish, nil
	case vErr != nil:
		return decisionPublish, fmt.Errorf("verify %s: %w", m.path, vErr)
	}
	if actual.SHA256 != obj.SHA256 {
		logger.Warn("blob ref ↔ fs digest drift — re-publishing",
			"uri", "kojo://"+string(m.scope)+"/"+m.path,
			"ref_sha256", obj.SHA256, "fs_sha256", actual.SHA256)
		return decisionPublish, nil
	}
	srcDigest, cerr := fileChecksumRO(v0Dir, m.absPath)
	if cerr != nil {
		return decisionPublish, fmt.Errorf("hash %s: %w", m.absPath, cerr)
	}
	if actual.SHA256 == srcDigest {
		return decisionSkipFresh, nil
	}
	logger.Warn("blob fs body diverged from v0 source — re-publishing",
		"uri", "kojo://"+string(m.scope)+"/"+m.path,
		"fs_sha256", actual.SHA256, "src_sha256", srcDigest)
	return decisionPublish, nil
}

// publishBlob streams one v0 leaf into the v1 blob store. Open via
// openV0 (read-only + symlink-escape guard) so the migration cannot be
// coerced into reading outside V0Dir even by a hostile symlink planted
// during a partial run.
//
// A v1 path-validation failure (NFC, reserved chars, control bytes,
// etc.) is wrapped into errSkipInvalidPath so the importer's main loop
// can warn-and-skip rather than abort the entire migration on a single
// pathological leaf.
func publishBlob(v0Dir string, bs *blob.Store, m blobMapping) error {
	f, err := openV0(v0Dir, m.absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := bs.Put(m.scope, m.path, f, blob.PutOptions{}); err != nil {
		if errors.Is(err, blob.ErrInvalidPath) || errors.Is(err, blob.ErrInvalidScope) {
			return fmt.Errorf("%w: %v", errSkipInvalidPath, err)
		}
		return err
	}
	return nil
}

// knownAgentIDs returns the set of agent ids that the agents importer
// actually committed. Reading from the v1 store (rather than re-parsing
// agents.json) means blobsImporter inherits whatever skip policy the
// agents importer applied — empty name, schema-rejection rows,
// malformed timestamps that surfaced as Unmarshal errors — without
// duplicating that decision tree here. The importer registration order
// guarantees agentsImporter ran first.
func knownAgentIDs(ctx context.Context, st *store.Store) (map[string]struct{}, error) {
	ags, err := st.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	ids := make(map[string]struct{}, len(ags))
	for _, a := range ags {
		ids[a.ID] = struct{}{}
	}
	return ids, nil
}

// collectBlobMappings walks v0/agents/<id>/ for every leaf allowed to
// become a blob URI. The list is stable (sorted by relPath) so re-runs
// under identical v0 state produce identical migration_status
// imported_count and identical mapping order in logs.
//
// Only agent ids declared in agents.json are included — orphan dirs
// (backup copies, partial-delete leftovers) are skipped to keep
// blob_refs from referencing agents that agentsImporter itself
// declined to migrate.
//
// Symlink leaves and parent-component symlinks are silently skipped —
// the importer already refuses to read through them via openV0, but
// here we keep the policy explicit so a hostile sync that planted
// `agents/<id>/avatar.png` as a symlink can't trick the source-path
// list into "covering" a file the publish loop will refuse.
func collectBlobMappings(ctx context.Context, st *store.Store, v0Dir string) ([]blobMapping, error) {
	var out []blobMapping
	base := agentsBaseDir(v0Dir)
	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}

	known, err := knownAgentIDs(ctx, st)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// "groupdms" is a sibling kind handled by groupdmsImporter; it
		// holds no blob artefacts.
		if e.Name() == "groupdms" {
			continue
		}
		agentID := e.Name()
		if _, ok := known[agentID]; !ok {
			// Orphan agent dir — skip outright. Surfacing it via the
			// checksum is acceptable (collectAgentsSourcePaths already
			// includes orphan persona/MEMORY for drift detection); the
			// blob copy belongs only to live agents.
			continue
		}
		agentRoot := filepath.Join(base, agentID)

		// Avatars: avatar.{png,svg,jpg,jpeg,webp}. Multiple could
		// exist (rare) — publish each independently because the URI
		// suffix differs. The probed list mirrors runtime
		// avatarExtProbe (internal/agent/avatar.go); a v0 install
		// with avatar.gif (never accepted by v0's IsAllowedImageExt
		// either, only possible from a hand-edited dir) is left in
		// the v0 dir untouched — runtime can't render it post-
		// cutover and migrating it would just create an orphan blob.
		for _, ext := range []string{"png", "svg", "jpg", "jpeg", "webp"} {
			leaf := filepath.Join(agentRoot, "avatar."+ext)
			if mapping, ok, err := blobLeaf(v0Dir, leaf,
				blob.ScopeGlobal,
				"agents/"+agentID+"/avatar."+ext); err != nil {
				return nil, err
			} else if ok {
				out = append(out, mapping)
			}
		}

		// Everything else under agentRoot is intentionally left out
		// of blob_refs. MEMORY.md and memory/ ride typed DB tables;
		// index/ is rebuilt per peer; credentials are in
		// credentials.db; arbitrary agent-created files are local
		// working-directory state, not multi-device blob payload.
	}
	return out, nil
}

// blobLeaf builds a blobMapping for a single leaf, returning ok=false
// if the leaf is missing or non-regular. Errors other than ErrNotExist
// surface up so a permission glitch on a known leaf doesn't silently
// drop the row from the migration.
func blobLeaf(v0Dir, leaf string, scope blob.Scope, logical string) (blobMapping, bool, error) {
	st, err := os.Lstat(leaf)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return blobMapping{}, false, nil
		}
		return blobMapping{}, false, fmt.Errorf("lstat %s: %w", leaf, err)
	}
	if !st.Mode().IsRegular() {
		return blobMapping{}, false, nil
	}
	if err := assertUnderRoot(v0Dir, leaf); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return blobMapping{}, false, nil
		}
		return blobMapping{}, false, err
	}
	rel, err := filepath.Rel(v0Dir, leaf)
	if err != nil {
		return blobMapping{}, false, fmt.Errorf("rel %s: %w", leaf, err)
	}
	return blobMapping{
		relPath: filepath.ToSlash(rel),
		absPath: leaf,
		scope:   scope,
		path:    logical,
	}, true, nil
}
