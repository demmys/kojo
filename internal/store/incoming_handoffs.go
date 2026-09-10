package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrStaleHandoff = errors.New("store: handoff source, operation or ownership generation changed")
var ErrIncomingHandoffPending = errors.New("store: incoming handoff has not been finalized")

type IncomingHandoff struct {
	AgentID       string
	OpID          string
	SourcePeer    string
	TargetPeer    string
	Expected      AgentLockVersion
	DelegatedBy   string // receiver-verified route from Expected.Holder to SourcePeer
	AcceptedToken int64
	AllowedProxy  string
	Phase         string
}

func scanIncomingHandoff(row rowScanner) (*IncomingHandoff, error) {
	var h IncomingHandoff
	err := row.Scan(&h.AgentID, &h.OpID, &h.SourcePeer, &h.TargetPeer, &h.Expected.Token, &h.Expected.Holder, &h.DelegatedBy, &h.AcceptedToken, &h.AllowedProxy, &h.Phase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &h, err
}

const incomingHandoffColumns = `agent_id, op_id, source_peer, target_peer, expected_token, expected_holder, delegated_by, accepted_token, allowed_proxy, phase`

func (s *Store) GetIncomingHandoff(ctx context.Context, agentID, opID string) (*IncomingHandoff, error) {
	return scanIncomingHandoff(s.db.QueryRowContext(ctx, `SELECT `+incomingHandoffColumns+` FROM incoming_handoffs WHERE agent_id=? AND op_id=?`, agentID, opID))
}

func agentLockVersionTx(ctx context.Context, tx *sql.Tx, agentID string) (AgentLockVersion, error) {
	var v AgentLockVersion
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT next_token FROM agent_fencing_counters WHERE agent_id=?),0), COALESCE((SELECT holder_peer FROM agent_locks WHERE agent_id=?),'')`, agentID, agentID).Scan(&v.Token, &v.Holder)
	return v, err
}

// ValidateIncomingHandoff is the pre-filesystem gate. Snapshot commit repeats
// this validation in its transaction, closing ownership changes during IO.
func (s *Store) ValidateIncomingHandoff(ctx context.Context, h *IncomingHandoff) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return validateIncomingHandoffTx(ctx, tx, h)
}

func validateIncomingHandoffTx(ctx context.Context, tx *sql.Tx, h *IncomingHandoff) error {
	if h.AgentID == "" || h.OpID == "" || h.SourcePeer == "" || h.TargetPeer == "" || h.SourcePeer == h.TargetPeer {
		return ErrStaleHandoff
	}
	v, err := agentLockVersionTx(ctx, tx, h.AgentID)
	if err != nil {
		return err
	}
	if v != h.Expected || v.Holder == h.TargetPeer {
		return ErrStaleHandoff
	}
	if v.Holder != "" && v.Holder != h.SourcePeer && h.DelegatedBy != v.Holder {
		return ErrStaleHandoff
	}
	var cancelled bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incoming_handoff_cancellations WHERE agent_id=? AND op_id=?)`, h.AgentID, h.OpID).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return ErrStaleHandoff
	}
	old, err := scanIncomingHandoff(tx.QueryRowContext(ctx, `SELECT `+incomingHandoffColumns+` FROM incoming_handoffs WHERE agent_id=? AND op_id=?`, h.AgentID, h.OpID))
	if err == nil {
		if old.Phase != "prepared" || old.SourcePeer != h.SourcePeer || old.TargetPeer != h.TargetPeer || old.Expected != h.Expected || old.DelegatedBy != h.DelegatedBy {
			return ErrStaleHandoff
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	// An unknown op cannot be ordered against another in-flight op. Require
	// an explicit source cancellation instead of letting a delayed old request
	// replace the current snapshot. Changed ownership makes old receipts inert.
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incoming_handoffs WHERE agent_id=? AND op_id<>? AND phase IN ('prepared','accepted','activated') AND CASE WHEN accepted_token>0 THEN accepted_token ELSE expected_token END=?)`, h.AgentID, h.OpID, v.Token).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrStaleHandoff
	}
	return nil
}

// Called inside the same transaction as the phase-1 snapshot. A replay never
// resets an accepted/terminal operation or recaptures a newer generation.
func prepareIncomingHandoffTx(ctx context.Context, tx *sql.Tx, h *IncomingHandoff) error {
	if err := validateIncomingHandoffTx(ctx, tx, h); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO incoming_handoffs (agent_id,op_id,source_peer,target_peer,expected_token,expected_holder,delegated_by,phase) VALUES (?,?,?,?,?,?,?,'prepared') ON CONFLICT(agent_id,op_id) DO NOTHING`, h.AgentID, h.OpID, h.SourcePeer, h.TargetPeer, h.Expected.Token, h.Expected.Holder, h.DelegatedBy)
	return err
}

// AcceptIncomingHandoff consumes the authenticated source's finalize
// attestation (complete and drain finished) for exactly the prepared operation.
// The target's local counter, not the source's unrelated counter, fences the
// claim. Holder, proxy authorization and the retry receipt commit together.
func (s *Store) AcceptIncomingHandoff(ctx context.Context, agentID, opID, source, target, proxy string, now, lease int64) (*IncomingHandoff, error) {
	if proxy == "" || lease <= 0 {
		return nil, ErrStaleHandoff
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	h, err := scanIncomingHandoff(tx.QueryRowContext(ctx, `SELECT `+incomingHandoffColumns+` FROM incoming_handoffs WHERE agent_id=? AND op_id=?`, agentID, opID))
	if err != nil {
		return nil, err
	}
	if h.SourcePeer != source || h.TargetPeer != target || h.Phase == "aborted" {
		return nil, ErrStaleHandoff
	}
	v, err := agentLockVersionTx(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	if h.Phase == "done" {
		// A completed op is a no-op, never an instruction to activate again.
		return h, nil
	}
	if h.Phase == "accepted" || h.Phase == "activated" {
		if h.AllowedProxy != proxy || v.Token != h.AcceptedToken || (v.Holder != target && v.Holder != "") {
			return nil, ErrStaleHandoff
		}
		if v.Holder == target {
			lock, err := scanAgentLockTx(ctx, tx, agentID)
			if err != nil {
				return nil, err
			}
			if lock.FencingToken != h.AcceptedToken || lock.AllowedProxyPeer != proxy {
				return nil, ErrStaleHandoff
			}
			return h, nil
		}
		// Graceful shutdown may have removed the row. Reissue only if the
		// durable counter proves no ownership change since this acceptance.
	} else if v != h.Expected {
		return nil, ErrStaleHandoff
	}
	token, err := nextFencingToken(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_locks (agent_id,holder_peer,fencing_token,lease_expires_at,acquired_at,allowed_proxy_peer) VALUES (?,?,?,?,?,?) ON CONFLICT(agent_id) DO UPDATE SET holder_peer=excluded.holder_peer,fencing_token=excluded.fencing_token,lease_expires_at=excluded.lease_expires_at,acquired_at=excluded.acquired_at,allowed_proxy_peer=excluded.allowed_proxy_peer`, agentID, target, token, now+lease, now, proxy)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE incoming_handoffs SET phase='accepted', accepted_token=?, allowed_proxy=? WHERE agent_id=? AND op_id=?`, token, proxy, agentID, opID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	h.Phase = "accepted"
	h.AcceptedToken = token
	h.AllowedProxy = proxy
	return h, nil
}

// Mark activation only after token adoption, durable arrival/proxy markers
// and Guard registration succeed. Arrival admission may now use the runtime.
func (s *Store) ActivateIncomingHandoff(ctx context.Context, agentID, opID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE incoming_handoffs SET phase='activated' WHERE agent_id=? AND op_id=? AND phase IN ('accepted','activated') AND accepted_token=(SELECT fencing_token FROM agent_locks WHERE agent_id=?) AND target_peer=(SELECT holder_peer FROM agent_locks WHERE agent_id=?)`, agentID, opID, agentID, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStaleHandoff
	}
	return nil
}

func (s *Store) IsIncomingHandoffIncomplete(ctx context.Context, agentID string) (bool, error) {
	var blocked bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incoming_handoffs h WHERE h.agent_id=? AND h.phase IN ('prepared','accepted','aborted') AND COALESCE((SELECT next_token FROM agent_fencing_counters WHERE agent_id=?),0)=CASE WHEN h.accepted_token>0 THEN h.accepted_token ELSE h.expected_token END)`, agentID, agentID).Scan(&blocked)
	return blocked, err
}

func (s *Store) FinishIncomingHandoff(ctx context.Context, agentID, opID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE incoming_handoffs SET phase='done' WHERE agent_id=? AND op_id=? AND (phase='done' OR (phase='activated' AND accepted_token=(SELECT fencing_token FROM agent_locks WHERE agent_id=?) AND target_peer=(SELECT holder_peer FROM agent_locks WHERE agent_id=?)))`, agentID, opID, agentID, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStaleHandoff
	}
	return nil
}

func (s *Store) AbortIncomingHandoff(ctx context.Context, agentID, opID, source string) error {
	if agentID == "" || opID == "" || source == "" {
		return ErrStaleHandoff
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	h, err := scanIncomingHandoff(tx.QueryRowContext(ctx, `SELECT `+incomingHandoffColumns+` FROM incoming_handoffs WHERE agent_id=? AND op_id=?`, agentID, opID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && (h.SourcePeer != source || (h.Phase != "prepared" && h.Phase != "aborted")) {
		return ErrStaleHandoff
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO incoming_handoff_cancellations (agent_id,op_id,source_peer) VALUES (?,?,?) ON CONFLICT(agent_id,op_id) DO NOTHING`, agentID, opID, source); err != nil {
		return err
	}
	var boundSource string
	if err := tx.QueryRowContext(ctx, `SELECT source_peer FROM incoming_handoff_cancellations WHERE agent_id=? AND op_id=?`, agentID, opID).Scan(&boundSource); err != nil {
		return err
	}
	if boundSource != source {
		return ErrStaleHandoff
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incoming_handoffs SET phase='aborted' WHERE agent_id=? AND op_id=? AND phase='prepared'`, agentID, opID); err != nil {
		return err
	}
	return tx.Commit()
}

// IncompleteIncomingHandoffs gates startup and pre-admission checks. A local
// force-reclaim advances the counter and therefore makes old tombstones inert.
func (s *Store) IncompleteIncomingHandoffs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT h.agent_id FROM incoming_handoffs h LEFT JOIN agent_fencing_counters c ON c.agent_id=h.agent_id WHERE h.phase IN ('prepared','accepted','aborted') AND COALESCE(c.next_token,0)=CASE WHEN h.accepted_token>0 THEN h.accepted_token ELSE h.expected_token END`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Ordinary Guard acquisition must not turn a prepared/aborted snapshot into
// a runtime merely because an unrelated outgoing shadow lease expired.
func incomingHandoffBlocksAcquireTx(ctx context.Context, tx *sql.Tx, agentID string) (bool, error) {
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incoming_handoffs h WHERE h.agent_id=? AND h.phase IN ('prepared','aborted') AND COALESCE((SELECT next_token FROM agent_fencing_counters WHERE agent_id=?),0)=CASE WHEN h.accepted_token>0 THEN h.accepted_token ELSE h.expected_token END)`, agentID, agentID).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("incoming handoff fence: %w", err)
	}
	return blocked, nil
}

// Carry a successfully activated receipt across graceful release/reacquire.
// An intervening ownership generation must not be mistaken for a restart.
func resumeActivatedIncomingTx(ctx context.Context, tx *sql.Tx, agentID, target string, previousToken, nextToken int64) (string, error) {
	var op, proxy string
	err := tx.QueryRowContext(ctx, `SELECT op_id,allowed_proxy FROM incoming_handoffs WHERE agent_id=? AND target_peer=? AND phase='activated' AND accepted_token=?`, agentID, target, previousToken).Scan(&op, &proxy)
	if errors.Is(err, sql.ErrNoRows) {
		return target, nil
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incoming_handoffs SET accepted_token=? WHERE agent_id=? AND op_id=?`, nextToken, agentID, op); err != nil {
		return "", err
	}
	return proxy, nil
}
