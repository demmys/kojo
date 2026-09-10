package server

import (
	"context"
	"fmt"

	"github.com/loppo-llc/kojo/internal/store"
)

// authorizeIncomingSource never purges/rewrites a lock. For a multi-hop return
// A→B→C→A, A may still record B. Follow that recorded peer's DB route (not the
// external-chat hint cache) until it delegates to C, using the device-auth
// transport. The receiver binds this observation to its unchanged local
// version in the snapshot transaction and later consumes it in finalize.
// This is v1 delegation evidence, not distributed ownership consensus.
func (s *Server) authorizeIncomingSource(ctx context.Context, agentID, source string) (store.AgentLockVersion, string, error) {
	st := s.agents.Store()
	version, err := st.GetAgentLockVersion(ctx, agentID)
	if err != nil {
		return version, "", err
	}
	if version.Holder == "" || version.Holder == source {
		return version, "", nil
	}
	if s.peerID == nil || version.Holder == s.peerID.DeviceID {
		return version, "", store.ErrStaleHandoff
	}
	router := newExternalChatRouter(s)
	current := version.Holder
	seen := map[string]bool{s.peerID.DeviceID: true}
	for hop := 0; hop < 8; hop++ {
		if current == "" || seen[current] {
			break
		}
		seen[current] = true
		reply, err := router.probeHolder(ctx, agentID, current)
		if err != nil {
			return version, "", fmt.Errorf("verify handoff delegation from %s: %w", current, err)
		}
		if reply.HolderPeer == source {
			// Do not let a response received after local reclaim authorize phase-1.
			now, err := st.GetAgentLockVersion(ctx, agentID)
			if err != nil {
				return version, "", err
			}
			if now != version {
				return version, "", store.ErrStaleHandoff
			}
			return version, version.Holder, nil
		}
		current = reply.HolderPeer
	}
	return version, "", fmt.Errorf("recorded holder does not delegate to source %s: %w", source, store.ErrStaleHandoff)
}
