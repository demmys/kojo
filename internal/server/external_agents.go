package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// External agents are AI agents that run outside kojo on another
// tailnet node and take part in group DMs. They never touch the Owner
// listener or the loopback agent listener: they reach a dedicated tsnet
// listener (cmd/kojo --external-port) whose mux carries only the /v1/*
// external API. Every call there needs both a Bearer token and a WhoIs
// match on the tailnet node the token was paired from.

const (
	externalPairingTTL     = 10 * time.Minute
	externalPairAttempts   = 5
	externalPairWindow     = 10 * time.Minute
	externalTouchInterval  = time.Minute
	externalTokenPrefix    = "kxa_"
	externalIDPrefix       = "xa_"
	externalNameMaxBytes   = 64
	externalDescMaxBytes   = 1024
	externalWhoIsTimeout   = 3 * time.Second
	externalMaxRatePerMin  = 600
	externalDefaultRatePer = 20
)

// ExternalWhoIsFunc maps a caller's RemoteAddr to its tailnet node.
type ExternalWhoIsFunc func(ctx context.Context, remoteAddr string) (store.ExternalNodeIdentity, error)

type externalState struct {
	mu      sync.RWMutex
	whois   ExternalWhoIsFunc
	baseURL string // e.g. http://kojo.tailnet.ts.net:8082, "" while disabled
	srv     *http.Server

	refreshMu sync.Mutex
	denyMu    sync.RWMutex
	deniedIPs map[netip.Addr]bool

	pairMu       sync.Mutex
	pairAttempts map[string][]time.Time

	touchMu   sync.Mutex
	lastTouch map[string]time.Time

	// public-listener connections, by remote IP, so pairing a node can
	// cut the Owner sessions it already holds.
	connMu sync.Mutex
	conns  map[*trackedConn]netip.Addr

	hub *externalHub

	cache externalCache

	// credMu: credential changes hold it exclusively; posts hold it
	// shared across validate + append.
	credMu sync.RWMutex

	rateMu  sync.Mutex
	buckets map[string]*externalBucket
}

func newExternalState() *externalState {
	return &externalState{
		deniedIPs:    map[netip.Addr]bool{},
		pairAttempts: map[string][]time.Time{},
		lastTouch:    map[string]time.Time{},
		conns:        map[*trackedConn]netip.Addr{},
		hub:          newExternalHub(),
		buckets:      map[string]*externalBucket{},
	}
}

func remoteIP(remoteAddr string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// SetExternalWhoIs wires the tsnet WhoIs used by the external listener.
func (s *Server) SetExternalWhoIs(fn ExternalWhoIsFunc) {
	s.external.mu.Lock()
	s.external.whois = fn
	s.external.mu.Unlock()
}

// SetExternalBaseURL records the external listener's dial URL (shown
// with a pairing code).
func (s *Server) SetExternalBaseURL(u string) {
	s.external.mu.Lock()
	s.external.baseURL = u
	s.external.mu.Unlock()
}

func (s *Server) externalBaseURL() string {
	s.external.mu.RLock()
	defer s.external.mu.RUnlock()
	return s.external.baseURL
}

func (s *Server) externalWhoIs(ctx context.Context, remoteAddr string) (store.ExternalNodeIdentity, error) {
	s.external.mu.RLock()
	fn := s.external.whois
	s.external.mu.RUnlock()
	if fn == nil {
		return store.ExternalNodeIdentity{}, errors.New("external whois not wired")
	}
	ctx, cancel := context.WithTimeout(ctx, externalWhoIsTimeout)
	defer cancel()
	id, err := fn(ctx, remoteAddr)
	if err != nil {
		return id, err
	}
	if id.StableNodeID == "" {
		return id, errors.New("whois returned no node")
	}
	return id, nil
}

// refreshExternalDeny reloads the in-memory denylist from the store.
func (s *Server) refreshExternalDeny(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	// Load+apply under one lock so a stale snapshot can never overwrite
	// a newer one.
	s.external.refreshMu.Lock()
	defer s.external.refreshMu.Unlock()
	st := s.externalStore()
	if st == nil {
		return
	}
	rows, err := st.ListExternalNodeDenylist(ctx)
	if err != nil {
		s.logger.Warn("external denylist load failed", "err", err)
		return
	}
	m := map[netip.Addr]bool{}
	for _, r := range rows {
		for _, ip := range r.TailnetIPs {
			if a, ok := remoteIP(ip); ok {
				m[a] = true
			}
		}
	}
	s.external.denyMu.Lock()
	s.external.deniedIPs = m
	s.external.denyMu.Unlock()
}

// denyIPsNow adds ips to the in-memory denylist immediately (fail
// closed: used before/independently of the store reload).
func (s *Server) denyIPsNow(ips []string) {
	s.external.denyMu.Lock()
	for _, ip := range ips {
		if a, ok := remoteIP(ip); ok {
			s.external.deniedIPs[a] = true
		}
	}
	s.external.denyMu.Unlock()
}

// externalDeniedAddr is the public listener's DenyAddr hook.
func (s *Server) externalDeniedAddr(remoteAddr string) bool {
	a, ok := remoteIP(remoteAddr)
	if !ok {
		return false
	}
	s.external.denyMu.RLock()
	defer s.external.denyMu.RUnlock()
	return s.external.deniedIPs[a]
}

// TrackPublicListener wraps the RAW (pre-TLS) public tsnet listener so
// every accepted conn (including hijacked websockets) is tracked until
// closed, letting pairing cut the Owner sessions a node already holds.
// It must sit inside tls.NewListener: wrapping a *tls.Conn would hide
// it from net/http and break ALPN h2.
func (s *Server) TrackPublicListener(ln net.Listener) net.Listener {
	if s.external == nil {
		return ln
	}
	return &trackingListener{Listener: ln, s: s.external}
}

type trackingListener struct {
	net.Listener
	s *externalState
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	a, ok := remoteIP(c.RemoteAddr().String())
	if !ok {
		return c, nil
	}
	tc := &trackedConn{Conn: c, s: l.s}
	l.s.connMu.Lock()
	l.s.conns[tc] = a
	l.s.connMu.Unlock()
	return tc, nil
}

type trackedConn struct {
	net.Conn
	s    *externalState
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.s.connMu.Lock()
		delete(c.s.conns, c)
		c.s.connMu.Unlock()
	})
	return c.Conn.Close()
}

// closePublicConnsFrom cuts every public-listener connection from ips.
func (s *Server) closePublicConnsFrom(ips []string) {
	want := map[netip.Addr]bool{}
	for _, ip := range ips {
		if a, ok := remoteIP(ip); ok {
			want[a] = true
		}
	}
	s.external.connMu.Lock()
	var victims []*trackedConn
	for c, a := range s.external.conns {
		if want[a] {
			victims = append(victims, c)
			delete(s.external.conns, c)
		}
	}
	s.external.connMu.Unlock()
	for _, c := range victims {
		_ = c.Close()
	}
}

func (s *Server) externalStore() *store.Store {
	if s.agents == nil {
		return nil
	}
	return s.agents.Store()
}

func hashExternalSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newExternalToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return externalTokenPrefix + hex.EncodeToString(b), nil
}

func newExternalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return externalIDPrefix + hex.EncodeToString(b)
}

// newPairingCode returns a human-typable code "ABCD-EFGH-IJKL" (60 bits).
func newPairingCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:12]
	return enc[:4] + "-" + enc[4:8] + "-" + enc[8:12], nil
}

// normalizePairingCode strips separators/case so "abcd efgh-ijkl" works.
func normalizePairingCode(c string) string {
	c = strings.ToUpper(c)
	var b strings.Builder
	for _, r := range c {
		if (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// allowPairAttempt rate-limits pairing per tailnet node.
func (s *Server) allowPairAttempt(node string) bool {
	now := time.Now()
	s.external.pairMu.Lock()
	defer s.external.pairMu.Unlock()
	kept := s.external.pairAttempts[node][:0]
	for _, t := range s.external.pairAttempts[node] {
		if now.Sub(t) < externalPairWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= externalPairAttempts {
		s.external.pairAttempts[node] = kept
		return false
	}
	s.external.pairAttempts[node] = append(kept, now)
	return true
}

// ---------------------------------------------------------------------
// Owner API (/api/v1/external-agents, /api/v1/external-nodes)
// ---------------------------------------------------------------------

type externalAgentView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Paired           bool   `json:"paired"`
	HasToken         bool   `json:"hasToken"`
	NodeName         string `json:"nodeName,omitempty"`
	Enabled          bool   `json:"enabled"`
	RatePerMin       int    `json:"ratePerMin"`
	Online           bool   `json:"online"`
	PairingExpiresAt int64  `json:"pairingExpiresAt,omitempty"`
	LastSeenAt       int64  `json:"lastSeenAt,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

func (s *Server) externalView(r *store.ExternalAgentRecord) externalAgentView {
	v := externalAgentView{
		ID: r.ID, Name: r.Name, Description: r.Description,
		Paired: r.StableNodeID != "", HasToken: r.TokenHash != "",
		NodeName: r.NodeName, Enabled: r.Enabled, RatePerMin: r.RatePerMin,
		Online:     s.external.hub.online(r.ID),
		LastSeenAt: r.LastSeenAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.PairingHash != "" && r.PairingExpires > store.NowMillis() {
		v.PairingExpiresAt = r.PairingExpires
	}
	return v
}

func validateExternalName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > externalNameMaxBytes || strings.ContainsAny(name, "\r\n\t@") {
		return "", false
	}
	return name, true
}

func (s *Server) handleListExternalAgents(w http.ResponseWriter, r *http.Request) {
	st, ok := s.requireAgentStore(w, "store unavailable")
	if !ok {
		return
	}
	rows, err := st.ListExternalAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]externalAgentView, 0, len(rows))
	for _, rec := range rows {
		out = append(out, s.externalView(rec))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"externalAgents": out,
		"listenerUrl":    s.externalBaseURL(),
	})
}

func (s *Server) handleCreateExternalAgent(w http.ResponseWriter, r *http.Request) {
	st, ok := s.requireAgentStore(w, "store unavailable")
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		RatePerMin  int    `json:"ratePerMin"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name, ok := validateExternalName(req.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "name must be 1-64 bytes without newlines, tabs or '@'")
		return
	}
	if len(req.Description) > externalDescMaxBytes {
		writeError(w, http.StatusBadRequest, "invalid_request", "description too long")
		return
	}
	if req.RatePerMin < 0 || req.RatePerMin > externalMaxRatePerMin {
		writeError(w, http.StatusBadRequest, "invalid_request", "ratePerMin out of range")
		return
	}
	if req.RatePerMin == 0 {
		req.RatePerMin = externalDefaultRatePer
	}
	rec, err := st.CreateExternalAgent(r.Context(), newExternalID(), name, strings.TrimSpace(req.Description), req.RatePerMin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.onExternalAgentChanged(rec.ID)
	writeJSONResponse(w, http.StatusCreated, s.externalView(rec))
}

func (s *Server) externalByPath(w http.ResponseWriter, r *http.Request) (*store.Store, *store.ExternalAgentRecord, bool) {
	st, ok := s.requireAgentStore(w, "store unavailable")
	if !ok {
		return nil, nil, false
	}
	rec, err := st.GetExternalAgent(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "external agent not found")
		return nil, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, nil, false
	}
	return st, rec, true
}

func (s *Server) handleGetExternalAgent(w http.ResponseWriter, r *http.Request) {
	_, rec, ok := s.externalByPath(w, r)
	if !ok {
		return
	}
	writeJSONResponse(w, http.StatusOK, s.externalView(rec))
}

func (s *Server) handlePatchExternalAgent(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.externalByPath(w, r)
	if !ok {
		return
	}
	s.external.credMu.Lock()
	defer s.external.credMu.Unlock()
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Enabled     *bool   `json:"enabled"`
		RatePerMin  *int    `json:"ratePerMin"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var p store.ExternalAgentPatch
	if req.Name != nil {
		name, ok := validateExternalName(*req.Name)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "name must be 1-64 bytes without newlines, tabs or '@'")
			return
		}
		p.Name = &name
	}
	if req.Description != nil {
		d := strings.TrimSpace(*req.Description)
		if len(d) > externalDescMaxBytes {
			writeError(w, http.StatusBadRequest, "invalid_request", "description too long")
			return
		}
		p.Description = &d
	}
	if req.RatePerMin != nil && (*req.RatePerMin <= 0 || *req.RatePerMin > externalMaxRatePerMin) {
		writeError(w, http.StatusBadRequest, "invalid_request", "ratePerMin out of range")
		return
	}
	p.Enabled = req.Enabled
	p.RatePerMin = req.RatePerMin
	out, err := st.UpdateExternalAgent(r.Context(), rec.ID, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.onExternalAgentChanged(out.ID)
	if !out.Enabled {
		s.external.hub.disconnect(out.ID, "disabled")
	}
	writeJSONResponse(w, http.StatusOK, s.externalView(out))
}

func (s *Server) handleDeleteExternalAgent(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.externalByPath(w, r)
	if !ok {
		return
	}
	s.external.credMu.Lock()
	defer s.external.credMu.Unlock()
	if err := st.SoftDeleteExternalAgent(r.Context(), rec.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.onExternalAgentDeleted(rec.ID)
	s.external.hub.disconnect(rec.ID, "deleted")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExternalPairingCode(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.externalByPath(w, r)
	if !ok {
		return
	}
	code, err := newPairingCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	expires := time.Now().Add(externalPairingTTL).UnixMilli()
	if err := st.SetExternalPairing(r.Context(), rec.ID, hashExternalSecret(normalizePairingCode(code)), expires); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp := map[string]any{"code": code, "expiresAt": expires}
	if base := s.externalBaseURL(); base != "" {
		resp["pairUrl"] = base + "/v1/pair"
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

func (s *Server) handleRevokeExternalAgent(w http.ResponseWriter, r *http.Request) {
	st, rec, ok := s.externalByPath(w, r)
	if !ok {
		return
	}
	s.external.credMu.Lock()
	defer s.external.credMu.Unlock()
	out, err := st.RevokeExternalToken(r.Context(), rec.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.onExternalAgentChanged(rec.ID)
	s.external.hub.disconnect(rec.ID, "revoked")
	writeJSONResponse(w, http.StatusOK, s.externalView(out))
}

func (s *Server) handleListExternalNodes(w http.ResponseWriter, r *http.Request) {
	st, ok := s.requireAgentStore(w, "store unavailable")
	if !ok {
		return
	}
	rows, err := st.ListExternalNodeDenylist(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	type view struct {
		StableNodeID string   `json:"stableNodeId"`
		NodeName     string   `json:"nodeName"`
		TailnetIPs   []string `json:"tailnetIps"`
		ExternalID   string   `json:"externalId"`
		CreatedAt    int64    `json:"createdAt"`
		UpdatedAt    int64    `json:"updatedAt"`
	}
	out := make([]view, 0, len(rows))
	for _, d := range rows {
		ips := d.TailnetIPs
		if ips == nil {
			ips = []string{}
		}
		out = append(out, view{d.StableNodeID, d.NodeName, ips, d.ExternalID, d.CreatedAt, d.UpdatedAt})
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"nodes": out})
}

func (s *Server) handleDeleteExternalNode(w http.ResponseWriter, r *http.Request) {
	st, ok := s.requireAgentStore(w, "store unavailable")
	if !ok {
		return
	}
	err := st.DeleteExternalNodeDeny(r.Context(), r.PathValue("node"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "node not in denylist")
		return
	case errors.Is(err, store.ErrExternalNodeBound):
		writeError(w, http.StatusConflict, "node_bound", "an external agent is still bound to this node; delete it first")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.refreshExternalDeny(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------
// External listener
// ---------------------------------------------------------------------

type externalCallerKey struct{}

type externalCaller struct {
	rec  *store.ExternalAgentRecord
	node store.ExternalNodeIdentity
}

func externalCallerFrom(ctx context.Context) *externalCaller {
	c, _ := ctx.Value(externalCallerKey{}).(*externalCaller)
	return c
}

// ExternalHandler is the handler for the dedicated external listener.
func (s *Server) ExternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair", s.handleExternalPair)
	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/me", s.handleExternalMe)
	s.registerExternalGroupRoutes(authed)
	mux.Handle("/v1/", s.externalAuth(authed))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
	})
	return apiNoStoreDefaultMiddleware(mux)
}

// ServeExternal serves the external listener until Shutdown.
func (s *Server) ServeExternal(ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.ExternalHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          httpServerErrorLog(s.logger),
	}
	s.external.mu.Lock()
	s.external.srv = srv
	s.external.mu.Unlock()
	s.logger.Info("external agent listener started", "addr", ln.Addr().String())
	return srv.Serve(ln)
}

func (s *Server) shutdownExternal(ctx context.Context) {
	if s.external == nil {
		return
	}
	s.external.hub.shutdown()
	s.external.mu.RLock()
	srv := s.external.srv
	s.external.mu.RUnlock()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			s.logger.Warn("external listener shutdown error", "err", err)
		}
	}
}

func (s *Server) handleExternalPair(w http.ResponseWriter, r *http.Request) {
	st := s.externalStore()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store unavailable")
		return
	}
	node, err := s.externalWhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "whois_failed", "could not identify the calling tailnet node; retry")
		return
	}
	if !s.allowPairAttempt(node.StableNodeID) {
		w.Header().Set("Retry-After", "600")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many pairing attempts")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	code := normalizePairingCode(req.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_code", "pairing code invalid or expired")
		return
	}
	token, err := newExternalToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Hold credMu across redeem + cache reload + disconnect so no post
	// validated against the old token can land afterwards.
	s.external.credMu.Lock()
	rec, err := st.RedeemExternalPairing(r.Context(), hashExternalSecret(code), node, hashExternalSecret(token))
	if err == nil {
		s.onExternalAgentChanged(rec.ID)
		s.external.hub.disconnect(rec.ID, "re-paired")
	} else if !errors.Is(err, store.ErrPairingInvalid) && !errors.Is(err, store.ErrExternalNodeBound) {
		s.reloadExternalCache(r.Context())
	}
	s.external.credMu.Unlock()
	switch {
	case errors.Is(err, store.ErrPairingInvalid):
		writeError(w, http.StatusBadRequest, "invalid_code", "pairing code invalid or expired")
		return
	case errors.Is(err, store.ErrExternalNodeBound):
		writeError(w, http.StatusConflict, "node_bound", "this tailnet node is already paired to another external agent")
		return
	case err != nil:
		// The commit may have landed; fail closed on the public side.
		s.denyIPsNow(node.TailnetIPs)
		s.closePublicConnsFrom(node.TailnetIPs)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.denyIPsNow(node.TailnetIPs)
	s.refreshExternalDeny(r.Context())
	s.closePublicConnsFrom(node.TailnetIPs)
	s.logger.Info("external agent paired", "id", rec.ID, "node", node.NodeName)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"id": rec.ID, "name": rec.Name, "token": token,
	})
}

// externalAuth authenticates every /v1/* call except pairing.
func (s *Server) externalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.externalStore()
		if st == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "store unavailable")
			return
		}
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(raw, externalTokenPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bearer token required")
			return
		}
		node, err := s.externalWhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "whois_failed", "could not identify the calling tailnet node; retry")
			return
		}
		rec, err := st.LookupExternalAgentByToken(r.Context(), hashExternalSecret(strings.TrimSpace(raw)))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(rec.StableNodeID), []byte(node.StableNodeID)) != 1 {
			s.logger.Warn("external token presented from a different tailnet node",
				"id", rec.ID, "node", node.NodeName)
			writeError(w, http.StatusForbidden, "forbidden", "token is bound to a different tailnet node")
			return
		}
		if !rec.Enabled {
			writeError(w, http.StatusForbidden, "disabled", "external agent is disabled")
			return
		}
		s.touchExternal(r.Context(), rec.ID, node)
		ctx := context.WithValue(r.Context(), externalCallerKey{}, &externalCaller{rec: rec, node: node})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// touchExternal records last_seen and the node's IPs (throttled). A new
// IP is pushed into the public-listener denylist immediately.
func (s *Server) touchExternal(ctx context.Context, id string, node store.ExternalNodeIdentity) {
	newIP := false
	for _, ip := range node.TailnetIPs {
		if a, ok := remoteIP(ip); ok && !s.externalDeniedAddr(a.String()) {
			newIP = true
		}
	}
	s.external.touchMu.Lock()
	last := s.external.lastTouch[id]
	due := newIP || time.Since(last) >= externalTouchInterval
	if due {
		s.external.lastTouch[id] = time.Now()
	}
	s.external.touchMu.Unlock()
	if !due {
		return
	}
	st := s.externalStore()
	if st == nil {
		return
	}
	if err := st.TouchExternalAgent(ctx, id, node); err != nil {
		s.logger.Warn("external touch failed", "id", id, "err", err)
		return
	}
	if newIP {
		s.denyIPsNow(node.TailnetIPs)
		s.refreshExternalDeny(ctx)
		s.closePublicConnsFrom(node.TailnetIPs)
	}
}

func (s *Server) handleExternalMe(w http.ResponseWriter, r *http.Request) {
	c := externalCallerFrom(r.Context())
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"id": c.rec.ID, "name": c.rec.Name, "description": c.rec.Description,
		"nodeName": c.node.NodeName,
	})
}
