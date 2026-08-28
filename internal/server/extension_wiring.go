package server

import (
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

// Extension wiring
//
// The registry (internal/extpkg) knows nothing about auth or the agent
// backends, and neither of those imports the registry. This file is the
// single place the three are joined: it hands the auth resolver a token
// lookup, hands the agent backends the per-agent skill and MCP
// contributions, and starts the service supervisor.
//
// StartExtensions is called once, after the API listener is bound —
// every extension process is handed KOJO_API_BASE at spawn time and
// reads it immediately, so starting them earlier would hand them an
// address that does not exist yet.

// StartExtensions activates the extension subsystem against a bound
// API listener. apiBase is the loopback auth-listener URL. Calling it
// with no registry (PeerOnly, or a registry that failed to open) or an
// empty apiBase is a no-op, so callers need not branch.
func (s *Server) StartExtensions(apiBase string, resolver *auth.Resolver) {
	if s == nil || s.extensions == nil || apiBase == "" {
		return
	}
	mgr := s.extensions
	mgr.SetAPIBase(apiBase)

	// Extension tokens resolve to auth.RoleExtension. The identity is
	// recomputed per request inside the registry, so disabling a
	// package or unbinding an agent takes effect on the next call.
	if resolver != nil {
		resolver.SetExtensionResolver(func(token string) (auth.ExtensionIdentity, bool) {
			ident, ok := mgr.ResolveToken(token)
			if !ok {
				return auth.ExtensionIdentity{}, false
			}
			return auth.ExtensionIdentity{
				ID:         ident.ID,
				Scopes:     ident.Scopes,
				AgentScope: ident.AgentScope,
			}, true
		})
	}

	// Per-agent contributions. Both lookups run on every prepareChat,
	// which is what makes enable/disable/update take effect on an
	// agent's next turn rather than at the next restart.
	agent.SetExtensionSkillLookup(func(agentID string) []agent.ExtensionSkill {
		rows := mgr.SkillsForAgent(agentID)
		out := make([]agent.ExtensionSkill, 0, len(rows))
		for _, r := range rows {
			out = append(out, agent.ExtensionSkill{
				ExtensionID: r.ExtensionID,
				Name:        r.Name,
				Dir:         r.Dir,
			})
		}
		return out
	})
	agent.SetExternalMCPLookup(func(agentID string) []agent.ExternalMCPServer {
		rows := mgr.MCPServersForAgent(agentID)
		out := make([]agent.ExternalMCPServer, 0, len(rows))
		for _, r := range rows {
			out = append(out, agent.ExternalMCPServer{
				Name:    r.Name,
				Command: r.Command,
				Args:    r.Args,
				Env:     r.Env,
			})
		}
		return out
	})

	s.extSupervisor = extpkg.NewSupervisor(mgr, s.logger)
	// An archived agent is not reachable, so its per-agent service has
	// nothing to serve. The registry keeps the binding (unarchive
	// restores the agent's config), the supervisor just declines to
	// run it.
	if s.agents != nil {
		agents := s.agents
		s.extSupervisor.SetAgentFilter(func(agentID string) bool {
			a, ok := agents.Get(agentID)
			return ok && !a.Archived
		})
	}
	s.extSupervisor.Reconcile()
}

// reconcileExtensionServices brings supervised processes in line with
// the registry after a mutation. Every extension handler that changes
// enablement, configuration or the checkout calls it.
func (s *Server) reconcileExtensionServices() {
	if s == nil || s.extSupervisor == nil {
		return
	}
	s.extSupervisor.Reconcile()
}

// runningExtensionServices reports the live service processes, for the
// status the settings UI shows next to each package.
func (s *Server) runningExtensionServices() []extpkg.ServiceStatus {
	if s == nil || s.extSupervisor == nil {
		return nil
	}
	return s.extSupervisor.RunningServices()
}

// stopExtensionServices tears down every supervised process. Called
// from Server.Shutdown.
func (s *Server) stopExtensionServices() {
	if s == nil || s.extSupervisor == nil {
		return
	}
	s.extSupervisor.Shutdown()
}
