package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
)

type externalChatRelayRegistry struct {
	mu      sync.Mutex
	entries map[string]externalChatRelayEntry
}

type externalChatRelayEntry struct {
	peerID string
	refs   int
}

var errExternalChatHubForbidden = errors.New("external chat caller is not the agent's allowed Hub proxy")

func newExternalChatRelayRegistry() *externalChatRelayRegistry {
	return &externalChatRelayRegistry{entries: make(map[string]externalChatRelayEntry)}
}

func (rr *externalChatRelayRegistry) acquire(agentID, peerID string) func() {
	rr.mu.Lock()
	e := rr.entries[agentID]
	if e.refs == 0 {
		e.peerID = peerID
	}
	e.refs++
	rr.entries[agentID] = e
	rr.mu.Unlock()
	return func() {
		rr.mu.Lock()
		e := rr.entries[agentID]
		e.refs--
		if e.refs <= 0 {
			delete(rr.entries, agentID)
		} else {
			rr.entries[agentID] = e
		}
		rr.mu.Unlock()
	}
}

func (rr *externalChatRelayRegistry) allowed(agentID, peerID string, unsafeOwner bool) bool {
	if rr == nil {
		return false
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	e, ok := rr.entries[agentID]
	return ok && e.refs > 0 && ((peerID != "" && peerID == e.peerID) || (unsafeOwner && e.peerID == ""))
}

func (s *Server) externalChatHubAddress(ctx context.Context, r *http.Request, hinted string) (string, string, error) {
	p := auth.FromContext(r.Context())
	if p.PeerID != "" {
		// A paired peer is not automatically the orchestrator for every
		// migrated agent. Only the peer stamped into the agent lock by the
		// handoff protocol may supply the Hub MCP endpoint; otherwise an
		// unrelated paired peer could make the runtime send its agent token
		// (and Slack MCP calls) to an attacker-controlled registry URL.
		lock, err := s.agents.Store().GetAgentLock(ctx, r.PathValue("id"))
		if err != nil {
			return "", "", fmt.Errorf("resolve agent lock for Hub: %w", err)
		}
		if lock.AllowedProxyPeer == "" || lock.AllowedProxyPeer != p.PeerID {
			return "", "", errExternalChatHubForbidden
		}
		rec, err := s.agents.Store().GetPeer(ctx, p.PeerID)
		if err != nil {
			return "", "", fmt.Errorf("resolve Hub peer: %w", err)
		}
		addr, err := peer.NormalizeAddress(rec.URL)
		return p.PeerID, addr, err
	}
	if s.unsafePeer && p.IsOwner() && strings.TrimSpace(hinted) != "" {
		addr, err := peer.NormalizeAddress(hinted)
		return "", addr, err
	}
	return "", "", errors.New("external chat caller has no usable Hub identity")
}

func (s *Server) handleExternalChatFile(w http.ResponseWriter, r *http.Request) {
	if !s.externalChatPeerAllowed(w, r) {
		return
	}
	agentID := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if s.externalChatRelays == nil || !s.externalChatRelays.allowed(agentID, p.PeerID, s.unsafePeer && p.IsOwner()) {
		writeError(w, http.StatusForbidden, "forbidden", "external chat file relay is not active")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid file relay request")
		return
	}
	f, size, kind := openUploadPath(in.Path)
	if kind != "" {
		writeError(w, http.StatusBadRequest, "invalid_path", uploadPathUserMessage(kind))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
