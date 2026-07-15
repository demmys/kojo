package peer

import (
	"errors"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// PeerNameMaxBytes is the upper bound on peer_registry.name. 255
// is the typical hostname limit; the FQDN + port form
// `<host>.<tailnet>.ts.net:8080` fits comfortably.
const PeerNameMaxBytes = 255

// ValidateDeviceID enforces the canonical 8-4-4-4-12 lowercase
// UUID form. uuid.Parse on its own is too permissive (accepts
// URN form, braced form, raw bytes, uppercase) — letting any of
// those through would let the same logical UUID land twice in
// peer_registry under different keys and bypass self-detection
// by submitting an alternate spelling of the local device id.
// Empty input is a distinct error so callers can produce clearer
// messages ("required" vs "invalid format").
//
// Shared by the HTTP handler (internal/server/peer_handlers.go)
// and the CLI (cmd/kojo/peer_cmd.go) so the same shape gate
// applies regardless of entry point.
func ValidateDeviceID(id string) error {
	if id == "" {
		return errors.New("deviceId is required")
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return errors.New("deviceId must be a UUID")
	}
	if parsed.String() != id {
		return errors.New("deviceId must be canonical lowercase 8-4-4-4-12 UUID form")
	}
	return nil
}

// PeerVersionHeader carries the dialing peer's binary version string
// on the /api/v1/peers/events WS upgrade so the receiving peer can
// stamp peer_registry.version. Informational only — it plays no part
// in auth or the auto-update decision (which reads hub-info, not
// this header).
const PeerVersionHeader = "X-Kojo-Peer-Version"

// PeerVersionMaxBytes bounds a reported version string. git-describe
// output ("v0.119.1-3-g07d4e24c-dirty") is well under this; anything
// longer is garbage or hostile.
const PeerVersionMaxBytes = 64

// SanitizePeerVersion returns the version string a peer reported if
// it is storable, or "" when it should be ignored. Printable ASCII
// only (0x21–0x7E) — git-describe output never leaves that range,
// and rejecting the rest kills control chars AND Unicode formatting
// tricks (bidi overrides, zero-width joiners) that could spoof the
// rendered value in a UI. Hard length cap on top. Leading/trailing
// whitespace is trimmed rather than rejected — proxies love to pad
// header values.
func SanitizePeerVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > PeerVersionMaxBytes {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// ValidateName checks the human-readable peer name. Trimmed
// length > 0 and ≤ PeerNameMaxBytes; all Unicode control
// characters rejected so a UI rendering the value can't be
// tricked into ANSI escape / null / DEL / TAB injection.
// unicode.IsControl covers the C0 range (NUL, TAB, LF, CR,
// ESC), DEL (U+007F), and the C1 range (U+0080..U+009F).
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if len(name) > PeerNameMaxBytes {
		return errors.New("name exceeds maximum length")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name contains control characters")
		}
	}
	return nil
}
