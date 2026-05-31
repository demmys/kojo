package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

func newBlobCleanTestStore(t *testing.T) (string, *store.Store, *blob.Store) {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bs := blob.New(root,
		blob.WithRefs(blob.NewStoreRefs(st, "peer-test")),
		blob.WithHomePeer("peer-test"),
	)
	return root, st, bs
}

func putBlobForCleanTest(t *testing.T, bs *blob.Store, p, body string) string {
	t.Helper()
	if _, err := bs.Put(blob.ScopeGlobal, p, bytes.NewBufferString(body), blob.PutOptions{}); err != nil {
		t.Fatalf("blob.Put %s: %v", p, err)
	}
	return blob.BuildURI(blob.ScopeGlobal, p)
}

func ageBlobRefForCleanTest(t *testing.T, st *store.Store, uri string, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).UnixMilli()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE blob_refs SET created_at = ?, updated_at = ? WHERE uri = ?`,
		ts, ts, uri,
	); err != nil {
		t.Fatalf("age blob ref %s: %v", uri, err)
	}
}

func TestPlanBlobCleanupExpiresOldAttachmentsOnly(t *testing.T) {
	root, st, bs := newBlobCleanTestStore(t)
	oldAttach := putBlobForCleanTest(t, bs, "agents/ag_1/attach/m_old/chart.png", "old")
	missingOldAttach := putBlobForCleanTest(t, bs, "agents/ag_1/attach/m_missing/chart.png", "missing")
	freshAttach := putBlobForCleanTest(t, bs, "agents/ag_1/attach/m_fresh/chart.png", "fresh")
	oldAvatar := putBlobForCleanTest(t, bs, "agents/ag_1/avatar.png", "avatar")
	ageBlobRefForCleanTest(t, st, oldAttach, 10*24*time.Hour)
	ageBlobRefForCleanTest(t, st, missingOldAttach, 10*24*time.Hour)
	ageBlobRefForCleanTest(t, st, freshAttach, time.Hour)
	ageBlobRefForCleanTest(t, st, oldAvatar, 10*24*time.Hour)
	missingPath := filepath.Join(root, "global", "agents", "ag_1", "attach", "m_missing", "chart.png")
	if err := os.Remove(missingPath); err != nil {
		t.Fatalf("remove missing-body setup: %v", err)
	}

	plan, err := planBlobCleanup(context.Background(), st, root, 7)
	if err != nil {
		t.Fatalf("planBlobCleanup: %v", err)
	}
	gotExpired := map[string]bool{}
	for _, ref := range plan.ExpiredAttachments {
		gotExpired[ref.URI] = true
	}
	if len(gotExpired) != 2 || !gotExpired[oldAttach] || !gotExpired[missingOldAttach] {
		t.Fatalf("ExpiredAttachments = %+v, want %s and %s", plan.ExpiredAttachments, oldAttach, missingOldAttach)
	}
	if len(plan.GCRefs) != 0 {
		t.Fatalf("GCRefs = %+v, want none", plan.GCRefs)
	}
	if len(plan.OrphanFiles) != 0 {
		t.Fatalf("OrphanFiles = %+v, want none", plan.OrphanFiles)
	}

	if errs := applyBlobCleanPlan(context.Background(), plan, st); len(errs) > 0 {
		t.Fatalf("applyBlobCleanPlan: %v", errs)
	}

	oldPath := filepath.Join(root, "global", "agents", "ag_1", "attach", "m_old", "chart.png")
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired attachment body survived: %v", err)
	}
	if _, err := st.GetBlobRef(context.Background(), oldAttach); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired attachment ref survived: %v", err)
	}
	if _, err := st.GetBlobRef(context.Background(), missingOldAttach); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing expired attachment ref survived: %v", err)
	}
	for _, uri := range []string{freshAttach, oldAvatar} {
		if _, err := st.GetBlobRef(context.Background(), uri); err != nil {
			t.Fatalf("kept ref %s missing: %v", uri, err)
		}
	}
}
