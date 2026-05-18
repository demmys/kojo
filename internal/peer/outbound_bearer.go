package peer

import (
	"context"
	"errors"
	"net/http"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrNoOutboundBearer is returned by LoadOutboundBearer when the kv has no
// row for the requested peer. Callers map this to "fall back to the legacy
// signing path" during the dual-stack window (docs/peer-simplify-plan.md);
// after step 9 deletes signing, the same condition becomes a hard 401 on
// the receiver side.
var ErrNoOutboundBearer = errors.New("peer: no outbound Bearer for target")

// LoadOutboundBearer reads the raw Bearer this kojo uses when calling
// target peer `peerDeviceID`. Returns ErrNoOutboundBearer for missing rows
// so the caller can distinguish "not paired with Bearer yet" from generic
// kv failure.
//
// Single source of truth for the OutBearerNS namespace: every outbound
// call site that needs Authorization should route through this helper
// rather than reach into kv directly.
func LoadOutboundBearer(ctx context.Context, st *store.Store, peerDeviceID string) (string, error) {
	if st == nil {
		return "", errors.New("peer.LoadOutboundBearer: nil store")
	}
	if peerDeviceID == "" {
		return "", errors.New("peer.LoadOutboundBearer: peer device_id required")
	}
	rec, err := st.GetKV(ctx, OutBearerNS, peerDeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrNoOutboundBearer
		}
		return "", err
	}
	if rec.Value == "" {
		return "", ErrNoOutboundBearer
	}
	return rec.Value, nil
}

// AttachOutboundBearer stamps `Authorization: Bearer <token>` on req,
// loading the raw token from kv on the fly. Returns ErrNoOutboundBearer
// when no row exists so the caller can decide whether to fall back to the
// legacy Ed25519 signing path or fail closed.
//
// The helper does NOT clear any existing Authorization header — callers
// that mix peer Bearer with other auth flows on the same client are
// presumed to know what they're doing. In practice every caller in
// internal/server / internal/peer builds a fresh *http.Request, so this
// detail rarely matters.
func AttachOutboundBearer(ctx context.Context, st *store.Store, req *http.Request, peerDeviceID string) error {
	if req == nil {
		return errors.New("peer.AttachOutboundBearer: nil request")
	}
	raw, err := LoadOutboundBearer(ctx, st, peerDeviceID)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	return nil
}

// AuthorizeOutbound is the single migration entry point used by the
// SignRequest callers in internal/server / internal/peer during the
// docs/peer-simplify-plan.md step-5 dual-stack window. The function:
//
//  1. Tries the Bearer path via AttachOutboundBearer.
//  2. On ErrNoOutboundBearer (no Bearer paired for this peer yet),
//     OR when the store handle is nil (test fixtures, daemon
//     bootstrap before kvstore is wired), falls back to SignRequest
//     with the supplied identity material.
//  3. Surfaces any other error verbatim — these signal a genuine
//     kv failure (transient SQLite BUSY, schema drift), not the
//     steady-state "no Bearer minted yet".
//
// Once step 9 removes SignRequest the fallback branch goes away and
// the function fails closed when no Bearer is present.
func AuthorizeOutbound(ctx context.Context, st *store.Store, req *http.Request, selfIdent *Identity, peerDeviceID, nonce string) error {
	if st != nil {
		if err := AttachOutboundBearer(ctx, st, req, peerDeviceID); err == nil {
			return nil
		} else if !errors.Is(err, ErrNoOutboundBearer) {
			return err
		}
	}
	if selfIdent == nil {
		return errors.New("peer.AuthorizeOutbound: nil identity for signing fallback")
	}
	return SignRequest(req, selfIdent.DeviceID, selfIdent.PrivateKey, nonce, peerDeviceID)
}
