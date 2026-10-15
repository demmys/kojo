package extpkg

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Extension secrets
//
// A settings schema marks a string property as a secret by giving it
// `"format": "password"`. Such a value never lands in state.json, in an
// API response, or in KOJO_EXT_CONFIG — the last matters most, because
// a contributed MCP server's environment travels on the backend CLI's
// command line, where every process on the machine can read it.
//
// Instead the value goes to the encrypted credential store, and the
// registry keeps only the *names* of the secrets that are set, so the
// settings form can show "already saved" without ever seeing the value.
// A package gets its secrets two ways:
//
//   - a supervised service finds them in KOJO_EXT_SECRETS (a JSON
//     object), next to its token — kojo spawns that process itself, so
//     the environment is private;
//   - anything else (notably an MCP server) asks for them over HTTP
//     with its own token: GET /api/v1/extension/secrets.

// secretProvider is the credential-store provider column every
// extension secret is filed under. The source ID is the package ID.
const secretProvider = "extension"

// maxSecretLen bounds one secret value. Generous for API keys and bot
// tokens; small enough that the store cannot be used as a blob dump.
const maxSecretLen = 16 << 10

// SecretStore is the slice of the credential store extension secrets
// need. agent.CredentialStore implements it; the server wires it in.
type SecretStore interface {
	GetToken(provider, agentID, sourceID, key string) (string, error)
	SetToken(provider, agentID, sourceID, key, value string, expiresAt time.Time) error
	DeleteToken(provider, agentID, sourceID, key string) error
	DeleteTokensBySource(provider, agentID, sourceID string) error
}

// SetSecretStore installs the encrypted store secrets are kept in.
// Without one, saving a secret fails loudly rather than falling back
// to plaintext in the registry.
func (m *Manager) SetSecretStore(store SecretStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets = store
	if m.migrateSecretsLocked() {
		if err := m.save(); err != nil {
			m.logger.Warn("saving the registry after secret migration failed", "error", err)
		}
	}
}

// secretKeys lists the settings properties the package's schema marks
// as secrets. The schema is re-read each time: it is a small file, and
// an update may have swapped it since the last call. A package without
// a schema, or with an unreadable one, has no secrets.
func (m *Manager) secretKeys(row *Installed) []string {
	st := row.Manifest.Contributes.Settings
	if st == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(pkgPath(m.root, row.ID), filepath.FromSlash(st.Schema)))
	if err != nil {
		return nil
	}
	return secretKeysFromSchema(data)
}

func secretKeysFromSchema(data []byte) []string {
	var schema struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Format string `json:"format"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil
	}
	var keys []string
	for k, p := range schema.Properties {
		if p.Type == "string" && p.Format == "password" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// splitSecretsLocked pulls secret values out of a submitted settings
// object and files them in the credential store. cfg is edited in
// place — on return it holds no secret keys — and the result is the
// updated list of secret names that are set for (row, agentID).
//
// Per key the submitted value means:
//
//	absent or ""  keep whatever is stored (the form never receives the
//	              value back, so an untouched field arrives empty)
//	null          forget the stored value
//	"text"        store it
//
// The write is all or nothing. Every value is checked before the store
// is touched, and the returned undo puts the store back as it was: it
// has already run when an error is returned, and the caller runs it if
// the registry row that names these secrets then fails to save.
//
// Caller holds m.mu.
func (m *Manager) splitSecretsLocked(row *Installed, agentID string, cfg map[string]any, prev []string) (names []string, undo func(), err error) {
	undo = func() {}
	keys := m.secretKeys(row)
	if len(keys) == 0 {
		return prev, undo, nil
	}
	set := map[string]bool{}
	for _, k := range prev {
		set[k] = true
	}

	// Pass 1: validate, and work out what changes.
	type change struct {
		key    string
		value  string
		remove bool
		// What the change overwrites; see the read pass below.
		old      string
		hadOld   bool
		oldKnown bool
	}
	var changes []change
	for _, key := range keys {
		v, present := cfg[key]
		if !present {
			continue
		}
		delete(cfg, key)
		switch val := v.(type) {
		case nil:
			if set[key] {
				changes = append(changes, change{key: key, remove: true})
			}
		case string:
			if val == "" {
				continue
			}
			if len(val) > maxSecretLen {
				return nil, undo, fmt.Errorf("secret %s is too long", key)
			}
			changes = append(changes, change{key: key, value: val})
		default:
			return nil, undo, fmt.Errorf("secret %s must be a string", key)
		}
	}
	if len(changes) > 0 && m.secrets == nil {
		return nil, undo, fmt.Errorf("secret storage is unavailable")
	}

	// Read what each change would overwrite, before anything is
	// written, so the undo can put it back. A value that is on record
	// but cannot be read is not the same as an absent one: replacing it
	// could not be undone, so that is refused. Forgetting it is still
	// allowed — that is how an operator clears a value that went bad.
	for i := range changes {
		c := &changes[i]
		c.oldKnown = true
		if !set[c.key] {
			continue
		}
		v, gerr := m.secrets.GetToken(secretProvider, agentID, row.ID, c.key)
		switch {
		case gerr == nil:
			c.old, c.hadOld = v, true
		case errors.Is(gerr, sql.ErrNoRows):
		case c.remove:
			c.oldKnown = false
		default:
			return nil, undo, fmt.Errorf("secret %s: the stored value cannot be read (%v); remove it, save, then set it again", c.key, gerr)
		}
	}

	// Pass 2: apply, remembering how to put each one back.
	var restore []func()
	undo = func() {
		for i := len(restore) - 1; i >= 0; i-- {
			restore[i]()
		}
		restore = nil
	}
	for _, c := range changes {
		key := c.key
		old, hadOld, oldKnown := c.old, c.hadOld, c.oldKnown
		if c.remove {
			err = m.secrets.DeleteToken(secretProvider, agentID, row.ID, key)
		} else {
			err = m.secrets.SetToken(secretProvider, agentID, row.ID, key, c.value, time.Time{})
		}
		if err != nil {
			undo()
			verb := "store"
			if c.remove {
				verb = "delete"
			}
			return nil, func() {}, fmt.Errorf("%s secret %s: %w", verb, key, err)
		}
		restore = append(restore, func() {
			if !oldKnown {
				m.logger.Warn("extension secret not rolled back: the previous value could not be read",
					"extension", row.ID, "agent", agentID, "key", key)
				return
			}
			var rerr error
			if hadOld {
				rerr = m.secrets.SetToken(secretProvider, agentID, row.ID, key, old, time.Time{})
			} else {
				rerr = m.secrets.DeleteToken(secretProvider, agentID, row.ID, key)
			}
			if rerr != nil {
				m.logger.Warn("extension secret rollback failed",
					"extension", row.ID, "agent", agentID, "key", key, "error", rerr)
			}
		})
		if c.remove {
			delete(set, key)
		} else {
			set[key] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, undo, nil
	}
	return out, undo, nil
}

// migrateSecretsLocked moves secret values that are sitting in the
// registry in the clear into the credential store. That happens when a
// package update turns an existing setting into a `format: password`
// one, and for rows written before secrets were split out at all.
// Reports whether any row changed. Caller holds m.mu and saves.
func (m *Manager) migrateSecretsLocked() bool {
	if m.secrets == nil {
		return false
	}
	changed := false
	move := func(row *Installed, agentID string, cfg map[string]any, names []string, keys []string) []string {
		for _, key := range keys {
			v, present := cfg[key]
			if !present {
				continue
			}
			val, ok := v.(string)
			if !ok || len(val) > maxSecretLen {
				// Not something the store can hold. Leave it for the
				// operator to re-enter rather than throw it away.
				m.logger.Warn("extension secret left unmigrated: not a storable string",
					"extension", row.ID, "agent", agentID, "key", key)
				continue
			}
			if val != "" {
				if err := m.secrets.SetToken(secretProvider, agentID, row.ID, key, val, time.Time{}); err != nil {
					// Leave it where it is: plaintext is bad, losing
					// the operator's credential is worse.
					m.logger.Warn("extension secret migration failed",
						"extension", row.ID, "agent", agentID, "key", key, "error", err)
					continue
				}
				if !containsString(names, key) {
					names = append(names, key)
					sort.Strings(names)
				}
			}
			delete(cfg, key)
			changed = true
		}
		return names
	}
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		keys := m.secretKeys(row)
		if len(keys) == 0 {
			continue
		}
		row.Secrets = move(row, "", row.Config, row.Secrets, keys)
		for agentID, b := range row.Agents {
			b.Secrets = move(row, agentID, b.Config, b.Secrets, keys)
			row.Agents[agentID] = b
		}
	}
	return changed
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Secrets returns the decrypted secrets a package should see for one
// agent ("" for the instance-wide set alone). Global secrets come
// first and the agent's own override them, mirroring how per-agent
// settings shadow the global object. A value that fails to decrypt is
// skipped and logged: a package missing one secret reports that
// clearly, whereas a failed launch would hide which one.
func (m *Manager) Secrets(id, agentID string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := map[string]string{}
	if m.secrets == nil {
		return out, nil
	}
	read := func(owner string, names []string) {
		for _, key := range names {
			v, err := m.secrets.GetToken(secretProvider, owner, id, key)
			if err != nil {
				m.logger.Warn("extension secret unreadable",
					"extension", id, "agent", owner, "key", key, "error", err)
				continue
			}
			out[key] = v
		}
	}
	read("", row.Secrets)
	if agentID != "" {
		if b, ok := row.Agents[agentID]; ok {
			read(agentID, b.Secrets)
		}
	}
	return out, nil
}

// dropSecretsLocked forgets every secret filed for (id, agentID).
// Best effort: the registry row is already gone by the time this runs,
// so a failure leaves an orphaned ciphertext nothing can address.
// Caller holds m.mu.
func (m *Manager) dropSecretsLocked(id, agentID string) {
	if m.secrets == nil {
		return
	}
	if err := m.secrets.DeleteTokensBySource(secretProvider, agentID, id); err != nil {
		m.logger.Warn("delete extension secrets failed",
			"extension", id, "agent", agentID, "error", err)
	}
}
