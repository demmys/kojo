package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ExternalAgentRecord mirrors one alive row of `external_agents`.
// TokenHash / PairingHash are sha256 hex digests ("" = none).
type ExternalAgentRecord struct {
	ID             string
	Name           string
	Description    string
	StableNodeID   string // "" until a pairing code is redeemed
	NodeName       string
	TokenHash      string
	TokenGen       int64
	PairingHash    string
	PairingExpires int64 // unix millis, 0 = none
	Enabled        bool
	RatePerMin     int
	LastSeenAt     int64
	CreatedAt      int64
	UpdatedAt      int64
}

// ExternalNodeDenyRecord is one row of `external_node_denylist`.
type ExternalNodeDenyRecord struct {
	StableNodeID string
	NodeName     string
	TailnetIPs   []string
	ExternalID   string
	CreatedAt    int64
	UpdatedAt    int64
}

// ErrExternalNodeBound is returned when a pairing would bind a tailnet
// node that another alive external agent already holds.
var ErrExternalNodeBound = errors.New("store: tailnet node already bound to another external agent")

// ErrPairingInvalid covers an unknown, already-used, or expired code.
var ErrPairingInvalid = errors.New("store: pairing code invalid or expired")

const externalAgentCols = `id, name, description, COALESCE(stable_node_id,''), node_name,
  COALESCE(token_hash,''), token_gen, COALESCE(pairing_hash,''), COALESCE(pairing_expires,0),
  enabled, rate_per_min, COALESCE(last_seen_at,0), created_at, updated_at`

func scanExternalAgent(sc interface{ Scan(...any) error }) (*ExternalAgentRecord, error) {
	var r ExternalAgentRecord
	var enabled int
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &r.StableNodeID, &r.NodeName,
		&r.TokenHash, &r.TokenGen, &r.PairingHash, &r.PairingExpires,
		&enabled, &r.RatePerMin, &r.LastSeenAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Enabled = enabled != 0
	return &r, nil
}

// CreateExternalAgent inserts a new, unpaired external agent.
func (s *Store) CreateExternalAgent(ctx context.Context, id, name, description string, ratePerMin int) (*ExternalAgentRecord, error) {
	if id == "" || strings.TrimSpace(name) == "" {
		return nil, errors.New("store.CreateExternalAgent: id and name required")
	}
	if ratePerMin <= 0 {
		ratePerMin = 20
	}
	now := NowMillis()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO external_agents (id, name, description, enabled, rate_per_min, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?, ?)`, id, name, description, ratePerMin, now, now); err != nil {
		return nil, fmt.Errorf("store.CreateExternalAgent: %w", err)
	}
	return s.GetExternalAgent(ctx, id)
}

// GetExternalAgent returns an alive row. ErrNotFound on miss.
func (s *Store) GetExternalAgent(ctx context.Context, id string) (*ExternalAgentRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+externalAgentCols+` FROM external_agents WHERE id = ? AND deleted_at IS NULL`, id)
	r, err := scanExternalAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListExternalAgents returns every alive row, oldest first.
func (s *Store) ListExternalAgents(ctx context.Context) ([]*ExternalAgentRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+externalAgentCols+` FROM external_agents WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ExternalAgentRecord
	for rows.Next() {
		r, err := scanExternalAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupExternalAgentByToken resolves a token hash to its alive row.
// ErrNotFound on miss. Callers still check Enabled and the node binding.
func (s *Store) LookupExternalAgentByToken(ctx context.Context, tokenHash string) (*ExternalAgentRecord, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+externalAgentCols+` FROM external_agents WHERE token_hash = ? AND deleted_at IS NULL`, tokenHash)
	r, err := scanExternalAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ExternalAgentPatch is a partial update; nil fields are left alone.
type ExternalAgentPatch struct {
	Name        *string
	Description *string
	Enabled     *bool
	RatePerMin  *int
}

// UpdateExternalAgent applies patch to an alive row.
func (s *Store) UpdateExternalAgent(ctx context.Context, id string, p ExternalAgentPatch) (*ExternalAgentRecord, error) {
	sets := []string{}
	args := []any{}
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return nil, errors.New("store.UpdateExternalAgent: empty name")
		}
		sets = append(sets, "name = ?")
		args = append(args, *p.Name)
	}
	if p.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *p.Description)
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *p.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if p.RatePerMin != nil {
		if *p.RatePerMin <= 0 {
			return nil, errors.New("store.UpdateExternalAgent: rate must be positive")
		}
		sets = append(sets, "rate_per_min = ?")
		args = append(args, *p.RatePerMin)
	}
	if len(sets) > 0 {
		sets = append(sets, "updated_at = ?")
		args = append(args, NowMillis(), id)
		res, err := s.db.ExecContext(ctx,
			`UPDATE external_agents SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`, args...)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrNotFound
		}
	}
	return s.GetExternalAgent(ctx, id)
}

// SoftDeleteExternalAgent tombstones the row and drops its credentials.
// The node's denylist row is deliberately kept.
func (s *Store) SoftDeleteExternalAgent(ctx context.Context, id string) error {
	now := NowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
UPDATE external_agents SET deleted_at = ?, updated_at = ?, token_hash = NULL,
  token_gen = token_gen + 1, pairing_hash = NULL, pairing_expires = NULL
WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM external_agent_cursors WHERE external_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetExternalPairing stores a fresh pairing-code hash, replacing any
// previous unused code.
func (s *Store) SetExternalPairing(ctx context.Context, id, codeHash string, expiresAt int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE external_agents SET pairing_hash = ?, pairing_expires = ?, updated_at = ?
WHERE id = ? AND deleted_at IS NULL`, codeHash, expiresAt, NowMillis(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExternalNodeIdentity is what WhoIs told us about the calling node.
type ExternalNodeIdentity struct {
	StableNodeID string
	NodeName     string
	TailnetIPs   []string
}

// RedeemExternalPairing consumes a pairing code: it binds the calling
// node, installs the new token hash (bumping token_gen so any session
// on the previous token is dead), and records the node in the denylist.
// All in one transaction.
func (s *Store) RedeemExternalPairing(ctx context.Context, codeHash string, node ExternalNodeIdentity, tokenHash string) (*ExternalAgentRecord, error) {
	if codeHash == "" || tokenHash == "" || node.StableNodeID == "" {
		return nil, errors.New("store.RedeemExternalPairing: code, token and node required")
	}
	now := NowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	var expires int64
	err = tx.QueryRowContext(ctx, `
SELECT id, COALESCE(pairing_expires,0) FROM external_agents
WHERE pairing_hash = ? AND deleted_at IS NULL`, codeHash).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPairingInvalid
	}
	if err != nil {
		return nil, err
	}
	if expires <= now {
		// Burn the expired code so it can never be retried.
		if _, err := tx.ExecContext(ctx,
			`UPDATE external_agents SET pairing_hash = NULL, pairing_expires = NULL WHERE id = ?`, id); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrPairingInvalid
	}

	var other string
	err = tx.QueryRowContext(ctx, `
SELECT id FROM external_agents WHERE stable_node_id = ? AND deleted_at IS NULL AND id != ?`,
		node.StableNodeID, id).Scan(&other)
	if err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExternalNodeBound, other)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE external_agents SET stable_node_id = ?, node_name = ?, token_hash = ?,
  token_gen = token_gen + 1, pairing_hash = NULL, pairing_expires = NULL,
  last_seen_at = ?, updated_at = ?
WHERE id = ?`, node.StableNodeID, node.NodeName, tokenHash, now, now, id); err != nil {
		return nil, err
	}
	if err := upsertExternalNodeDenyTx(ctx, tx, node, id, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetExternalAgent(ctx, id)
}

// RevokeExternalToken drops the token (node binding stays) and bumps
// token_gen so live sessions notice.
func (s *Store) RevokeExternalToken(ctx context.Context, id string) (*ExternalAgentRecord, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE external_agents SET token_hash = NULL, token_gen = token_gen + 1, updated_at = ?
WHERE id = ? AND deleted_at IS NULL`, NowMillis(), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetExternalAgent(ctx, id)
}

// TouchExternalAgent records a successful authenticated call: last_seen
// and the node's current tailnet IPs in the denylist row.
func (s *Store) TouchExternalAgent(ctx context.Context, id string, node ExternalNodeIdentity) error {
	now := NowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE external_agents SET last_seen_at = ? WHERE id = ? AND deleted_at IS NULL`, now, id); err != nil {
		return err
	}
	if node.StableNodeID != "" {
		if err := upsertExternalNodeDenyTx(ctx, tx, node, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// upsertExternalNodeDenyTx inserts or refreshes a denylist row. IPs are
// merged (never dropped) so an address the node used to have still
// refuses until the operator releases the row.
func upsertExternalNodeDenyTx(ctx context.Context, tx *sql.Tx, node ExternalNodeIdentity, externalID string, now int64) error {
	var prevJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT tailnet_ips_json FROM external_node_denylist WHERE stable_node_id = ?`, node.StableNodeID).Scan(&prevJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var ips []string
	if prevJSON != "" {
		_ = json.Unmarshal([]byte(prevJSON), &ips)
	}
	for _, ip := range node.TailnetIPs {
		if ip != "" && !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	if ips == nil {
		ips = []string{}
	}
	b, err := json.Marshal(ips)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO external_node_denylist (stable_node_id, node_name, tailnet_ips_json, external_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(stable_node_id) DO UPDATE SET
  node_name = excluded.node_name, tailnet_ips_json = excluded.tailnet_ips_json,
  external_id = excluded.external_id, updated_at = excluded.updated_at`,
		node.StableNodeID, node.NodeName, string(b), externalID, now, now)
	return err
}

// ListExternalNodeDenylist returns every denylisted node.
func (s *Store) ListExternalNodeDenylist(ctx context.Context) ([]*ExternalNodeDenyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT stable_node_id, node_name, tailnet_ips_json, external_id, created_at, updated_at
FROM external_node_denylist ORDER BY created_at, stable_node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ExternalNodeDenyRecord
	for rows.Next() {
		var r ExternalNodeDenyRecord
		var ipsJSON string
		if err := rows.Scan(&r.StableNodeID, &r.NodeName, &ipsJSON, &r.ExternalID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ipsJSON), &r.TailnetIPs)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteExternalNodeDeny releases a node from the denylist. Refused
// while an alive external agent is still bound to it.
func (s *Store) DeleteExternalNodeDeny(ctx context.Context, stableNodeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var other string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM external_agents WHERE stable_node_id = ? AND deleted_at IS NULL`, stableNodeID).Scan(&other)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrExternalNodeBound, other)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM external_node_denylist WHERE stable_node_id = ?`, stableNodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// GetExternalCursors returns groupdm_id → acked_seq for one external agent.
func (s *Store) GetExternalCursors(ctx context.Context, externalID string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT groupdm_id, acked_seq FROM external_agent_cursors WHERE external_id = ?`, externalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var g string
		var seq int64
		if err := rows.Scan(&g, &seq); err != nil {
			return nil, err
		}
		out[g] = seq
	}
	return out, rows.Err()
}

// AdvanceExternalCursor moves the read position forward; it never goes
// backwards.
func (s *Store) AdvanceExternalCursor(ctx context.Context, externalID, groupID string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO external_agent_cursors (external_id, groupdm_id, acked_seq, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(external_id, groupdm_id) DO UPDATE SET
  acked_seq = MAX(acked_seq, excluded.acked_seq), updated_at = excluded.updated_at`,
		externalID, groupID, seq, NowMillis())
	return err
}

// DeleteExternalCursor drops one read position (member removed).
func (s *Store) DeleteExternalCursor(ctx context.Context, externalID, groupID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM external_agent_cursors WHERE external_id = ? AND groupdm_id = ?`, externalID, groupID)
	return err
}

// ListGroupDMExternalTrust returns every room's non-default trust level.
func (s *Store) ListGroupDMExternalTrust(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT groupdm_id, trust FROM groupdm_external_trust`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// SetGroupDMExternalTrust sets a room's trust level; "" or "untrusted"
// clears the row (back to the default).
func (s *Store) SetGroupDMExternalTrust(ctx context.Context, groupID, trust string) error {
	if trust == "" || trust == "untrusted" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM groupdm_external_trust WHERE groupdm_id = ?`, groupID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO groupdm_external_trust (groupdm_id, trust, updated_at) VALUES (?, ?, ?)
ON CONFLICT(groupdm_id) DO UPDATE SET trust = excluded.trust, updated_at = excluded.updated_at`,
		groupID, trust, NowMillis())
	return err
}
