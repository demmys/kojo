// Package auth provides a lightweight, role-based access control layer for
// kojo's HTTP API. Its purpose is "spoiler prevention" — keeping an agent
// from incidentally reading other agents' Persona / configuration when it
// curls the API on its own. It is NOT a security boundary against a
// malicious agent: an agent runs as the same OS user as kojo itself and
// can read other agents' files directly. See README for the threat model.
package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// Role identifies the actor behind an HTTP request after middleware
// resolution.
type Role int

const (
	// RoleGuest is the default role for unauthenticated requests on the
	// auth-required listener. Guests can only read directory entries.
	RoleGuest Role = iota
	// RoleAgent is a regular agent-bound principal. It can fully read its
	// own data and is limited to DirectoryEntry on other agents.
	RoleAgent
	// RolePrivAgent extends RoleAgent with the ability to delete/reset
	// other agents (but not fork or read their full data).
	RolePrivAgent
	// RolePeer authenticates a request that arrived from a
	// registered remote peer over tsnet (docs §3.10 / §3.7). The
	// principal's PeerID names the device_id from peer_registry;
	// the scope is the inter-peer surface plus proxied
	// agent/session routes — every other gate returns false. Set
	// by TailnetIdentityMiddleware on ServeAuthTsnet (peer-mode
	// primary listener) and on the Hub's public listener via
	// WhoIs → peer_registry; the legacy Ed25519-signed Bearer
	// stamping path is retired. --unsafe collapses the WhoIs
	// check and stamps RolePeer unconditionally for LAN/docker/CI.
	RolePeer
	// RoleOwner is the kojo user. It has full access to everything.
	RoleOwner
	// RoleExtension authenticates an installed extension package's
	// out-of-process service (internal/extpkg). It is the only role
	// whose surface is data-driven: the manifest's scope list, which
	// the Owner acknowledged at install time, is carried on the
	// Principal and consumed by allowExtension. An extension holds
	// strictly less than RoleAgent — it has no self-scoped agent, so
	// every agent-addressed route is gated on the operator having
	// bound the package to that agent.
	RoleExtension
)

// Principal identifies the actor behind a request.
type Principal struct {
	Role Role
	// OwnerDeputy marks an agent principal the Owner has deputised to
	// stand in for them over OTHER agents: create, fork, read in full,
	// PATCH, write their persona and memory, delete and reset. It is
	// strictly stronger than RolePrivAgent, but kept as a flag rather
	// than a Role because the two are orthogonal — an agent may hold
	// either, both, or neither, and only the Role decides
	// CanRestartServer. Never set for the Owner (who needs no flag) or
	// a Peer.
	OwnerDeputy bool
	AgentID     string // populated for RoleAgent / RolePrivAgent
	PeerID      string // populated for RolePeer (device_id from peer_registry); also stamped on RoleOwner when the Hub-public TailnetIdentityMiddleware's WhoIs lookup matches a paired peer, so events handlers can identify which paired-peer connection they're on without re-querying the registry
	// ExtensionID names the installed package behind a RoleExtension
	// request. It doubles as the KV namespace suffix the extension may
	// read and write.
	ExtensionID string
	// Scopes is the manifest scope list the Owner granted at install
	// time. Only meaningful for RoleExtension.
	Scopes []string
	// AgentScope lists the agent IDs the extension is currently bound
	// to AND enabled for. Resolved per request, so revoking a binding
	// takes effect on the next call rather than at the next restart.
	AgentScope []string
}

// IsOwner returns true if the principal is the kojo user.
func (p Principal) IsOwner() bool { return p.Role == RoleOwner }

// IsAgent returns true if the principal is bound to a specific agent
// (regular or privileged).
func (p Principal) IsAgent() bool {
	return p.Role == RoleAgent || p.Role == RolePrivAgent
}

// IsPeer reports whether the principal was authenticated via
// Tailnet identity (WhoIs over tsnet → peer_registry hit). RolePeer
// is scoped to inter-peer endpoints (cross-subscribe status feed,
// blob handoff fetch, device-switch orchestration) AND to proxied
// agent requests (§3.7 remoteAgentProxy: Hub forwards browser/
// agent requests to the holder peer over tsnet). Handler-level
// guard methods (CanReadFull, CanMutateSelf, CanDeleteOrReset)
// admit IsPeer because the Hub already ran Enforce before
// proxying — re-blocking at the handler would 403 every proxied
// request.
func (p Principal) IsPeer() bool { return p.Role == RolePeer }

// IsExtension reports whether the principal is an installed extension
// package's service process.
func (p Principal) IsExtension() bool { return p.Role == RoleExtension }

// HasScope reports whether the extension was granted scope at install
// time. Non-extension principals never hold scopes: the Owner does not
// need them and an agent's surface is decided by AgentID, not a grant
// list, so answering true here would silently widen those roles.
func (p Principal) HasScope(scope string) bool {
	if p.Role != RoleExtension {
		return false
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// BoundTo reports whether the extension is enabled for agentID. An
// extension may only address agents the operator explicitly bound it
// to — installing a package is not consent to touch every agent.
func (p Principal) BoundTo(agentID string) bool {
	if p.Role != RoleExtension || agentID == "" {
		return false
	}
	for _, id := range p.AgentScope {
		if id == agentID {
			return true
		}
	}
	return false
}

// IsOwnerDeputy reports whether this principal is an agent the Owner
// deputised to stand in for them over other agents. Owner and Peer are
// NOT covered: both already pass every gate on their own, and reporting
// them as deputies would make "deputy over someone else" checks read
// wrongly for principals that have no AgentID to compare against.
func (p Principal) IsOwnerDeputy() bool {
	return p.IsAgent() && p.OwnerDeputy
}

// IsOwnerDeputyOver reports whether the principal may act as the Owner's
// proxy on targetID. The target must be someone ELSE: the grant exists
// to manage other agents, and letting it apply to the holder would turn
// it into a self-elevation path (a deputy could clear its own
// disabledInjections, or PATCH itself in ways a plain agent may not).
func (p Principal) IsOwnerDeputyOver(targetID string) bool {
	return p.IsOwnerDeputy() && targetID != "" && p.AgentID != targetID
}

// CanReadFull returns true if the principal can read the full record
// (Persona, Token-bearing fields, etc.) for the given target agent ID.
// Owners can read any. Agents can only read their own. Peers are
// admitted because the Hub's proxy already validated the original
// caller's identity before forwarding.
func (p Principal) CanReadFull(targetID string) bool {
	if p.IsOwner() || p.IsPeer() || p.IsOwnerDeputyOver(targetID) {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanMutateSelf returns true if the principal may issue self-scoped
// mutations (PATCH, reset, etc.) against targetID. Peers pass through
// because the Hub's Enforce layer already authorised the original
// request before the proxy signed and forwarded it.
func (p Principal) CanMutateSelf(targetID string) bool {
	if p.IsOwner() || p.IsPeer() || p.IsOwnerDeputyOver(targetID) {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanDeleteOrReset returns true for delete/reset/unarchive/checkin/
// reset-session ops. Owner: any. PrivAgent: any. Agent: self only.
// Peer: admitted — Hub proxy validated the original caller.
func (p Principal) CanDeleteOrReset(targetID string) bool {
	if p.IsOwner() || p.Role == RolePrivAgent || p.IsPeer() || p.IsOwnerDeputyOver(targetID) {
		return true
	}
	return p.IsAgent() && p.AgentID == targetID
}

// CanForkOrCreate returns true only for the Owner. It doubles as the
// owner-only gate for a few unrelated global routes (TTS/STT), so it
// deliberately did NOT grow a deputy case — the agent-scoped decisions
// live in CanCreateAgent / CanFork.
func (p Principal) CanForkOrCreate() bool {
	return p.IsOwner()
}

// CanCreateAgent gates POST /api/v1/agents: the Owner, and the agents
// the Owner deputised, may make new ones.
func (p Principal) CanCreateAgent() bool {
	return p.IsOwner() || p.IsOwnerDeputy()
}

// CanFork gates POST /api/v1/agents/{id}/fork. Forking copies the
// source's persona and memory, so it stays denied for a privileged
// agent — but a deputy already reads those in full and may create
// agents, so withholding fork would buy nothing. Self-fork is NOT
// covered (IsOwnerDeputyOver excludes self): a deputy cloning itself
// would hand its own memory to a second agent that the Owner never
// deputised.
func (p Principal) CanFork(targetID string) bool {
	return p.IsOwner() || p.IsOwnerDeputyOver(targetID)
}

// CanSetOwnerDeputy returns true only for the Owner. A deputy must
// never be able to mint another deputy, or promote itself — that would
// make the grant self-propagating.
func (p Principal) CanSetOwnerDeputy() bool {
	return p.IsOwner()
}

// CanSetPrivileged returns true only for the Owner. A privileged-agent
// must never be able to grant or revoke privilege.
func (p Principal) CanSetPrivileged() bool {
	return p.IsOwner()
}

// CanRestartServer returns true for principals allowed to trigger a
// daemon self-restart (POST /api/v1/system/restart): the Owner and
// privileged agents. Regular agents and peers are refused — a restart
// quiesces every agent on this host, not just the caller.
func (p Principal) CanRestartServer() bool {
	return p.IsOwner() || p.Role == RolePrivAgent
}

// Resolver maps a Bearer token to a Principal.
type Resolver struct {
	tokens        *TokenStore
	isPrivileged  func(agentID string) bool
	isOwnerDeputy func(agentID string) bool

	mu           sync.RWMutex
	extResolveFn ExtensionResolveFunc
}

// ExtensionResolveFunc maps a raw token to the installed extension that
// owns it. extpkg.Manager implements it; the server wires it in after
// both packages are constructed, which is why it is a setter rather
// than a NewResolver argument.
type ExtensionResolveFunc func(token string) (ExtensionIdentity, bool)

// ExtensionIdentity is what an extension token resolves to.
type ExtensionIdentity struct {
	ID         string
	Scopes     []string
	AgentScope []string
}

// NewResolver builds a Resolver from a TokenStore and the two per-agent
// grant predicates (agent.Manager.IsPrivileged / IsOwnerDeputy). A nil
// predicate reads as "nobody holds this grant".
func NewResolver(tokens *TokenStore, isPrivileged, isOwnerDeputy func(string) bool) *Resolver {
	if isPrivileged == nil {
		isPrivileged = func(string) bool { return false }
	}
	if isOwnerDeputy == nil {
		isOwnerDeputy = func(string) bool { return false }
	}
	return &Resolver{tokens: tokens, isPrivileged: isPrivileged, isOwnerDeputy: isOwnerDeputy}
}

// Resolve maps a Bearer token to a Principal. Empty/unknown tokens
// resolve to RoleGuest.
func (r *Resolver) Resolve(token string) Principal {
	if r == nil || r.tokens == nil || token == "" {
		return Principal{Role: RoleGuest}
	}
	// Owner: hash-only comparison; we don't need the raw to verify a
	// presented token. VerifyOwner is constant-time internally.
	if r.tokens.VerifyOwner(token) {
		return Principal{Role: RoleOwner}
	}
	if id, ok := r.tokens.LookupAgent(token); ok {
		deputy := r.isOwnerDeputy(id)
		if r.isPrivileged(id) {
			return Principal{Role: RolePrivAgent, AgentID: id, OwnerDeputy: deputy}
		}
		return Principal{Role: RoleAgent, AgentID: id, OwnerDeputy: deputy}
	}
	// Extension tokens are checked last: they are the weakest role, so
	// an ID collision with an agent token must never downgrade the
	// agent (and cannot upgrade the extension).
	r.mu.RLock()
	fn := r.extResolveFn
	r.mu.RUnlock()
	if fn != nil {
		if ident, ok := fn(token); ok && ident.ID != "" {
			return Principal{
				Role:        RoleExtension,
				ExtensionID: ident.ID,
				Scopes:      ident.Scopes,
				AgentScope:  ident.AgentScope,
			}
		}
	}
	return Principal{Role: RoleGuest}
}

// SetExtensionResolver installs (or clears, with nil) the lookup used
// to resolve extension tokens.
func (r *Resolver) SetExtensionResolver(fn ExtensionResolveFunc) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.extResolveFn = fn
	r.mu.Unlock()
}

// --- context plumbing ------------------------------------------------

type ctxKey struct{}

// WithPrincipal attaches a Principal to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext retrieves the Principal stashed in ctx by middleware.
// Defaults to RoleGuest when no principal is set.
func FromContext(ctx context.Context) Principal {
	if v, ok := ctx.Value(ctxKey{}).(Principal); ok {
		return v
	}
	return Principal{Role: RoleGuest}
}

// --- middleware ------------------------------------------------------

// OwnerOnlyMiddleware tags every request as the Owner UNLESS an
// earlier middleware (e.g. TailnetIdentityMiddleware) already
// attached a non-Guest principal. The exception keeps the
// "Tailscale reach == Owner" UX for the kojo user from clobbering
// a paired peer's Tailnet-identified RolePeer on the same listener.
//
// Used on the public (Tailscale) listener that the kojo user
// accesses from their phone — the user's UX is preserved (no
// token required) for everything except inter-peer requests that
// arrive pre-stamped.
func OwnerOnlyMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if existing := FromContext(r.Context()); existing.Role != RoleGuest {
			h.ServeHTTP(w, r)
			return
		}
		ctx := WithPrincipal(r.Context(), Principal{Role: RoleOwner})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthMiddleware resolves Authorization: Bearer / X-Kojo-Token to a
// Principal and passes it through ctx. It does NOT enforce per-route
// policy — that is the handler's responsibility (or a separate gate).
//
// Skips the Bearer resolution when an earlier middleware (e.g.
// TailnetIdentityMiddleware) already attached a non-Guest principal
// so a paired peer's Tailnet-identified RolePeer doesn't get
// downgraded to Guest by the absence of a Bearer.
func AuthMiddleware(resolver *Resolver) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if existing := FromContext(r.Context()); existing.Role != RoleGuest {
				h.ServeHTTP(w, r)
				return
			}
			tok := extractBearer(r)
			p := resolver.Resolve(tok)
			ctx := WithPrincipal(r.Context(), p)
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer reads the bearer token from `Authorization: Bearer X`,
// the `X-Kojo-Token` header, or — only for GET / HEAD requests — the
// `?token=` query parameter. The query-param fallback exists because
// the browser WebSocket API cannot set custom headers and `<img>` /
// `<a>` elements similarly drive their own GET requests; restricting
// it to safe verbs keeps a leaked URL from being replayed against
// state-changing endpoints (POST/PATCH/DELETE).
//
// Query-param tokens land in HTTP access logs. That is acceptable for
// the spoiler-prevention threat model, and the UI strips the param
// from window.location after consuming it on first load.
func extractBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	if h := r.Header.Get("X-Kojo-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, "":
		if t := r.URL.Query().Get("token"); t != "" {
			return strings.TrimSpace(t)
		}
	}
	return ""
}
