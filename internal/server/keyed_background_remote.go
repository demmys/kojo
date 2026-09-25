package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// Remote keyed background continuation (holder -> Hub pull, the goal-recovery
// shape):
//
//  1. A Hub dispatch with lingerBackgroundTasks binds a remoteKeyedSurface to
//     the holder's keyed claude session. The surface holds its own Hub file
//     relay reference, so relay callbacks stay valid while the session
//     lingers; it is released when the session exits.
//  2. A run_in_background completion opens an unsolicited turn. The surface
//     buffers it under a one-time token and POSTs keyed-background/notify
//     (kind "turn") to the Hub over the authenticated peer transport.
//  3. The Hub fences the caller to the current lock holder, opens its
//     synthetic admission in the surface FIFO (Slack thread / WebUI thread)
//     and dispatches a one-shot to exactly that holder carrying the token.
//  4. The holder's external-chat handler claims the token and streams the
//     buffered turn back as NDJSON (attachment ACKs included).
//
// Abandoned / stop notices use the same notify route (kind "abandoned").
// Failure policy: bounded buffer (the per-turn 64-event relay plus the
// terminal event) and bounded retry. A turn the Hub cannot take within the
// budget is dropped: it runs to completion unobserved, is logged, and leaves
// a note for the agent's next turn on that key. An abandoned notice is
// retried within its budget, then logged and dropped.

const keyedBgNotifyPath = "/api/v1/peers/keyed-background/notify"

const (
	keyedBgNotifyKindTurn      = "turn"
	keyedBgNotifyKindAbandoned = "abandoned"
	keyedBgPlaceholderMessage  = "[background task notification]"
	maxKeyedBgAttaches         = 256
	maxKeyedBgSeen             = 4096
	keyedBgSeenTTL             = 10 * time.Minute
)

// Test seams.
var (
	keyedBgAttachWait          = 2 * time.Minute
	keyedBgTurnNotifyBudget    = 30 * time.Second
	keyedBgAbandonNotifyBudget = 2 * time.Minute
	keyedBgNotifyBackoff       = 250 * time.Millisecond
	keyedBgNotifyMaxBackoff    = 5 * time.Second
	keyedBgNotifyAttemptLimit  = 10 * time.Second
	// keyedBgHandoffNoticeWait covers the whole abandoned-notice retry
	// budget: after the lock transfers the Hub fences this peer (409), so
	// the switch must not proceed while a notice is still retrying. It only
	// takes this long when the Hub is unreachable (and is still well inside
	// switchDeviceOpTimeout).
	keyedBgHandoffNoticeWait = keyedBgAbandonNotifyBudget + 5*time.Second
)

type keyedBgNotifyRequest struct {
	HolderID   string `json:"holderId"`
	AgentID    string `json:"agentId"`
	SessionKey string `json:"sessionKey"`
	Kind       string `json:"kind"`
	Token      string `json:"token,omitempty"`
	Pending    int    `json:"pending,omitempty"`
	Reason     string `json:"reason,omitempty"`
	NotifyID   string `json:"notifyId"`
}

// keyedBgRegistry: holder-side pending attaches, Hub-side notify dedup.
type keyedBgRegistry struct {
	mu       sync.Mutex
	attaches map[string]*keyedBgAttachEntry
	seen     map[string]time.Time
}

type keyedBgAttachEntry struct {
	agentID    string
	sessionKey string
	hubPeerID  string
	events     <-chan agent.ChatEvent
	cancel     func()
	claimed    chan struct{}
	finished   chan struct{}
	finishOnce sync.Once
}

func randomKeyedBgToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (rr *keyedBgRegistry) register(e *keyedBgAttachEntry) (string, error) {
	token, err := randomKeyedBgToken()
	if err != nil {
		return "", err
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.attaches == nil {
		rr.attaches = make(map[string]*keyedBgAttachEntry)
	}
	if len(rr.attaches) >= maxKeyedBgAttaches {
		return "", errors.New("keyed background attach registry is full")
	}
	rr.attaches[token] = e
	return token, nil
}

// claim removes and returns the entry when it matches the caller exactly.
func (rr *keyedBgRegistry) claim(token, agentID, sessionKey, hubPeerID string) *keyedBgAttachEntry {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	e := rr.attaches[token]
	if e == nil || e.agentID != agentID || e.sessionKey != sessionKey || e.hubPeerID != hubPeerID {
		return nil
	}
	delete(rr.attaches, token)
	close(e.claimed)
	return e
}

// unregister withdraws an unclaimed entry; false means it was claimed.
func (rr *keyedBgRegistry) unregister(token string, e *keyedBgAttachEntry) bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.attaches[token] != e {
		return false
	}
	delete(rr.attaches, token)
	return true
}

// firstNotify records notifyID and reports whether it is new (idempotent
// holder retries after a lost 202).
func (rr *keyedBgRegistry) firstNotify(id string) bool {
	now := time.Now()
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.seen == nil {
		rr.seen = make(map[string]time.Time)
	}
	for k, at := range rr.seen {
		if now.Sub(at) > keyedBgSeenTTL {
			delete(rr.seen, k)
		}
	}
	if _, ok := rr.seen[id]; ok {
		return false
	}
	if len(rr.seen) >= maxKeyedBgSeen {
		// Bounded: dedup is best-effort beyond the cap (a duplicate turn
		// notify fails its one-time claim anyway).
		for k := range rr.seen {
			delete(rr.seen, k)
			break
		}
	}
	rr.seen[id] = now
	return true
}

// finish settles a claimed entry once the attach stream ends: without a
// terminal event the Hub is gone, so the turn is aborted.
func (e *keyedBgAttachEntry) finish(terminal bool) {
	e.finishOnce.Do(func() {
		if !terminal && e.cancel != nil {
			e.cancel()
		}
		close(e.finished)
	})
}

// remoteKeyedSurface is the holder-side KeyedSessionSurface bound to a keyed
// session dispatched by a Hub. It is refcounted: the dispatching request and
// the session each hold a reference; the Hub file relay reference is dropped
// when the last one is released (session exit, at most the 2h max linger).
type remoteKeyedSurface struct {
	s          *Server
	agentID    string
	sessionKey string
	hubPeerID  string
	hubAddr    string

	mu      sync.Mutex
	refs    int
	release func()
}

func (s *Server) newRemoteKeyedSurface(agentID, sessionKey, hubPeerID, hubAddr string) *remoteKeyedSurface {
	sf := &remoteKeyedSurface{s: s, agentID: agentID, sessionKey: sessionKey, hubPeerID: hubPeerID, hubAddr: hubAddr}
	sf.Retain()
	return sf
}

func (sf *remoteKeyedSurface) Retain() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.refs++
	if sf.refs == 1 && sf.release == nil && sf.s.externalChatRelays != nil {
		sf.release = sf.s.externalChatRelays.acquire(sf.agentID, sf.hubPeerID)
	}
}

func (sf *remoteKeyedSurface) Release() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.refs <= 0 {
		return
	}
	sf.refs--
	if sf.refs == 0 && sf.release != nil {
		sf.release()
		sf.release = nil
	}
}

func (sf *remoteKeyedSurface) OriginPeerID() string { return sf.hubPeerID }

func (sf *remoteKeyedSurface) holderID() string {
	if sf.s.peerID == nil {
		return ""
	}
	return sf.s.peerID.DeviceID
}

// HandleKeyedSessionTurn buffers the unsolicited turn for the Hub's attach.
// Returning leaves any unconsumed events to the caller's drain (the turn then
// finishes unobserved; it is not aborted).
func (sf *remoteKeyedSurface) HandleKeyedSessionTurn(ctx context.Context, agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	e := &keyedBgAttachEntry{
		agentID: agentID, sessionKey: sessionKey, hubPeerID: sf.hubPeerID,
		events: events, cancel: cancel,
		claimed: make(chan struct{}), finished: make(chan struct{}),
	}
	token, err := sf.s.keyedBg.register(e)
	if err != nil {
		sf.undelivered(err)
		return
	}
	notifyID, err := randomKeyedBgToken()
	if err == nil {
		err = sf.notify(ctx, keyedBgNotifyRequest{Kind: keyedBgNotifyKindTurn, Token: token, NotifyID: notifyID}, keyedBgTurnNotifyBudget)
	}
	if err != nil && sf.s.keyedBg.unregister(token, e) {
		sf.undelivered(err)
		return
	}
	// Notified (or claimed although the 202 was lost): wait for the claim.
	timer := time.NewTimer(keyedBgAttachWait)
	defer timer.Stop()
	select {
	case <-e.claimed:
	case <-ctx.Done():
		// Lifecycle cancel (reset / delete / shutdown / handoff quiesce).
		if sf.s.keyedBg.unregister(token, e) {
			return
		}
	case <-timer.C:
		if sf.s.keyedBg.unregister(token, e) {
			sf.undelivered(errors.New("Hub did not attach within " + keyedBgAttachWait.String()))
			return
		}
	}
	<-e.finished
}

func (sf *remoteKeyedSurface) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	sf.HandleKeyedSessionTurn(context.Background(), agentID, sessionKey, events, cancel)
}

func (sf *remoteKeyedSurface) undelivered(err error) {
	sf.s.logger.Warn("keyed background turn not delivered to Hub; dropped",
		"agent", sf.agentID, "sessionKey", sf.sessionKey, "hub", sf.hubPeerID, "err", err)
	if sf.s.agents != nil {
		sf.s.agents.SetKeyedBackgroundNote(sf.agentID, sf.sessionKey,
			"[kojo] 前回このスレッドのバックグラウンドタスク完了後のターンは、Hubへ届けられずユーザーに表示されていません。必要なら結果を改めて伝えてください。")
	}
}

// KeyedBackgroundTasksAbandoned reports the abandoned / stop notice to the Hub
// (bounded retry, then logged and dropped). Called off the session's locks.
func (sf *remoteKeyedSurface) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	if pending <= 0 {
		return
	}
	notifyID, err := randomKeyedBgToken()
	if err == nil {
		err = sf.notify(context.Background(), keyedBgNotifyRequest{
			Kind: keyedBgNotifyKindAbandoned, Pending: pending, Reason: reason, NotifyID: notifyID,
		}, keyedBgAbandonNotifyBudget)
	}
	if err != nil {
		sf.s.logger.Warn("keyed background abandoned notice not delivered to Hub; dropped",
			"agent", agentID, "sessionKey", sessionKey, "pending", pending, "reason", reason, "err", err)
	}
}

type keyedBgPermanentError struct{ status int }

func (e keyedBgPermanentError) Error() string {
	return fmt.Sprintf("Hub rejected keyed background notify: HTTP %d", e.status)
}

func (sf *remoteKeyedSurface) notify(ctx context.Context, req keyedBgNotifyRequest, budget time.Duration) error {
	req.HolderID = sf.holderID()
	req.AgentID = sf.agentID
	req.SessionKey = sf.sessionKey
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(budget)
	// The budget bounds in-flight attempts too, so keyedBgHandoffNoticeWait
	// strictly covers a whole notify.
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	backoff := keyedBgNotifyBackoff
	for {
		err = sf.postNotify(ctx, body)
		if err == nil {
			return nil
		}
		var perm keyedBgPermanentError
		if errors.As(err, &perm) || ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
			return err
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > keyedBgNotifyMaxBackoff {
			backoff = keyedBgNotifyMaxBackoff
		}
	}
}

func (sf *remoteKeyedSurface) postNotify(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, keyedBgNotifyAttemptLimit)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, sf.hubAddr+keyedBgNotifyPath, bytes.NewReader(body))
	if err != nil {
		return keyedBgPermanentError{}
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := peer.NoKeepAliveHTTPClient(keyedBgNotifyAttemptLimit).Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests:
		return keyedBgPermanentError{status: resp.StatusCode}
	default:
		return fmt.Errorf("keyed background notify HTTP %d", resp.StatusCode)
	}
}

func validKeyedBgToken(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// handlePeerKeyedBackgroundNotify (Hub) accepts a remote holder's keyed
// background turn / abandoned notice. It answers 202 before any FIFO wait or
// holder I/O; delivery runs in the background.
func (s *Server) handlePeerKeyedBackgroundNotify(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsPeer() {
		writeError(w, http.StatusForbidden, "forbidden", "peer required")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	var req keyedBgNotifyRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid keyed background notify: "+err.Error())
		return
	}
	// Bind the claimed holder to the authenticated peer. Only the --unsafe
	// owner escape hatch may act without a peer identity.
	if req.HolderID == "" || (p.PeerID != "" && p.PeerID != req.HolderID) || (p.PeerID == "" && !(s.unsafePeer && p.IsOwner())) {
		writeError(w, http.StatusForbidden, "forbidden", "holder identity mismatch")
		return
	}
	validKey := req.AgentID != "" && (strings.HasPrefix(req.SessionKey, "groupdm:") && len(req.SessionKey) > len("groupdm:") ||
		strings.HasPrefix(req.SessionKey, req.AgentID+":slack:"))
	switch {
	case !validKey || len(req.SessionKey) > 512 || !validKeyedBgToken(req.NotifyID):
		validKey = false
	case req.Kind == keyedBgNotifyKindTurn:
		validKey = validKeyedBgToken(req.Token) && req.Pending == 0 && req.Reason == ""
	case req.Kind == keyedBgNotifyKindAbandoned:
		validKey = req.Token == "" && req.Pending > 0 && len(req.Reason) <= 512
	default:
		validKey = false
	}
	if !validKey {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid keyed background notify")
		return
	}
	if s.externalChat == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no external chat router")
		return
	}
	// Fence to the current remote holder: a stale holder (lock moved by a
	// handoff) can never post into the conversation.
	lock, err := s.agents.Store().GetAgentLock(r.Context(), req.AgentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// Transient: the holder retries 5xx (a 409 would end its retries).
		writeError(w, http.StatusServiceUnavailable, "lock_unavailable", "agent lock lookup failed")
		return
	}
	if err != nil || lock == nil || s.peerID == nil || lock.HolderPeer == s.peerID.DeviceID || lock.HolderPeer != req.HolderID {
		writeError(w, http.StatusConflict, "holder_not_current", "keyed background notify must come from the current remote holder")
		return
	}
	if !s.agents.HasKeyedBackgroundSurface(req.AgentID, req.SessionKey) {
		writeError(w, http.StatusNotFound, "no_surface", "no response surface for this session key")
		return
	}
	if !s.keyedBg.firstNotify(req.NotifyID) {
		writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	agentID, key, holder := req.AgentID, req.SessionKey, lock.HolderPeer
	switch req.Kind {
	case keyedBgNotifyKindTurn:
		routeCtx := context.WithValue(r.Context(), externalChatRouteVersionKey{}, externalChatRouteVersion{AgentID: agentID, Version: store.AgentLockVersion{Token: lock.FencingToken, Holder: lock.HolderPeer}})
		s.externalChat.rememberRouteFrom(routeCtx, agentID, holder)
		token := req.Token
		go func() {
			err := s.agents.DeliverRemoteKeyedBackgroundTurn(agentID, key, func(ctx context.Context, groupID, messageID string) (<-chan agent.ChatEvent, error) {
				return s.externalChat.ChatOneShot(ctx, agentID, keyedBgPlaceholderMessage, agent.OneShotOpts{
					SessionKey:                  key,
					ExpectedHolderPeer:          holder,
					AttachBackgroundToken:       token,
					ResponseAttachmentGroupID:   groupID,
					ResponseAttachmentMessageID: messageID,
				})
			})
			if err != nil {
				s.logger.Warn("remote keyed background turn not delivered", "agent", agentID, "sessionKey", key, "holder", holder, "err", err)
			}
		}()
	case keyedBgNotifyKindAbandoned:
		pending, reason := req.Pending, req.Reason
		go s.agents.DeliverRemoteKeyedTasksAbandoned(agentID, key, pending, reason)
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]bool{"accepted": true})
}
