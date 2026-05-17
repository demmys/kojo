package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

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
// up on the next forward without a daemon restart.
//
// Hub selection: trust bit is intentionally NOT a filter — when
// peer-onboarding-plan auto-discovery runs, it stamps the hub row
// with trusted=true, but a peer paired manually via `--peer-add`
// (without `--peer-add-trusted`) would otherwise be invisible to
// this scan and the agent's attachments would silently never reach
// the operator. We accept any paired peer with a dialable URL and
// rely on Hub-side handler guards (regex-locked path, mandatory
// X-Kojo-Expected-SHA256, signer == home_peer cross-overwrite
// refusal) to keep the surface narrow.
//
// When the registry holds multiple candidate peers, we walk all of
// them in last_seen DESC order, returning on the first success and
// surfacing the per-target error from the last attempt when none
// reply. Combined with the per-attempt retry in pushWithRetry,
// this means a transient hub outage (TLS handshake failure on the
// first dial, brief listener restart on hub) no longer drops the
// attachment.
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
		// Candidate set: every non-self peer with a dialable URL.
		// last_seen DESC ordering means the most-recently-active
		// hub (typical single-hub deploy) is tried first.
		candidates := make([]*store.PeerRecord, 0, len(peers))
		for _, p := range peers {
			if p.DeviceID == selfID || p.URL == "" {
				continue
			}
			candidates = append(candidates, p)
		}
		if len(candidates) == 0 {
			return errors.New("attach forward: no candidate hub peers in registry (need at least one paired peer with a URL)")
		}

		// body is a fresh io.Reader the caller opened just for this
		// invocation; if multiple candidates need to be tried, the
		// per-candidate retry inside pushWithRetry handles its own
		// Seek-to-start. Reset once up front so the very first
		// attempt is also from offset 0.
		seeker, _ := body.(io.Seeker)

		var lastErr error
		for i, hub := range candidates {
			if seeker != nil && i > 0 {
				if _, err := seeker.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("attach forward: seek 0 before candidate %s: %w", hub.DeviceID, err)
				}
			}
			err := pushWithRetry(ctx, pushClient, hub, scope, path, sha256Hex, body, size, seeker, logger)
			if err == nil {
				if len(candidates) > 1 {
					logger.Info("attach forward: hub push succeeded",
						"hub", hub.DeviceID, "url", hub.URL,
						"candidates_total", len(candidates), "candidate_index", i)
				}
				return nil
			}
			logger.Warn("attach forward: candidate hub failed; trying next",
				"hub", hub.DeviceID, "url", hub.URL,
				"trusted", hub.Trusted, "err", err)
			lastErr = fmt.Errorf("%s (%s): %w", hub.DeviceID, hub.URL, err)
		}
		return fmt.Errorf("attach forward: all %d candidate(s) failed; last: %w", len(candidates), lastErr)
	})
}

// pushWithRetry tries a single hub up to attachForwardMaxAttempts
// times with exponential backoff. Body is rewound between attempts
// via seeker when available; without a seeker we can only try once
// (the io.Reader has been consumed).
func pushWithRetry(
	ctx context.Context,
	pc *peer.PushClient,
	hub *store.PeerRecord,
	scope blob.Scope,
	path string,
	sha256Hex string,
	body io.Reader,
	size int64,
	seeker io.Seeker,
	logger *slog.Logger,
) error {
	maxAttempts := attachForwardMaxAttempts
	if seeker == nil {
		// Non-seekable body — one shot only.
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("seek 0 before retry %d: %w", attempt, err)
			}
			// Exponential backoff with a cap; respect ctx.
			delay := attachForwardBackoffBase << (attempt - 2)
			if delay > attachForwardBackoffMax {
				delay = attachForwardBackoffMax
			}
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		}
		err := pc.PushOne(ctx,
			peer.PushTarget{DeviceID: hub.DeviceID, Address: hub.URL},
			scope, path, sha256Hex, body, size)
		if err == nil {
			if attempt > 1 {
				logger.Info("attach forward: hub push succeeded after retry",
					"hub", hub.DeviceID, "attempt", attempt)
			}
			return nil
		}
		lastErr = err
		logger.Warn("attach forward: hub push attempt failed",
			"hub", hub.DeviceID, "url", hub.URL,
			"attempt", attempt, "max", maxAttempts, "err", err)
	}
	return lastErr
}

const (
	// attachForwardMaxAttempts caps the per-hub retry count. 3 gives
	// us ~600 ms total backoff (200 + 400 ms between attempts) which
	// covers a brief listener restart on hub without stalling the
	// chat goroutine.
	attachForwardMaxAttempts = 3
	attachForwardBackoffBase = 200 * time.Millisecond
	attachForwardBackoffMax  = 2 * time.Second
)
