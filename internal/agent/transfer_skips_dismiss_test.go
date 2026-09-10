package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

func TestDismissTransferSkipsSurvivesStaleFullSaveAndNewTransfer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	disabled := ""
	a, err := m.Create(AgentConfig{Name: "A", Tool: "claude", CronExpr: &disabled})
	if err != nil {
		t.Fatal(err)
	}

	seed := func(opID, path string) {
		t.Helper()
		_, err := m.Store().UpdateAgent(context.Background(), a.ID, "", func(rec *store.AgentRecord) error {
			rec.Settings[transferSkipsKey] = []any{map[string]any{"path": path, "reason": "capacity"}}
			rec.Settings[transferSkipsOpIDKey] = opID
			delete(rec.Settings, transferSkipsDismissedGenKey)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ReloadAgentFromStore(a.ID); err != nil {
			t.Fatal(err)
		}
	}

	seed("op-old", "old.jsonl")
	stale, ok := m.Get(a.ID)
	if !ok || stale.LastTransferSkipsGeneration != "op-old" {
		t.Fatalf("stale snapshot = %+v, ok=%v", stale, ok)
	}
	acknowledged, err := m.DismissTransferSkips(a.ID, "op-old")
	if err != nil || !acknowledged {
		t.Fatalf("dismiss = %v, %v", acknowledged, err)
	}

	// Simulate an m.save snapshot captured before acknowledgement but landing
	// afterward. The unknown acknowledgement key must survive its row rewrite.
	if err := m.store.SaveAgentRowOnly(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.store.LoadByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.LastTransferSkips) != 0 {
		t.Fatalf("stale save resurrected warning: %+v", loaded.LastTransferSkips)
	}

	// Capture the acknowledged in-memory shape (no visible warning), then let
	// a new transfer land before that stale snapshot is persisted. Full saves
	// must preserve the newer DB-owned transfer metadata.
	acknowledgedSnapshot, ok := m.Get(a.ID)
	if !ok || len(acknowledgedSnapshot.LastTransferSkips) != 0 {
		t.Fatalf("acknowledged snapshot = %+v, ok=%v", acknowledgedSnapshot, ok)
	}
	seed("op-new", "new.jsonl")
	if err := m.store.SaveAgentRowOnly(acknowledgedSnapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err = m.store.LoadByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.LastTransferSkips) != 1 || loaded.LastTransferSkipsGeneration != "op-new" {
		t.Fatalf("new transfer warning did not reappear: %+v", loaded)
	}
}

func TestLegacyTransferSkipsGetsStableGeneration(t *testing.T) {
	settings := map[string]any{
		transferSkipsKey: []any{map[string]any{"path": "legacy.jsonl", "reason": "capacity"}},
	}
	first := transferSkipsGeneration(settings)
	second := transferSkipsGeneration(settings)
	if first == "" || first != second {
		t.Fatalf("legacy generation unstable: %q vs %q", first, second)
	}
}
