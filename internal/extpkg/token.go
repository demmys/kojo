package extpkg

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Extension tokens
//
// A package's service process talks to kojo as an ordinary HTTP client,
// so it needs a bearer token. Unlike agent tokens (internal/auth's
// TokenStore, hash-only at rest) the raw value is kept in state.json:
// kojo respawns supervised services on every boot and has to hand the
// process its token again, and a hash-only store makes that
// impossible without re-issuing on each restart — which would break
// any extension that caches its token. state.json is 0600 in the
// operator's config dir, the same directory that already holds
// credentials the agents can read, so this widens nothing.
//
// The token is minted at install time and rotated on demand
// (Manager.RotateToken); removing the package drops it.

// tokenBytes is the entropy per extension token. 32 bytes matches the
// agent token width.
const tokenBytes = 32

// tokenFilename is where a package finds its token when it was not
// handed one in the environment. It sits inside the package's data
// directory at 0600. Contributed MCP servers get only this path
// (KOJO_EXT_TOKEN_FILE), because their environment is assembled by the
// backend CLI from command-line config, where a secret would be
// readable by every process on the machine.
const tokenFilename = ".kojo-token"

func newExtensionToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate extension token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Identity is what a valid extension token resolves to. It mirrors
// auth.ExtensionIdentity without importing it: internal/auth must not
// depend on extpkg (the server wires the two together), and extpkg
// importing auth would close the cycle from the other side.
type Identity struct {
	ID         string
	Scopes     []string
	AgentScope []string
}

// ResolveToken maps a presented bearer token to the extension that owns
// it. A disabled package resolves to nothing: the operator's kill
// switch has to cut API access too, not just stop the process.
//
// The returned AgentScope lists only agents whose binding is enabled,
// recomputed per call, so revoking a binding takes effect immediately.
func (m *Manager) ResolveToken(token string) (Identity, bool) {
	if token == "" {
		return Identity{}, false
	}
	want := hashToken(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		if row.Token == "" || hashToken(row.Token) != want {
			continue
		}
		if !row.Enabled {
			return Identity{}, false
		}
		ident := Identity{
			ID:     row.ID,
			Scopes: append([]string(nil), row.GrantedScopes...),
		}
		for agentID, b := range row.Agents {
			if b.Enabled {
				ident.AgentScope = append(ident.AgentScope, agentID)
			}
		}
		return ident, true
	}
	return Identity{}, false
}

// Token returns the raw bearer token for an installed extension. The
// owner-facing API exposes it so an operator can run a package's
// service by hand during development.
func (m *Manager) Token(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return "", ErrNotFound
	}
	return row.Token, nil
}

// RotateToken mints a fresh token, invalidating the old one. Callers
// that supervise the package's service must restart it afterwards, or
// the running process keeps presenting a token that no longer resolves.
func (m *Manager) RotateToken(id string) (string, error) {
	tok, err := newExtensionToken()
	if err != nil {
		return "", err
	}
	if _, err := m.mutate(id, func(row *Installed) error {
		row.Token = tok
		return nil
	}); err != nil {
		return "", err
	}
	return tok, nil
}
