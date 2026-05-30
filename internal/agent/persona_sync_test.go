package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestSyncAgentPersonaFromDisk_InsertUpdateNoop covers the disk→DB
// happy path: a fresh persona.md creates the row; an edit updates it;
// an unchanged file is a no-op (etag stable).
func TestSyncAgentPersonaFromDisk_InsertUpdateNoop(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_persona")
	ctx := context.Background()
	logger := quietLogger()

	if err := writePersonaFile("ag_persona", "you are v1"); err != nil {
		t.Fatalf("write persona v1: %v", err)
	}
	if err := SyncAgentPersonaFromDisk(ctx, st, "ag_persona", logger); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rec, err := st.GetAgentPersona(ctx, "ag_persona")
	if err != nil {
		t.Fatalf("GetAgentPersona after insert: %v", err)
	}
	if rec.Body != "you are v1" {
		t.Errorf("body after insert = %q, want %q", rec.Body, "you are v1")
	}
	firstETag := rec.ETag

	// No-op: re-sync without editing the file keeps the etag stable.
	if err := SyncAgentPersonaFromDisk(ctx, st, "ag_persona", logger); err != nil {
		t.Fatalf("noop sync: %v", err)
	}
	rec2, _ := st.GetAgentPersona(ctx, "ag_persona")
	if rec2.ETag != firstETag {
		t.Errorf("etag drifted on no-op sync: %q -> %q", firstETag, rec2.ETag)
	}

	// Edit propagates disk→DB. This is the device-switch data-loss
	// guard: a persona edit made during the closing turn MUST reach
	// the DB row before the sync payload is built.
	if err := writePersonaFile("ag_persona", "you are v2"); err != nil {
		t.Fatalf("write persona v2: %v", err)
	}
	if err := SyncAgentPersonaFromDisk(ctx, st, "ag_persona", logger); err != nil {
		t.Fatalf("update sync: %v", err)
	}
	rec3, _ := st.GetAgentPersona(ctx, "ag_persona")
	if rec3.Body != "you are v2" {
		t.Errorf("body after update = %q, want %q", rec3.Body, "you are v2")
	}
}

// TestSyncAgentPersonaFromDisk_MissingFileDoesNotClobber asserts that a
// missing persona.md never wipes a live DB row — it hydrates disk from
// the row instead. An operator `rm persona.md` must not delete the
// canonical persona; clearing requires an explicit API round-trip.
func TestSyncAgentPersonaFromDisk_MissingFileDoesNotClobber(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_persona_hy")
	ctx := context.Background()
	logger := quietLogger()

	// Seed a live DB row, then ensure no on-disk file exists.
	if _, err := st.UpsertAgentPersona(ctx, "ag_persona_hy", "canonical body", "", store.AgentInsertOptions{AllowOverwrite: true}); err != nil {
		t.Fatalf("seed persona row: %v", err)
	}
	personaPath := filepath.Join(agentDir("ag_persona_hy"), "persona.md")
	_ = os.Remove(personaPath)

	if err := SyncAgentPersonaFromDisk(ctx, st, "ag_persona_hy", logger); err != nil {
		t.Fatalf("sync with missing file: %v", err)
	}

	// Row must be untouched (not cleared/tombstoned).
	rec, err := st.GetAgentPersona(ctx, "ag_persona_hy")
	if err != nil {
		t.Fatalf("GetAgentPersona: %v", err)
	}
	if rec.Body != "canonical body" || rec.DeletedAt != nil {
		t.Errorf("missing file clobbered the row: body=%q deleted=%v", rec.Body, rec.DeletedAt)
	}
	// Disk must have been hydrated from the DB row.
	got, err := os.ReadFile(personaPath)
	if err != nil {
		t.Fatalf("persona.md not hydrated from DB: %v", err)
	}
	if string(got) != "canonical body" {
		t.Errorf("hydrated disk = %q, want %q", string(got), "canonical body")
	}
}
