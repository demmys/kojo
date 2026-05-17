package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// wireAttachForwarder installs the kojo-attach hub-push callback on
// the agent manager. Extracted out of main.go so the imports stay
// scoped (peer / blob / io are only needed for this wiring) and so
// the closure's hub-discovery heuristic can be reasoned about in
// isolation.
//
// The closure runs on every attachment so a peer_registry edit
// (operator promotes a different hub via `--peer-trust`) is picked
// up on the next forward without a daemon restart. ListPeers is
// already last_seen-sorted DESC, so if multiple trusted peers are
// paired the most-recently-active one wins.
func wireAttachForwarder(mgr *agent.Manager, st *store.Store, self *peer.Identity, logger *slog.Logger) {
	pushClient := peer.NewPushClient(self, nil, logger)
	selfID := self.DeviceID
	mgr.SetAttachmentForwarder(func(
		ctx context.Context,
		scope blob.Scope,
		path string,
		sha256Hex string,
		body io.Reader,
		size int64,
	) error {
		peers, err := st.ListPeers(ctx, store.ListPeersOptions{})
		if err != nil {
			return fmt.Errorf("attach forward: list peers: %w", err)
		}
		var hub *store.PeerRecord
		for _, p := range peers {
			if p.DeviceID == selfID || !p.Trusted || p.URL == "" {
				continue
			}
			if hub == nil {
				hub = p
				continue
			}
			// Multiple trusted candidates. Stick with the first
			// (most-recently-active per ListPeers' DESC sort)
			// and log so the operator can disambiguate by
			// untrusting the extras.
			logger.Warn("attach forward: multiple trusted peer candidates, picking first",
				"selected", hub.DeviceID, "skipped", p.DeviceID)
			break
		}
		if hub == nil {
			return errors.New("attach forward: no trusted hub peer in registry")
		}
		return pushClient.PushOne(ctx,
			peer.PushTarget{DeviceID: hub.DeviceID, Address: hub.URL},
			scope, path, sha256Hex, body, size)
	})
}
