package store

import (
	"context"
	"errors"
	"testing"
)

func openExternalTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExternalAgentPairingLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openExternalTestStore(t)
	if _, err := s.CreateExternalAgent(ctx, "xa_1", "Hermes", "d", 0); err != nil {
		t.Fatal(err)
	}
	node := ExternalNodeIdentity{StableNodeID: "nS1", NodeName: "box", TailnetIPs: []string{"100.64.0.5"}}

	if _, err := s.RedeemExternalPairing(ctx, "nope", node, "tok"); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("unknown code: %v", err)
	}
	// Expired code is burned.
	if err := s.SetExternalPairing(ctx, "xa_1", "c0", NowMillis()-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemExternalPairing(ctx, "c0", node, "tok"); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("expired: %v", err)
	}
	if err := s.SetExternalPairing(ctx, "xa_1", "c1", NowMillis()+60_000); err != nil {
		t.Fatal(err)
	}
	r, err := s.RedeemExternalPairing(ctx, "c1", node, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	if r.StableNodeID != "nS1" || r.TokenHash != "tok1" || r.TokenGen != 1 || r.PairingHash != "" {
		t.Fatalf("redeemed row: %+v", r)
	}
	// Single use.
	if _, err := s.RedeemExternalPairing(ctx, "c1", node, "tok2"); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("reuse: %v", err)
	}
	got, err := s.LookupExternalAgentByToken(ctx, "tok1")
	if err != nil || got.ID != "xa_1" {
		t.Fatalf("lookup: %v %v", got, err)
	}

	// Another agent cannot bind the same node.
	if _, err := s.CreateExternalAgent(ctx, "xa_2", "Other", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExternalPairing(ctx, "xa_2", "c2", NowMillis()+60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemExternalPairing(ctx, "c2", node, "tok3"); !errors.Is(err, ErrExternalNodeBound) {
		t.Fatalf("double bind: %v", err)
	}

	// Revoke kills the token, bumps gen.
	r, err = s.RevokeExternalToken(ctx, "xa_1")
	if err != nil || r.TokenHash != "" || r.TokenGen != 2 {
		t.Fatalf("revoke: %+v %v", r, err)
	}
	if _, err := s.LookupExternalAgentByToken(ctx, "tok1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked lookup: %v", err)
	}

	// Denylist survives delete; release refused while bound.
	if err := s.DeleteExternalNodeDeny(ctx, "nS1"); !errors.Is(err, ErrExternalNodeBound) {
		t.Fatalf("release while bound: %v", err)
	}
	if err := s.TouchExternalAgent(ctx, "xa_1", ExternalNodeIdentity{StableNodeID: "nS1", TailnetIPs: []string{"fd7a::5"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDeleteExternalAgent(ctx, "xa_1"); err != nil {
		t.Fatal(err)
	}
	deny, err := s.ListExternalNodeDenylist(ctx)
	if err != nil || len(deny) != 1 || len(deny[0].TailnetIPs) != 2 {
		t.Fatalf("denylist after delete: %+v %v", deny, err)
	}
	if err := s.DeleteExternalNodeDeny(ctx, "nS1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Node free again after delete: xa_2 can bind it now.
	if _, err := s.RedeemExternalPairing(ctx, "c2", node, "tok3"); err != nil {
		t.Fatalf("rebind after delete: %v", err)
	}
}

func TestExternalCursorNeverGoesBack(t *testing.T) {
	ctx := context.Background()
	s := openExternalTestStore(t)
	_ = s.AdvanceExternalCursor(ctx, "xa_1", "gd_1", 5)
	_ = s.AdvanceExternalCursor(ctx, "xa_1", "gd_1", 3)
	c, err := s.GetExternalCursors(ctx, "xa_1")
	if err != nil || c["gd_1"] != 5 {
		t.Fatalf("cursor: %v %v", c, err)
	}
}
