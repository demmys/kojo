package store

import (
	"context"
)

// AgentLockVersion identifies a local ownership generation, including an
// empty-row gap after release. Lease renewal does not change the version.
// Tokens are local to one Store; never compare them across peers.
type AgentLockVersion struct {
	Token  int64  `json:"token"`
	Holder string `json:"holder"`
}

func (s *Store) GetAgentLockVersion(ctx context.Context, agentID string) (AgentLockVersion, error) {
	var v AgentLockVersion
	err := s.db.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT next_token FROM agent_fencing_counters WHERE agent_id = ?), 0),
 COALESCE((SELECT holder_peer FROM agent_locks WHERE agent_id = ?), '')`, agentID, agentID).Scan(&v.Token, &v.Holder)
	return v, err
}
