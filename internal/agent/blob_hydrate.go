package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

// hydrateAgentDirFromBlobs used to materialize per-agent blob_refs
// entries into agentDir(id). That was too broad: historical delivery
// artifacts and arbitrary working-directory files could reappear in the
// agent CWD just because a stale blob_refs row existed. v1 now keeps the
// only portable file surfaces on their dedicated paths:
//
//   - MEMORY.md + memory/ are hydrated from typed DB tables by
//     SyncAgentMemoryFromDisk / ReconcileMemoryEntriesDiskFromDBHeld.
//   - avatar.<ext> is read through the avatar/blob API and is not a
//     CWD file.
//   - chat attachments are delivery artifacts in blob store and message
//     metadata; the agent-facing staging dir is .kojo/attach/.
//
// The function remains as a load-time cleanup hook for old deployments:
// it removes the legacy CWD attach/ tree created by the prior broad
// hydrate path, but it no longer copies blob_refs rows into the CWD.
func hydrateAgentDirFromBlobs(ctx context.Context, st *store.Store, bs *blob.Store, agentID string, logger *slog.Logger) error {
	if st == nil || bs == nil || agentID == "" {
		return nil
	}
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hydrate blobs: ensure agent dir: %w", err)
	}
	cleanupHydratedAttachDir(dir, agentID, logger)

	scopes := []struct {
		scope     blob.Scope
		uriPrefix string
	}{
		{blob.ScopeGlobal, "kojo://global/agents/" + agentID + "/"},
		{blob.ScopeLocal, "kojo://local/agents/" + agentID + "/"},
	}

	var firstErr error
	for _, s := range scopes {
		refs, err := st.ListBlobRefs(ctx, store.ListBlobRefsOptions{
			Scope:     string(s.scope),
			URIPrefix: s.uriPrefix,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list blob_refs (scope=%s): %w", s.scope, err)
			}
			if logger != nil {
				logger.Warn("hydrate blobs: list failed",
					"agent", agentID, "scope", s.scope, "err", err)
			}
			continue
		}
		for _, ref := range refs {
			encoded := strings.TrimPrefix(ref.URI, s.uriPrefix)
			if encoded == "" || encoded == ref.URI {
				// uri didn't carry the expected prefix despite the
				// SQL filter — defensive skip.
				continue
			}
			// blob_refs.uri is built via blob.BuildURI which percent-
			// encodes each path segment. CWD hydration is now handled
			// by the typed memory/workspace reconcilers, not by the
			// generic blob store, so we no longer materialize the blob
			// into the CWD — we only validate the encoding so a
			// malformed row still surfaces as an error.
			if _, derr := decodeURISegments(encoded); derr != nil {
				if logger != nil {
					logger.Warn("hydrate blobs: invalid percent-encoded URI",
						"agent", agentID, "uri", ref.URI, "err", derr)
				}
				if firstErr == nil {
					firstErr = derr
				}
				continue
			}
		}
	}
	return firstErr
}

// decodeURISegments reverses the per-segment percent-encoding that
// blob.BuildURI applies. Splits on "/" (which BuildURI deliberately
// preserves between segments to keep the prefix range scan working)
// and url.PathUnescapes each segment. Any decode error surfaces so
// the caller can warn-and-skip rather than feeding a malformed path
// into blob.Get / filepath.Join.
func decodeURISegments(encoded string) (string, error) {
	parts := strings.Split(encoded, "/")
	for i, p := range parts {
		dec, err := url.PathUnescape(p)
		if err != nil {
			return "", fmt.Errorf("decode segment %q: %w", p, err)
		}
		parts[i] = dec
	}
	return strings.Join(parts, "/"), nil
}

func cleanupHydratedAttachDir(dir, agentID string, logger *slog.Logger) {
	p := filepath.Join(dir, "attach")
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		if logger != nil {
			logger.Warn("hydrate blobs: inspect legacy attach dir failed",
				"agent", agentID, "path", p, "err", err)
		}
		return
	}
	if st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(p); err != nil && logger != nil {
			logger.Warn("hydrate blobs: remove legacy attach symlink failed",
				"agent", agentID, "path", p, "err", err)
		}
		return
	}
	if !st.IsDir() {
		return
	}
	if err := os.RemoveAll(p); err != nil && logger != nil {
		logger.Warn("hydrate blobs: remove legacy attach dir failed",
			"agent", agentID, "path", p, "err", err)
	}
}

// hydrateAgentBlobsAtLoad runs hydrateAgentDirFromBlobs with a fresh
// 30s context per agent, mirroring the SyncAgentMemoryFromDisk
// load-time invocation pattern (Manager.Load loops agents and runs
// each sync sequentially). Best-effort: log+continue on per-agent
// failure.
func hydrateAgentBlobsAtLoad(st *store.Store, bs *blob.Store, agentID string, logger *slog.Logger) {
	if st == nil || bs == nil {
		return
	}
	ctx, cancel := dbContextWithCancel(nil, 30*time.Second)
	defer cancel()
	if err := hydrateAgentDirFromBlobs(ctx, st, bs, agentID, logger); err != nil {
		if logger != nil {
			logger.Warn("hydrate blobs at load failed", "agent", agentID, "err", err)
		}
	}
}
