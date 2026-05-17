package peer

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// docs/multi-device-storage.md §3.7 / §3.10 require peer-to-peer
// HTTP for two flows v1 ships:
//
//   1. Cross-peer status subscribe (§3.10's "両方向 heartbeat"
//      narrowed to near-realtime via WS push of peer_registry
//      changes).
//   2. Device-switch blob handoff (§3.7 step 4 — the target peer
//      pulls the blob body from the source peer).
//
// mTLS would require a CA bootstrap path that v1 doesn't have.
// Instead we authenticate peer-to-peer with an Ed25519
// challenge-response: every peer already carries an Ed25519
// identity (see Identity in identity.go), and every peer's public
// key is replicated cluster-wide in peer_registry. A request
// signed by peer A's private key can be verified by any other
// peer that has A's peer_registry row.
//
// Wire format: five headers per request.
//
//	X-Kojo-Peer-ID    : <device_id> (UUID v4 hex, no dashes per
//	                    peer_registry).
//	X-Kojo-Peer-Aud   : <device_id> of the intended receiver.
//	                    Binds the signature to one target peer so a
//	                    header set captured from A→B can't be
//	                    replayed against A→C.
//	X-Kojo-Peer-TS    : <unix millis>. Receiver rejects values
//	                    outside ±AuthMaxClockSkew so a replay
//	                    captured days ago can't be re-played.
//	X-Kojo-Peer-Nonce : 32 random bytes, base64. Receiver caches
//	                    seen nonces for AuthMaxClockSkew and
//	                    refuses duplicates.
//	X-Kojo-Peer-Sig   : base64 Ed25519 signature over the
//	                    canonical payload defined by SigningInput.
//
// Canonical signing payload (v2 — no body hash):
//
//	"kojo-peer-auth-v2\n" ||
//	device_id || "\n" ||
//	audience  || "\n" ||
//	ts        || "\n" ||
//	nonce     || "\n" ||
//	method    || "\n" ||
//	path      || "\n" ||
//	raw_query
//
// The domain prefix "kojo-peer-auth-v2\n" prevents an Ed25519
// signature lifted from any other context (or the older v1
// body-hashing variant) from validating here.
//
// Body integrity is intentionally NOT covered by the signature. The
// previous v1 wire format hashed the full body so a man-in-the-middle
// couldn't substitute a different POST payload — but that protection
// forced every signed request to buffer its full body for hashing,
// which capped cross-peer uploads at AuthMaxBodyBytes (16 MiB). kojo
// rides on Tailscale-only transport with mutually-authenticated TLS
// at the tsnet layer, so the threat model treats the wire as
// confidential and non-malleable. Dropping body hashing lets large
// uploads stream straight through. Endpoints that genuinely need
// content-binding (e.g. blob PUT) can opt into the per-handler
// X-Kojo-Expected-SHA256 header, which gives the same guarantee
// without bouncing through the auth middleware.

const (
	// AuthHeaderID is the device_id header.
	AuthHeaderID = "X-Kojo-Peer-ID"
	// AuthHeaderTS is the unix-millis timestamp header.
	AuthHeaderTS = "X-Kojo-Peer-TS"
	// AuthHeaderNonce is the random-nonce header.
	AuthHeaderNonce = "X-Kojo-Peer-Nonce"
	// AuthHeaderSig is the base64 Ed25519 signature.
	AuthHeaderSig = "X-Kojo-Peer-Sig"
	// AuthHeaderAud names the intended receiver's device_id. The
	// middleware refuses a request whose Aud doesn't match the
	// local peer's identity — without this, a valid signature
	// captured from peer A's traffic to peer B could be replayed
	// against peer C on the same path / body / timestamp window
	// because every receiver's verifier accepts the same
	// canonical payload. Audience binding closes that.
	AuthHeaderAud = "X-Kojo-Peer-Aud"

	// AuthDomainPrefix is the canonical first line of the signing
	// payload — domain separation across protocol versions /
	// kojo subsystems. v2 dropped the body-hash field; bumping the
	// prefix means a v1 signature deliberately won't validate
	// against a v2 verifier (and vice-versa), so a mixed-version
	// peer fleet fails loud rather than appearing to authenticate.
	AuthDomainPrefix = "kojo-peer-auth-v2\n"

	// AuthMaxClockSkew bounds both the timestamp window (a request
	// older or newer than this is refused) and the nonce cache
	// retention. 5 min is generous enough for clock drift across
	// Tailscale peers and tight enough that a captured request
	// can't be replayed indefinitely.
	AuthMaxClockSkew = 5 * time.Minute

	// AuthNonceLen is the required nonce length in bytes (before
	// base64). 32 bytes (256 bits) makes accidental collision
	// astronomically unlikely within a single AuthMaxClockSkew
	// window.
	AuthNonceLen = 32
)

// ErrAuthMalformedHeader is returned when a required header is
// missing or shaped wrong. The HTTP layer maps this to 400.
var ErrAuthMalformedHeader = errors.New("peer: malformed auth header")

// ErrAuthStaleTimestamp is returned when the request's timestamp
// is outside ±AuthMaxClockSkew. The HTTP layer maps this to 401.
var ErrAuthStaleTimestamp = errors.New("peer: timestamp outside skew window")

// ErrAuthReplay is returned when the (device_id, nonce) pair has
// already been seen within the skew window. Maps to 401.
var ErrAuthReplay = errors.New("peer: replayed nonce")

// ErrAuthUnknownPeer is returned when device_id has no
// peer_registry row. Maps to 401.
var ErrAuthUnknownPeer = errors.New("peer: unknown device_id")

// ErrAuthBadSignature is returned when the Ed25519 signature
// fails to verify. Maps to 401.
var ErrAuthBadSignature = errors.New("peer: signature verification failed")

// SigningInput holds the request shape the canonical payload is
// computed over. Method is the HTTP method (uppercase). Path is
// the URL-encoded request path. RawQuery is the URL's raw query
// string (without the '?'); an empty string is signed as the
// empty byte slice — distinct from "?" being present but empty,
// which net/url normalises identically.
//
// Including RawQuery in the signature was added in the round-1
// review fix: without it, a peer route like
// /api/v1/peers/blobs/X?since=N could have N mutated by an
// attacker without breaking the signature. We sign the full
// reconstructed request URI now.
//
// Body bytes are NOT part of the signature in v2 — see the package
// doc-comment in auth.go for the rationale and the per-handler
// X-Kojo-Expected-SHA256 escape hatch for endpoints that need
// content binding.
type SigningInput struct {
	DeviceID string
	// Audience is the device_id of the intended receiver. The
	// verifier refuses a request whose Audience doesn't match
	// the local peer's identity, closing cross-peer signature
	// replay (a header captured from A→B's traffic can't be
	// re-sent to C). Empty Audience signs as the literal empty
	// string — the verifier admits empty audience only when the
	// local peer's self-loopback guard is disabled (test
	// fixtures); production wiring always populates Audience.
	Audience string
	TS       int64
	Nonce    string // base64 (URL-safe NOT required; std encoding)
	Method   string
	Path     string
	RawQuery string
}

// CanonicalPayload returns the bytes a signer should sign /
// verifier should check. Reusing one helper across both sides
// keeps the encoding from accidentally diverging.
func (in SigningInput) CanonicalPayload() []byte {
	var b strings.Builder
	b.WriteString(AuthDomainPrefix)
	b.WriteString(in.DeviceID)
	b.WriteByte('\n')
	b.WriteString(in.Audience)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%d", in.TS)
	b.WriteByte('\n')
	b.WriteString(in.Nonce)
	b.WriteByte('\n')
	b.WriteString(strings.ToUpper(in.Method))
	b.WriteByte('\n')
	b.WriteString(in.Path)
	b.WriteByte('\n')
	b.WriteString(in.RawQuery)
	return []byte(b.String())
}

// Sign produces the base64 signature a sender attaches to
// AuthHeaderSig. The caller fills SigningInput with the request
// metadata + body bytes.
func Sign(priv ed25519.PrivateKey, in SigningInput) string {
	sig := ed25519.Sign(priv, in.CanonicalPayload())
	return base64.StdEncoding.EncodeToString(sig)
}

// Verify checks the signature against the canonical payload using
// the given public key. Returns nil on success, ErrAuthBadSignature
// on failure. The caller is expected to have already done
// timestamp / nonce / public-key lookup; Verify only owns the
// cryptographic check.
func Verify(pub ed25519.PublicKey, sig string, in SigningInput) error {
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: decode sig: %v", ErrAuthBadSignature, err)
	}
	if !ed25519.Verify(pub, in.CanonicalPayload(), raw) {
		return ErrAuthBadSignature
	}
	return nil
}

// CheckTimestamp returns ErrAuthStaleTimestamp if ts is outside
// ±AuthMaxClockSkew from now. nowMs is injectable for tests; pass
// store.NowMillis() in production.
//
// Overflow-safe: compares ts against the window boundaries
// directly rather than computing a signed delta first. An
// attacker-supplied ts near math.MinInt64 / math.MaxInt64 would
// otherwise overflow the subtraction and the abs flip, producing
// a small positive "delta" that wrongly passes the window check.
func CheckTimestamp(ts, nowMs int64) error {
	skew := AuthMaxClockSkew.Milliseconds()
	lo := nowMs - skew
	hi := nowMs + skew
	if ts < lo || ts > hi {
		return fmt.Errorf("%w: ts=%d not in [%d,%d]",
			ErrAuthStaleTimestamp, ts, lo, hi)
	}
	return nil
}

// CheckNonce returns ErrAuthMalformedHeader if the nonce isn't a
// well-formed 32-byte base64 value. Replay detection (cache
// lookup) lives in NonceCache.See below.
func CheckNonce(nonce string) error {
	raw, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return fmt.Errorf("%w: nonce decode: %v", ErrAuthMalformedHeader, err)
	}
	if len(raw) != AuthNonceLen {
		return fmt.Errorf("%w: nonce length %d != %d",
			ErrAuthMalformedHeader, len(raw), AuthNonceLen)
	}
	return nil
}
