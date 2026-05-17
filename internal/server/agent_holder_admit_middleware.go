package server

import (
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
)

// agentHolderAdmitMiddleware narrowly promotes an untrusted
// RolePeer request to the agent-proxy surface when THIS host is
// the current lock holder for the targeted agent. The promotion
// lives ONLY in the request's context.Principal; the
// peer_registry.trusted column stays untouched, so the other
// privileged surfaces (sessions, files, git, upload, info, dirs)
// remain closed.
//
// Direction of the check: a post-§3.7 device-switch leaves
// agent_locks.holder_peer == THIS peer's device_id on the host
// that adopted the agent. Every post-switch Hub→peer chat /
// messages / persona / memory edit travels as a RolePeer-signed
// proxy from the Hub; the SIGNER is the Hub, not the peer that
// holds the lock, so a signer-equals-holder check would always
// fail. The "self holds the lock" gate is what threads the
// needle: we admit any RolePeer-signed write only when the agent
// in question actually lives on this host, which is the same
// condition the local runtime checks before accepting frames at
// the fencing layer.
//
// Trust trade-off vs the strict PeerTrusted gate: any PAIRED
// peer (not just the Hub) can reach the agents/* surface for
// agents whose runtime currently lives on this host. The lock
// is the authoritative permission token here — moving an agent
// to this host implicitly authorises the cluster to talk to it,
// and an unpaired peer's signature wouldn't pass PeerAuth in the
// first place. Operators who want stricter scoping can flip the
// peer's registry.trusted bit to keep using the per-row gate.
//
// Chain placement: this middleware MUST run AFTER PeerAuth (so
// p.PeerID is populated) and BEFORE EnforceMiddleware (so the
// promoted Principal participates in the policy gate). Owner /
// already-trusted-peer / non-peer principals pass through
// untouched.
func (s *Server) agentHolderAdmitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/agents/") {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.FromContext(r.Context())
		if !p.IsPeer() || p.PeerTrusted {
			next.ServeHTTP(w, r)
			return
		}
		id, _, ok := auth.SplitAgentIDPath(r.URL.Path)
		if !ok || id == "" {
			next.ServeHTTP(w, r)
			return
		}
		if s.agents == nil || s.agents.Store() == nil || s.peerID == nil {
			next.ServeHTTP(w, r)
			return
		}
		lock, err := s.agents.Store().GetAgentLock(r.Context(), id)
		if err != nil || lock == nil {
			// No lock row or read error — fall through. The
			// policy gate will 403 this RolePeer request, which
			// is the correct default-deny posture for an agent
			// this host doesn't claim to hold.
			next.ServeHTTP(w, r)
			return
		}
		if lock.HolderPeer == "" || lock.HolderPeer != s.peerID.DeviceID {
			// This host isn't the holder — the agent lives
			// elsewhere; refuse via the policy gate.
			next.ServeHTTP(w, r)
			return
		}
		// Local host holds the lock for the targeted agent.
		// Admit the peer-signed proxy for THIS request only; the
		// registry row stays untrusted. The next request re-runs
		// the lookup, so a lock release revokes the admit on the
		// very next call.
		promoted := p
		promoted.PeerTrusted = true
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), promoted)))
	})
}
