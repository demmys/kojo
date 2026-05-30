package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/loppo-llc/kojo/internal/store"
)

// SyncAgentPersonaFromDisk reconciles the on-disk persona.md with the
// agent_persona DB row. It is the pure disk→DB half of
// Manager.syncPersona — WITHOUT the in-memory Agent.Persona update, the
// publicProfile regeneration, or the AcquireMutation device-switch gate.
//
// The gate omission is deliberate: this function is called from the
// device-switch source-flush (switch_device_handler step 0-pre) AFTER
// SetSwitching(true) is already set. Manager.syncPersona refuses to run
// in that window (AcquireMutation returns an error once switching is
// set), so the closing turn's persona.md edit would otherwise never
// reach the DB row that buildAgentSyncRequest ships to the target —
// silently rolling the persona back on the destination peer.
//
// Semantics mirror syncAgentMemoryToDB (DB-canonical, disk mirror):
//   - file present, body differs from DB → UPSERT disk→DB (disk wins)
//   - file present, body matches DB       → no-op
//   - file missing + live non-empty DB row → HYDRATE disk from DB
//   - file missing + no/empty/tombstoned row → no-op
//
// A missing file is NEVER treated as a clear: clearing persona must
// round-trip through PutAgentPersona / Manager.Update so an operator
// `rm persona.md` cannot wipe the canonical row. Idempotent and safe to
// call from any known mutation hook.
//
// Locking: holds personaSyncMu(agentID) for the whole read→write
// critical section so a concurrent PutAgentPersona can't interleave.
// It takes ONLY personaSyncMu (never memorySyncMu), so it cannot invert
// the memorySyncMu→personaSyncMu order and deadlock.
func SyncAgentPersonaFromDisk(ctx context.Context, st *store.Store, agentID string, logger *slog.Logger) error {
	if st == nil {
		return nil
	}

	release := lockPersonaSync(agentID)
	defer release()

	content, fileExists, readErr := readPersonaForSync(agentID)
	if readErr != nil {
		return fmt.Errorf("sync persona.md: read: %w", readErr)
	}

	prev, err := st.GetAgentPersona(ctx, agentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("sync persona.md: read DB row: %w", err)
	}

	if !fileExists {
		// Disk-hydrate path. Symmetrical with syncAgentMemoryToDB:
		// missing disk + live non-empty row means the importer / a
		// peer-sync populated the DB but disk wasn't written yet, OR
		// the operator wiped the file. Hydrate disk from DB rather
		// than tombstoning — a missing file is not a clear.
		if prev != nil && prev.DeletedAt == nil && prev.Body != "" {
			if werr := writePersonaFile(agentID, prev.Body); werr != nil {
				return fmt.Errorf("sync persona.md: hydrate disk: %w", werr)
			}
			if logger != nil {
				logger.Debug("persona.md sync: hydrated disk from DB row",
					"agent", agentID, "size", len(prev.Body))
			}
		}
		return nil
	}

	if prev != nil && prev.Body == content && prev.DeletedAt == nil {
		return nil // already in sync
	}

	if _, err := st.UpsertAgentPersona(ctx, agentID, content, "", store.AgentInsertOptions{
		AllowOverwrite: true,
	}); err != nil {
		// ErrNotFound means the agent row itself is gone (race against
		// Delete) — treat as a no-op, the next sync of a live agent wins.
		if errors.Is(err, store.ErrNotFound) {
			if logger != nil {
				logger.Debug("persona.md sync: agent row missing", "agent", agentID)
			}
			return nil
		}
		return fmt.Errorf("sync persona.md: upsert: %w", err)
	}
	return nil
}
