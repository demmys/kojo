package peer

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// v2 of the peer-auth wire format dropped body hashing from the
// signature (see auth.go). The middleware no longer buffers the
// request body, so there is no AuthMaxBodyBytes constant — each
// handler enforces its own per-route body cap via http.MaxBytesReader
// just like a non-peer request would.

// AuthMiddleware is the HTTP middleware that resolves an
// Ed25519-signed peer request to an auth.Principal{Role:RolePeer}.
//
// Resolution order:
//
//  1. Required headers missing → pass-through with no principal
//     change (lets the chain fall back to the regular OwnerOnly /
//     Auth middleware for non-peer traffic).
//  2. Headers present but malformed (bad nonce length, unparseable
//     timestamp, signature header isn't base64) → 401 + JSON error.
//     This is intentionally stricter than (1): once a peer commits
//     to peer-auth headers, any malformed shape is a peer-side bug
//     and surfacing it lets the operator find the buggy client.
//  3. Headers well-formed but checks fail (timestamp out of skew
//     window, nonce replay, unknown device_id, signature
//     mismatch) → 401 + JSON error naming the failure.
//  4. Every check passes → ctx gets Principal{Role: RolePeer,
//     PeerID: device_id} and the chain proceeds.
//
// Order in the chain: this MUST run BEFORE OwnerOnlyMiddleware and
// AuthMiddleware so its principal isn't clobbered. The downstream
// middlewares respect a pre-existing non-Guest principal.
//
// Body handling: v2 dropped body-hash signing, so the middleware
// passes r.Body through unchanged to the handler. Per-route body
// limits live on each handler (e.g. http.MaxBytesReader in the
// upload / blob PUT paths).
type AuthMiddleware struct {
	store  *store.Store
	nonces *NonceCache
	// selfDeviceID, when non-empty, makes the middleware refuse a
	// request that claims to be FROM the local peer — a peer
	// shouldn't be signing requests back to itself, and a
	// signature replay from the local peer's outbound traffic
	// would be the most likely source. Empty disables the
	// self-loopback guard (test fixtures).
	selfDeviceID string

	// now is the clock the timestamp gate uses. Injectable for
	// tests; defaults to time.Now in production.
	now func() time.Time
}

// NewAuthMiddleware wires the deps. The kvstore is required (it
// holds peer_registry). nonces is required for replay protection;
// a nil pointer is replaced with a fresh default cache so a
// misconfigured caller doesn't silently disable replay defence.
// selfDeviceID is the local peer's identity — empty disables the
// self-loopback guard.
func NewAuthMiddleware(st *store.Store, nonces *NonceCache, selfDeviceID string) *AuthMiddleware {
	if nonces == nil {
		nonces = NewNonceCache(AuthMaxClockSkew)
	}
	return &AuthMiddleware{
		store:        st,
		nonces:       nonces,
		selfDeviceID: selfDeviceID,
		now:          time.Now,
	}
}

// Wrap returns a handler that runs the auth check before
// delegating to next. The four required headers' presence is
// what gates the check — a request without any of them just
// falls through.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Step 1: header sniff. ANY of the five missing → not a
		// peer-auth request; pass through. We treat the audience
		// header as part of the sniff set so an older peer that
		// doesn't include it can't downgrade-attack the middleware
		// into accepting an audience-less signature.
		dev := r.Header.Get(AuthHeaderID)
		aud := r.Header.Get(AuthHeaderAud)
		tsHdr := r.Header.Get(AuthHeaderTS)
		nonce := r.Header.Get(AuthHeaderNonce)
		sig := r.Header.Get(AuthHeaderSig)
		if dev == "" && aud == "" && tsHdr == "" && nonce == "" && sig == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Partial presence: any of the five supplied alone is a
		// peer-side bug. Surface as 400.
		if dev == "" || aud == "" || tsHdr == "" || nonce == "" || sig == "" {
			writePeerAuthError(w, http.StatusBadRequest,
				"some peer-auth headers present but not all five")
			return
		}
		// Audience binding: the request MUST name this peer as
		// its intended receiver. Without this check, a valid
		// signature captured from peer A→B's traffic could be
		// replayed against peer C — the verifier would accept
		// every byte of the canonical payload because A's pub
		// key validates regardless of who receives.
		if m.selfDeviceID == "" {
			// Test fixtures only — production wiring always
			// populates selfDeviceID via NewAuthMiddleware.
		} else if aud != m.selfDeviceID {
			writePeerAuthError(w, http.StatusUnauthorized,
				"audience does not match local peer")
			return
		}
		// Self-loopback: refuse a request claiming to be from
		// this very peer. The signing key never leaves the peer's
		// kek-encrypted slot, so this would have to be a replayed
		// signature — and the replay protection below would catch
		// it, but rejecting up-front is a clearer error.
		if m.selfDeviceID != "" && dev == m.selfDeviceID {
			writePeerAuthError(w, http.StatusUnauthorized,
				"refusing self-signed request (device_id matches local peer)")
			return
		}
		// Step 2: header shape validation.
		ts, err := strconv.ParseInt(tsHdr, 10, 64)
		if err != nil {
			writePeerAuthError(w, http.StatusBadRequest,
				"timestamp header is not int64: "+err.Error())
			return
		}
		if err := CheckNonce(nonce); err != nil {
			writePeerAuthError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := base64.StdEncoding.DecodeString(sig); err != nil {
			writePeerAuthError(w, http.StatusBadRequest,
				"signature header is not base64: "+err.Error())
			return
		}
		// Step 3: timestamp + nonce gates. These run before the
		// DB lookup so a replay flood can't pin the store.
		if err := CheckTimestamp(ts, m.now().UnixMilli()); err != nil {
			writePeerAuthError(w, http.StatusUnauthorized, err.Error())
			return
		}
		// Probe (not Commit) so a bogus signature presented before
		// the genuine signer can't consume the victim's nonce. The
		// real Commit happens after Verify succeeds below.
		if m.nonces.Probe(dev, nonce) {
			writePeerAuthError(w, http.StatusUnauthorized,
				ErrAuthReplay.Error())
			return
		}
		// Step 4: peer_registry lookup.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rec, err := m.store.GetPeer(ctx, dev)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writePeerAuthError(w, http.StatusUnauthorized,
					ErrAuthUnknownPeer.Error())
				return
			}
			writePeerAuthError(w, http.StatusInternalServerError,
				"peer registry lookup: "+err.Error())
			return
		}
		pub, err := base64.StdEncoding.DecodeString(rec.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			writePeerAuthError(w, http.StatusInternalServerError,
				"peer_registry public_key shape invalid")
			return
		}
		// Step 5: signature verification. v2 doesn't cover the
		// body, so r.Body is passed through to the handler
		// untouched — no buffering, no per-middleware size cap.
		in := SigningInput{
			DeviceID: dev,
			Audience: aud,
			TS:       ts,
			Nonce:    nonce,
			Method:   r.Method,
			// EscapedPath preserves percent-encoded segments
			// (%2F, etc.) so an attacker can't smuggle a
			// different-but-decode-equivalent path past the
			// verifier. RawQuery is already raw-encoded.
			Path:     r.URL.EscapedPath(),
			RawQuery: r.URL.RawQuery,
		}
		if err := Verify(ed25519.PublicKey(pub), sig, in); err != nil {
			writePeerAuthError(w, http.StatusUnauthorized,
				ErrAuthBadSignature.Error())
			return
		}
		// Signature verified — commit the nonce. A concurrent
		// Commit winning here means an authenticated replay
		// slipped past Probe (vanishingly unlikely at 256-bit
		// random nonces; possible if a buggy peer reuses one).
		// Refuse the duplicate. We pass the request's ts so the
		// cache retains the nonce across the full timestamp
		// re-admission window (sender clocks may lead ours).
		if dup := m.nonces.Commit(dev, nonce, ts); dup {
			writePeerAuthError(w, http.StatusUnauthorized,
				ErrAuthReplay.Error())
			return
		}
		// Successful peer auth IS a liveness signal: the remote
		// peer must have been reachable enough to send us a
		// signed request. Touch the peer_registry row so
		// operator-visible `peer-list` / `GET /api/v1/peers`
		// reflect the connection without waiting on the next
		// heartbeat (which goes peer→Hub, not peer→peer; from
		// our POV the only liveness signal IS this auth event).
		// Best-effort — a failed touch doesn't reject the
		// request; OfflineSweeper will catch stale rows on its
		// next tick.
		touchCtx, touchCancel := context.WithTimeout(r.Context(), 2*time.Second)
		_ = m.store.TouchPeer(touchCtx, dev, store.PeerStatusOnline, time.Now().UnixMilli())
		touchCancel()
		// All checks passed. Stamp the principal and forward.
		// PeerTrusted is read from the peer_registry row so the
		// policy layer can admit /sessions, /ws, /files, /git
		// only when the operator explicitly marked this peer as
		// trusted. Untrusted RolePeer principals stay scoped to
		// the minimal inter-peer endpoints.
		next.ServeHTTP(w, r.WithContext(
			auth.WithPrincipal(r.Context(), auth.Principal{
				Role:        auth.RolePeer,
				PeerID:      dev,
				PeerTrusted: rec.Trusted,
			}),
		))
	})
}

// writePeerAuthError emits a JSON error body the test suite + the
// peer-side client can parse uniformly. status is the HTTP code;
// msg goes into the JSON.
func writePeerAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Hand-roll the JSON to dodge an import cycle with the
	// server package's writeError helper. Keep the shape
	// compatible with server-side writeError so a client written
	// against either surface decodes the same fields.
	fmt.Fprintf(w, `{"error":{"code":"peer_auth","message":%q}}`, msg)
}

// SignRequest helps a peer client attach the peer-auth headers to
// an outbound *http.Request. audienceDeviceID names the receiver
// peer; the receiver's middleware refuses a request whose
// audience doesn't match its own DeviceID, so cross-peer replay
// of a captured signature fails.
//
// Used by the cross-subscribe client + the device-switch
// handoff client.
func SignRequest(req *http.Request, deviceID string, priv ed25519.PrivateKey, nonce, audienceDeviceID string) error {
	if req == nil {
		return errors.New("peer.SignRequest: nil request")
	}
	if deviceID == "" {
		return errors.New("peer.SignRequest: device_id required")
	}
	if audienceDeviceID == "" {
		return errors.New("peer.SignRequest: audience device_id required")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("peer.SignRequest: bad private key length")
	}
	if nonce == "" {
		return errors.New("peer.SignRequest: nonce required")
	}
	// v2 doesn't cover the body; nothing to read here.
	ts := time.Now().UnixMilli()
	in := SigningInput{
		DeviceID: deviceID,
		Audience: audienceDeviceID,
		TS:       ts,
		Nonce:    nonce,
		Method:   req.Method,
		Path:     req.URL.EscapedPath(),
		RawQuery: req.URL.RawQuery,
	}
	sig := Sign(priv, in)
	req.Header.Set(AuthHeaderID, deviceID)
	req.Header.Set(AuthHeaderAud, audienceDeviceID)
	req.Header.Set(AuthHeaderTS, strconv.FormatInt(ts, 10))
	req.Header.Set(AuthHeaderNonce, nonce)
	req.Header.Set(AuthHeaderSig, sig)
	return nil
}
