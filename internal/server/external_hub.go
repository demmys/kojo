package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	externalStreamBuffer   = 64
	externalPingInterval   = 25 * time.Second
	externalWriteTimeout   = 10 * time.Second
	externalMaxContent     = 32 << 10
	externalDefaultLimit   = 50
	externalMaxLimit       = 200
	externalDefaultWait    = 30 * time.Second
	externalMaxWait        = 60 * time.Second
	externalStreamDropNote = "overflow"
)

// externalHub fans group-DM events out to live external-agent streams
// (one per agent; a new connection replaces the old) and wakes
// long-poll waiters.
type externalHub struct {
	mu      sync.Mutex
	streams map[string]*externalStream
	waiters map[string]map[chan struct{}]struct{}
	// evlog keeps recent non-message events per agent so long-poll
	// clients see membership / room changes (in-memory; a restart
	// resets it and clients resync via GET /v1/groups).
	evlog map[string]*externalEventLog
	// seqBase starts every agent's event seq at a per-boot value
	// (unix micros), so a cursor from a previous process is always
	// detected as a gap instead of silently aliasing new events.
	seqBase int64
	closed  bool
}

const externalEventLogCap = 200

type externalLoggedEvent struct {
	Seq int64 `json:"seq"`
	agent.ExternalEvent
}

type externalEventLog struct {
	seq   int64
	items []externalLoggedEvent
}

type externalStream struct {
	ch     chan agent.ExternalEvent
	done   chan struct{}
	once   sync.Once
	reason string
}

func (st *externalStream) close(reason string) {
	st.once.Do(func() { st.reason = reason; close(st.done) })
}

func newExternalHub() *externalHub {
	return &externalHub{
		streams: map[string]*externalStream{},
		waiters: map[string]map[chan struct{}]struct{}{},
		evlog:   map[string]*externalEventLog{},
		seqBase: time.Now().UnixMicro(),
	}
}

func (h *externalHub) online(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[id] != nil
}

// subscribe registers a new stream for id, closing any previous one.
func (h *externalHub) subscribe(id string) (*externalStream, bool) {
	st := &externalStream{ch: make(chan agent.ExternalEvent, externalStreamBuffer), done: make(chan struct{})}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, false
	}
	old := h.streams[id]
	h.streams[id] = st
	h.mu.Unlock()
	if old != nil {
		old.close("replaced")
	}
	return st, true
}

func (h *externalHub) unsubscribe(id string, st *externalStream) {
	h.mu.Lock()
	if h.streams[id] == st {
		delete(h.streams, id)
	}
	h.mu.Unlock()
	st.close("closed")
}

// publish never blocks: a full buffer kills the stream (the client
// reconnects and resyncs from the hello snapshot).
func (h *externalHub) publish(id string, ev agent.ExternalEvent) {
	h.mu.Lock()
	st := h.streams[id]
	ws := h.waiters[id]
	if ev.Type != agent.ExternalEventMessage {
		l := h.evlog[id]
		if l == nil {
			l = &externalEventLog{seq: h.seqBase}
			h.evlog[id] = l
		}
		l.seq++
		l.items = append(l.items, externalLoggedEvent{Seq: l.seq, ExternalEvent: ev})
		if len(l.items) > externalEventLogCap {
			l.items = l.items[len(l.items)-externalEventLogCap:]
		}
	}
	for c := range ws {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	if st != nil {
		select {
		case st.ch <- ev:
		default:
			delete(h.streams, id)
			st.close(externalStreamDropNote)
		}
	}
	h.mu.Unlock()
}

// eventsSince returns logged events after since, the current event seq
// and whether the client's position is no longer covered (it must
// resync from GET /v1/groups).
func (h *externalHub) eventsSince(id string, since int64) ([]externalLoggedEvent, int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.evlog[id]
	if l == nil {
		return nil, h.seqBase, since > 0 && since != h.seqBase
	}
	if since > l.seq || (since > 0 && since < h.seqBase) {
		return nil, l.seq, true
	}
	var out []externalLoggedEvent
	for _, it := range l.items {
		if it.Seq > since {
			out = append(out, it)
		}
	}
	from := since
	if from == 0 {
		from = h.seqBase
	}
	gap := len(l.items) > 0 && l.items[0].Seq > from+1
	return out, l.seq, gap
}

func (h *externalHub) forget(id string) {
	h.mu.Lock()
	delete(h.evlog, id)
	h.mu.Unlock()
}

func (h *externalHub) addWaiter(id string) chan struct{} {
	c := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters[id] == nil {
		h.waiters[id] = map[chan struct{}]struct{}{}
	}
	h.waiters[id][c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *externalHub) removeWaiter(id string, c chan struct{}) {
	h.mu.Lock()
	delete(h.waiters[id], c)
	if len(h.waiters[id]) == 0 {
		delete(h.waiters, id)
	}
	h.mu.Unlock()
}

// disconnect closes id's stream and wakes its waiters (they re-check
// auth state and return).
func (h *externalHub) disconnect(id, reason string) {
	h.mu.Lock()
	st := h.streams[id]
	delete(h.streams, id)
	for c := range h.waiters[id] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
	if st != nil {
		st.close(reason)
	}
}

func (h *externalHub) shutdown() {
	h.mu.Lock()
	h.closed = true
	all := h.streams
	h.streams = map[string]*externalStream{}
	for _, ws := range h.waiters {
		for c := range ws {
			select {
			case c <- struct{}{}:
			default:
			}
		}
	}
	h.mu.Unlock()
	for _, st := range all {
		st.close("shutdown")
	}
}

func (h *externalHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// ---------------------------------------------------------------------
// agent.ExternalBridge implementation (in-memory caches; never calls
// back into GroupDMManager).

type externalCache struct {
	reloadMu sync.Mutex // serializes read-from-DB + swap
	mu       sync.RWMutex
	recs     map[string]*store.ExternalAgentRecord
	trust    map[string]string
}

func (s *Server) reloadExternalCache(ctx context.Context) {
	st := s.externalStore()
	if st == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	s.external.cache.reloadMu.Lock()
	defer s.external.cache.reloadMu.Unlock()
	recs, err := st.ListExternalAgents(ctx)
	if err != nil {
		s.logger.Warn("external cache reload failed", "err", err)
		return
	}
	trust, err := st.ListGroupDMExternalTrust(ctx)
	if err != nil {
		s.logger.Warn("external trust reload failed", "err", err)
		return
	}
	m := make(map[string]*store.ExternalAgentRecord, len(recs))
	for _, r := range recs {
		m[r.ID] = r
	}
	c := &s.external.cache
	c.mu.Lock()
	c.recs = m
	c.trust = trust
	c.mu.Unlock()
}

func (s *Server) cachedExternal(id string) (*store.ExternalAgentRecord, bool) {
	c := &s.external.cache
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.recs[id]
	return r, ok
}

type externalBridge struct{ s *Server }

func (b externalBridge) LookupExternal(id string) (agent.ExternalInfo, bool) {
	r, ok := b.s.cachedExternal(id)
	if !ok {
		return agent.ExternalInfo{}, false
	}
	return agent.ExternalInfo{ID: r.ID, Name: r.Name, Description: r.Description, Enabled: r.Enabled}, true
}

func (b externalBridge) PublishExternal(id string, ev agent.ExternalEvent) {
	b.s.external.hub.publish(id, ev)
}

func (b externalBridge) ExternalTrust(groupID string) string {
	c := &b.s.external.cache
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.trust[groupID]
}

func (s *Server) initExternalBridge() {
	s.reloadExternalCache(context.Background())
	if s.groupdms != nil {
		s.groupdms.SetExternalBridge(externalBridge{s: s})
	}
}

func (s *Server) onExternalAgentChanged(id string) {
	s.reloadExternalCache(context.Background())
}

func (s *Server) onExternalAgentDeleted(id string) {
	s.reloadExternalCache(context.Background())
	if s.groupdms != nil {
		s.groupdms.RemoveExternal(id)
	}
	s.external.hub.forget(id)
	if st := s.externalStore(); st != nil {
		if _, err := st.DB().ExecContext(context.Background(),
			`DELETE FROM external_agent_cursors WHERE external_id = ?`, id); err != nil {
			s.logger.Warn("external cursor cleanup failed", "id", id, "err", err)
		}
	}
	s.external.rateMu.Lock()
	delete(s.external.buckets, id)
	s.external.rateMu.Unlock()
}

// callerStillValid re-checks a long-lived connection's credentials
// against the cache (disable / delete / revoke / re-pair).
func (s *Server) callerStillValid(c *externalCaller) bool {
	r, ok := s.cachedExternal(c.rec.ID)
	return ok && r.Enabled && r.TokenGen == c.rec.TokenGen && r.StableNodeID == c.rec.StableNodeID
}

// ---------------------------------------------------------------------
// Rate limiting: token bucket of RatePerMin per agent.

type externalBucket struct {
	tokens float64
	last   time.Time
}

func (s *Server) takeExternalToken(id string, perMin int) (bool, time.Duration) {
	if perMin <= 0 {
		perMin = externalDefaultRatePer
	}
	capacity := float64(perMin)
	rate := capacity / 60 // tokens per second
	now := time.Now()
	s.external.rateMu.Lock()
	defer s.external.rateMu.Unlock()
	b := s.external.buckets[id]
	if b == nil {
		b = &externalBucket{tokens: capacity, last: now}
		s.external.buckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return false, wait
}

// ---------------------------------------------------------------------
// /v1 group routes

func (s *Server) registerExternalGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/groups", s.handleExternalListGroups)
	mux.HandleFunc("GET /v1/groups/{id}", s.handleExternalGetGroup)
	mux.HandleFunc("GET /v1/groups/{id}/messages", s.handleExternalListMessages)
	mux.HandleFunc("POST /v1/groups/{id}/messages", s.handleExternalPostMessage)
	mux.HandleFunc("POST /v1/groups/{id}/ack", s.handleExternalAck)
	mux.HandleFunc("GET /v1/events", s.handleExternalEvents)
	mux.HandleFunc("GET /v1/updates", s.handleExternalUpdates)
}

type externalMemberView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
}

type externalGroupView struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Members         []externalMemberView `json:"members"`
	Muted           bool                 `json:"muted"`
	Trust           string               `json:"trust"`
	LatestMessageID string               `json:"latestMessageId"`
	LatestSeq       int64                `json:"latestSeq"`
	AckedSeq        int64                `json:"ackedSeq"`
}

func (s *Server) externalGroupView(g *agent.GroupDM, selfID string, cursors map[string]int64) externalGroupView {
	v := externalGroupView{ID: g.ID, Name: g.Name, Trust: s.groupdms.ExternalTrust(g.ID), AckedSeq: cursors[g.ID]}
	v.Members = append(v.Members, externalMemberView{ID: agent.UserSenderID, Name: "user", Kind: "user"})
	for _, m := range g.Members {
		kind := "agent"
		if agent.IsExternalID(m.AgentID) {
			kind = agent.MemberKindExternal
		}
		v.Members = append(v.Members, externalMemberView{ID: m.AgentID, Name: m.AgentName, Kind: kind, Description: m.Description})
		if m.AgentID == selfID && m.NotifyMode == agent.NotifyMuted {
			v.Muted = true
		}
	}
	if id, seq, err := s.groupdms.ExternalLatest(g.ID); err == nil {
		v.LatestMessageID, v.LatestSeq = id, seq
	}
	return v
}

// externalGroups returns the caller's rooms (group kind only).
func (s *Server) externalGroups(selfID string) []*agent.GroupDM {
	if s.groupdms == nil {
		return nil
	}
	return s.groupdms.GroupsForAgent(selfID)
}

func (s *Server) externalCursors(ctx context.Context, id string) map[string]int64 {
	st := s.externalStore()
	if st == nil {
		return map[string]int64{}
	}
	c, err := st.GetExternalCursors(ctx, id)
	if err != nil {
		s.logger.Warn("external cursors read failed", "id", id, "err", err)
		return map[string]int64{}
	}
	return c
}

// externalMember resolves {id} and confirms the caller is a member.
func (s *Server) externalMember(w http.ResponseWriter, r *http.Request) (*externalCaller, *agent.GroupDM, bool) {
	c := externalCallerFrom(r.Context())
	if s.groupdms == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "group DMs unavailable")
		return nil, nil, false
	}
	id := r.PathValue("id")
	g, ok := s.groupdms.Get(id)
	if !ok || s.groupdms.CheckMembership(id, c.rec.ID) != nil {
		writeError(w, http.StatusNotFound, "not_found", "group not found")
		return nil, nil, false
	}
	return c, g, true
}

func (s *Server) handleExternalListGroups(w http.ResponseWriter, r *http.Request) {
	c := externalCallerFrom(r.Context())
	cursors := s.externalCursors(r.Context(), c.rec.ID)
	out := []externalGroupView{}
	for _, g := range s.externalGroups(c.rec.ID) {
		out = append(out, s.externalGroupView(g, c.rec.ID, cursors))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"groups": out})
}

func (s *Server) handleExternalGetGroup(w http.ResponseWriter, r *http.Request) {
	c, g, ok := s.externalMember(w, r)
	if !ok {
		return
	}
	writeJSONResponse(w, http.StatusOK, s.externalGroupView(g, c.rec.ID, s.externalCursors(r.Context(), c.rec.ID)))
}

func externalLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return externalDefaultLimit
	}
	if n > externalMaxLimit {
		return externalMaxLimit
	}
	return n
}

func (s *Server) handleExternalListMessages(w http.ResponseWriter, r *http.Request) {
	_, g, ok := s.externalMember(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after, before := q.Get("after"), q.Get("before")
	limit := externalLimit(r)
	if after != "" && before != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "use either after or before")
		return
	}
	var msgs []*agent.GroupMessage
	var more bool
	if after != "" || q.Has("after") {
		var seq int64
		if after != "" {
			sq, found, err := s.groupdms.ExternalMessageSeq(g.ID, after)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if !found {
				writeError(w, http.StatusBadRequest, "unknown_message", "after: message not found in this group")
				return
			}
			seq = sq
		}
		m, _, mr, err := s.groupdms.ExternalMessagesAfter(g.ID, seq, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		msgs, more = m, mr
	} else {
		m, mr, _, err := s.groupdms.Messages(g.ID, limit, before)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		msgs, more = m, mr
	}
	if msgs == nil {
		msgs = []*agent.GroupMessage{}
	}
	latest, _, _ := s.groupdms.ExternalLatest(g.ID)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"messages": msgs, "hasMore": more, "latestMessageId": latest,
	})
}

func (s *Server) handleExternalPostMessage(w http.ResponseWriter, r *http.Request) {
	c, g, ok := s.externalMember(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, externalMaxContent+4096)
	var req struct {
		Content          string `json:"content"`
		ExpectedLatestID string `json:"expectedLatestId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "content is required")
		return
	}
	if len(req.Content) > externalMaxContent {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "content exceeds 32 KiB")
		return
	}
	if okTok, wait := s.takeExternalToken(c.rec.ID, c.rec.RatePerMin); !okTok {
		secs := int(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeJSONResponse(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate_limited", "message": "rate limit exceeded", "retryAfter": secs,
		})
		return
	}
	// Credential changes (revoke / re-pair / disable / delete) take
	// credMu exclusively, so a post validated here cannot land after
	// the change completes.
	s.external.credMu.RLock()
	if !s.callerStillValid(c) {
		s.external.credMu.RUnlock()
		writeError(w, http.StatusUnauthorized, "unauthorized", "credentials no longer valid")
		return
	}
	msg, err := s.groupdms.PostExternalMessage(r.Context(), g.ID, c.rec.ID, req.Content, req.ExpectedLatestID)
	s.external.credMu.RUnlock()
	if err != nil {
		var missingErr *agent.MissingExpectedIDError
		var staleErr *agent.StaleExpectedIDError
		switch {
		case errors.As(err, &missingErr):
			writeJSONResponse(w, http.StatusConflict, map[string]any{
				"error":           "expected_latest_id_required",
				"message":         "expectedLatestId is required; read the room first and repost with the returned latestMessageId",
				"latestMessageId": missingErr.Latest,
			})
		case errors.As(err, &staleErr):
			newMsgs := staleErr.NewMessages
			if newMsgs == nil {
				newMsgs = []*agent.GroupMessage{}
			}
			writeJSONResponse(w, http.StatusConflict, map[string]any{
				"error":           "stale_expected_id",
				"message":         staleErr.Error(),
				"latestMessageId": staleErr.Latest,
				"newMessages":     newMsgs,
				"hasMore":         staleErr.HasMore,
			})
		case errors.Is(err, agent.ErrExternalHopLimit):
			writeJSONResponse(w, http.StatusTooManyRequests, map[string]any{
				"error":   "hop_limit",
				"message": "the agent relay chain reached the room's hop limit; wait for the user or a fresh message",
			})
		case errors.Is(err, agent.ErrExternalDisabled):
			writeError(w, http.StatusForbidden, "disabled", "external agent is disabled")
		case errors.Is(err, agent.ErrGroupNotFound), errors.Is(err, agent.ErrGroupNotMember):
			writeError(w, http.StatusNotFound, "not_found", "group not found")
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	// The caller has obviously read up to its own post.
	if seq, found, err := s.groupdms.ExternalMessageSeq(g.ID, msg.ID); err == nil && found {
		if st := s.externalStore(); st != nil {
			_ = st.AdvanceExternalCursor(r.Context(), c.rec.ID, g.ID, seq)
		}
	}
	writeJSONResponse(w, http.StatusOK, msg)
}

func (s *Server) handleExternalAck(w http.ResponseWriter, r *http.Request) {
	c, g, ok := s.externalMember(w, r)
	if !ok {
		return
	}
	var req struct {
		MessageID string `json:"messageId"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.MessageID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "messageId is required")
		return
	}
	seq, found, err := s.groupdms.ExternalMessageSeq(g.ID, req.MessageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusBadRequest, "unknown_message", "message not found in this group")
		return
	}
	st := s.externalStore()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store unavailable")
		return
	}
	if err := st.AdvanceExternalCursor(r.Context(), c.rec.ID, g.ID, seq); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ackedSeq": s.externalCursors(r.Context(), c.rec.ID)[g.ID]})
}

// handleExternalEvents streams NDJSON events. The subscription is
// registered before the hello snapshot so nothing falls in the gap.
func (s *Server) handleExternalEvents(w http.ResponseWriter, r *http.Request) {
	c := externalCallerFrom(r.Context())
	st, ok := s.external.hub.subscribe(c.rec.ID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "shutting down")
		return
	}
	defer s.external.hub.unsubscribe(c.rec.ID, st)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	write := func(v any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(externalWriteTimeout))
		if err := enc.Encode(v); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// guarded writes are atomic w.r.t. credential changes (credMu):
	// nothing reaches the client after a revoke / disable / delete /
	// re-pair has completed. A credential change can stall behind one
	// in-flight write for at most externalWriteTimeout.
	guarded := func(v any) bool {
		s.external.credMu.RLock()
		defer s.external.credMu.RUnlock()
		select {
		case <-st.done:
			return false
		default:
		}
		if !s.callerStillValid(c) {
			return false
		}
		return write(v)
	}
	cursors := s.externalCursors(r.Context(), c.rec.ID)
	groups := []externalGroupView{}
	for _, g := range s.externalGroups(c.rec.ID) {
		groups = append(groups, s.externalGroupView(g, c.rec.ID, cursors))
	}
	if !guarded(map[string]any{
		"type": "hello", "agent": map[string]any{"id": c.rec.ID, "name": c.rec.Name}, "groups": groups,
	}) {
		return
	}
	ping := time.NewTicker(externalPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-st.done:
			_ = write(map[string]any{"type": "closed", "reason": st.reason})
			return
		case <-ping.C:
			if !guarded(map[string]any{"type": "ping"}) {
				return
			}
		case ev := <-st.ch:
			switch ev.Type {
			case agent.ExternalEventMessage, agent.ExternalEventGroupUpdated:
				if s.groupdms.CheckMembership(ev.GroupID, c.rec.ID) != nil {
					continue
				}
			}
			if !guarded(ev) {
				return
			}
		}
	}
}

type externalUpdate struct {
	GroupID         string `json:"groupId"`
	LatestMessageID string `json:"latestMessageId"`
	LatestSeq       int64  `json:"latestSeq"`
	AckedSeq        int64  `json:"ackedSeq"`
}

func (s *Server) externalPendingUpdates(ctx context.Context, id string) []externalUpdate {
	cursors := s.externalCursors(ctx, id)
	out := []externalUpdate{}
	for _, g := range s.externalGroups(id) {
		muted := false
		for _, m := range g.Members {
			if m.AgentID == id && m.NotifyMode == agent.NotifyMuted {
				muted = true
			}
		}
		if muted {
			continue
		}
		latest, seq, err := s.groupdms.ExternalLatest(g.ID)
		if err != nil || seq <= cursors[g.ID] {
			continue
		}
		out = append(out, externalUpdate{GroupID: g.ID, LatestMessageID: latest, LatestSeq: seq, AckedSeq: cursors[g.ID]})
	}
	return out
}

// handleExternalUpdates is a level-triggered long poll: it returns as
// soon as any non-muted room has messages past the ack cursor, or a
// membership / room event newer than ?since= (the eventSeq of the
// previous response) exists. resync=true means events were lost
// (overflow or kojo restart): re-read GET /v1/groups.
func (s *Server) handleExternalUpdates(w http.ResponseWriter, r *http.Request) {
	c := externalCallerFrom(r.Context())
	q := r.URL.Query()
	wait := externalDefaultWait
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "wait must be a non-negative integer (seconds)")
			return
		}
		wait = time.Duration(n) * time.Second
		if wait > externalMaxWait {
			wait = externalMaxWait
		}
	}
	var since int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "since must be a non-negative integer")
			return
		}
		since = n
	}
	waiter := s.external.hub.addWaiter(c.rec.ID)
	defer s.external.hub.removeWaiter(c.rec.ID, waiter)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	expired := wait == 0
	respond := func() bool {
		// credMu is held across validity check + gather + write + flush
		// so nothing leaves after a credential change completes.
		s.external.credMu.RLock()
		defer s.external.credMu.RUnlock()
		send := func(status int, body any) {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(externalWriteTimeout))
			writeJSONResponse(w, status, body)
			_ = rc.Flush()
		}
		if !s.callerStillValid(c) {
			send(http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": "unauthorized", "message": "credentials no longer valid"}})
			return true
		}
		evs, cur, gap := s.external.hub.eventsSince(c.rec.ID, since)
		ups := s.externalPendingUpdates(r.Context(), c.rec.ID)
		if len(ups) == 0 && len(evs) == 0 && !gap && !expired {
			return false
		}
		if evs == nil {
			evs = []externalLoggedEvent{}
		}
		send(http.StatusOK, map[string]any{
			"updates": ups, "events": evs, "eventSeq": cur, "resync": gap,
		})
		return true
	}
	for {
		if respond() {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			expired = true
		case <-waiter:
			if s.external.hub.isClosed() {
				expired = true
			}
		}
	}
}

// ---------------------------------------------------------------------
// Owner: per-room trust.

func (s *Server) handleSetGroupExternalTrust(w http.ResponseWriter, r *http.Request) {
	if !auth.FromContext(r.Context()).HasOwnerAuthority() {
		writeError(w, http.StatusForbidden, "forbidden", "owner only")
		return
	}
	id := r.PathValue("id")
	g, ok := s.groupdms.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "group not found: "+id)
		return
	}
	var req struct {
		Trust string `json:"trust"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Trust != agent.ExternalTrustUntrusted && req.Trust != agent.ExternalTrustPeer {
		writeError(w, http.StatusBadRequest, "invalid_request", "trust must be \"untrusted\" or \"peer\"")
		return
	}
	st := s.externalStore()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store unavailable")
		return
	}
	if err := st.SetGroupDMExternalTrust(r.Context(), g.ID, req.Trust); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.reloadExternalCache(r.Context())
	writeJSONResponse(w, http.StatusOK, map[string]any{"groupId": g.ID, "trust": s.groupdms.ExternalTrust(g.ID)})
}
