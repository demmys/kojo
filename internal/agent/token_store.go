package agent

import (
	"fmt"
	"time"
)

// createTokenTable creates the notify_tokens table for storing OAuth2 and
// other encrypted tokens used by notification sources.
func createTokenTable(s *CredentialStore) error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS notify_tokens (
		provider   TEXT NOT NULL,
		agent_id   TEXT NOT NULL DEFAULT '',
		source_id  TEXT NOT NULL DEFAULT '',
		key        TEXT NOT NULL,
		value_enc  TEXT NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (provider, agent_id, source_id, key)
	)`)
	return err
}

// SetToken stores an encrypted token value.
// Use empty agent_id/source_id for global tokens (e.g., OAuth2 client secrets).
func (s *CredentialStore) SetToken(provider, agentID, sourceID, key, value string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	enc, err := s.encryptChecked(value)
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}

	var expUnix int64
	if !expiresAt.IsZero() {
		expUnix = expiresAt.Unix()
	}

	_, err = s.db.Exec(
		`INSERT INTO notify_tokens (provider, agent_id, source_id, key, value_enc, expires_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider, agent_id, source_id, key)
		 DO UPDATE SET value_enc=excluded.value_enc, expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		provider, agentID, sourceID, key, enc, expUnix, time.Now().Unix(),
	)
	return err
}

// GetToken retrieves and decrypts a token value.
func (s *CredentialStore) GetToken(provider, agentID, sourceID, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var enc string
	err := s.db.QueryRow(
		`SELECT value_enc FROM notify_tokens WHERE provider=? AND agent_id=? AND source_id=? AND key=?`,
		provider, agentID, sourceID, key,
	).Scan(&enc)
	if err != nil {
		return "", err
	}
	return s.decrypt(enc)
}

// GetTokenExpiry retrieves a token's value and expiration time.
func (s *CredentialStore) GetTokenExpiry(provider, agentID, sourceID, key string) (string, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var enc string
	var expUnix int64
	err := s.db.QueryRow(
		`SELECT value_enc, expires_at FROM notify_tokens WHERE provider=? AND agent_id=? AND source_id=? AND key=?`,
		provider, agentID, sourceID, key,
	).Scan(&enc, &expUnix)
	if err != nil {
		return "", time.Time{}, err
	}
	val, err := s.decrypt(enc)
	if err != nil {
		return "", time.Time{}, err
	}
	var exp time.Time
	if expUnix > 0 {
		exp = time.Unix(expUnix, 0)
	}
	return val, exp, nil
}

// DeleteToken removes a single token.
func (s *CredentialStore) DeleteToken(provider, agentID, sourceID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`DELETE FROM notify_tokens WHERE provider=? AND agent_id=? AND source_id=? AND key=?`,
		provider, agentID, sourceID, key,
	)
	return err
}

// DeleteTokensBySource removes all tokens for a specific source.
func (s *CredentialStore) DeleteTokensBySource(provider, agentID, sourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`DELETE FROM notify_tokens WHERE provider=? AND agent_id=? AND source_id=?`,
		provider, agentID, sourceID,
	)
	return err
}

// DeleteTokensByAgent removes all notify tokens for an agent.
func (s *CredentialStore) DeleteTokensByAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM notify_tokens WHERE agent_id=?`, agentID)
	return err
}

// ListTokenKeys returns all keys stored for a given provider/agent/source.
func (s *CredentialStore) ListTokenKeys(provider, agentID, sourceID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT key FROM notify_tokens WHERE provider=? AND agent_id=? AND source_id=?`,
		provider, agentID, sourceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
