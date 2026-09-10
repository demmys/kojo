package server

import (
	"context"

	"github.com/loppo-llc/kojo/internal/store"
)

type sourceHandoffVersionKey struct{}

// A late source-release must be rejected BEFORE stopping runtimes or writing
// release markers. Reclaim and incoming finalize use the same per-agent lock.
func (s *Server) releaseHandoffSource(ctx context.Context, agentID, target string, token int64) {
	unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: agentID})
	defer unlock()
	v, err := s.agents.Store().GetAgentLockVersion(ctx, agentID)
	if err != nil || v != (store.AgentLockVersion{Token: token, Holder: target}) {
		s.logger.Warn("handoff: obsolete source release ignored", "agent", agentID, "expected_holder", target, "expected_token", token, "err", err)
		return
	}
	if s.onAgentReleasedAsSource != nil {
		s.onAgentReleasedAsSource(ctx, agentID)
	}
}
