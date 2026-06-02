package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

func newHydrateBlobFixture(t *testing.T) (*store.Store, *blob.Store) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), ".config"))
	root := configdir.Path()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bs := blob.New(root,
		blob.WithRefs(blob.NewStoreRefs(st, "peer-test")),
		blob.WithHomePeer("peer-test"),
	)
	return st, bs
}

func TestHydrateAgentDirFromBlobsSkipsGenericBlobArtifacts(t *testing.T) {
	st, bs := newHydrateBlobFixture(t)
	ctx := context.Background()
	agentID := "ag_hydrate_attach"
	if _, err := bs.Put(blob.ScopeGlobal, "agents/"+agentID+"/books/readme.md",
		strings.NewReader("book"), blob.PutOptions{}); err != nil {
		t.Fatalf("put book: %v", err)
	}
	if _, err := bs.Put(blob.ScopeGlobal, "agents/"+agentID+"/attach/m_1/chart.png",
		strings.NewReader("chart"), blob.PutOptions{}); err != nil {
		t.Fatalf("put attach: %v", err)
	}

	if err := hydrateAgentDirFromBlobs(ctx, st, bs, agentID, testLogger()); err != nil {
		t.Fatalf("hydrateAgentDirFromBlobs: %v", err)
	}

	bookPath := filepath.Join(agentDir(agentID), "books", "readme.md")
	if _, err := os.Stat(bookPath); !os.IsNotExist(err) {
		t.Fatalf("generic blob artifact was hydrated at %s; err=%v", bookPath, err)
	}
	attachPath := filepath.Join(agentDir(agentID), "attach", "m_1", "chart.png")
	if _, err := os.Stat(attachPath); !os.IsNotExist(err) {
		t.Fatalf("attach artifact was hydrated at %s; err=%v", attachPath, err)
	}
}

func TestHydrateAgentDirFromBlobsRemovesLegacyAttachDirOnly(t *testing.T) {
	st, bs := newHydrateBlobFixture(t)
	ctx := context.Background()
	agentID := "ag_hydrate_cleanup"
	legacyAttach := filepath.Join(agentDir(agentID), "attach", "m_old")
	if err := os.MkdirAll(legacyAttach, 0o755); err != nil {
		t.Fatalf("mkdir legacy attach: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyAttach, "old.png"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write legacy attach: %v", err)
	}
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("mkdir staging attach: %v", err)
	}
	stageFile := filepath.Join(stageDir, "new.png")
	if err := os.WriteFile(stageFile, []byte("new"), 0o644); err != nil {
		t.Fatalf("write staging attach: %v", err)
	}

	if err := hydrateAgentDirFromBlobs(ctx, st, bs, agentID, testLogger()); err != nil {
		t.Fatalf("hydrateAgentDirFromBlobs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(agentDir(agentID), "attach")); !os.IsNotExist(err) {
		t.Fatalf("legacy attach dir still exists or unexpected stat error: %v", err)
	}
	if got, err := os.ReadFile(stageFile); err != nil || string(got) != "new" {
		t.Fatalf(".kojo attach staging was changed: body=%q err=%v", string(got), err)
	}
}
