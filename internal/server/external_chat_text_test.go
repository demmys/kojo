package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func prepareRemoteExternalChat(t *testing.T, holderURL string) (*Server, *externalChatRouter, string) {
	t.Helper()
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "hub", Name: "Hub"}
	a, err := srv.agents.Create(agent.AgentConfig{Name: "remote Slack"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "holder", Name: "Holder", URL: holderURL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(ctx, a.ID, "holder",
		store.NowMillis(), int64((5*time.Minute)/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	router := newExternalChatRouter(srv)
	router.pollInterval = 5 * time.Millisecond
	router.probeTimeout = time.Second
	return srv, router, a.ID
}

func writeExternalChatTestStream(t *testing.T, w http.ResponseWriter, events ...agent.ChatEvent) {
	t.Helper()
	w.Header().Set("Content-Type", externalChatTextContentType)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for i := range events {
		evt := events[i]
		if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err != nil {
			t.Errorf("encode event: %v", err)
			return
		}
	}
}

func TestExternalChatRouterStreamsRemoteTextTurn(t *testing.T) {
	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case http.MethodPost:
			var req externalChatTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requestSeen <- req
			writeExternalChatTestStream(t, w,
				agent.ChatEvent{Type: "text", Delta: "hel"},
				agent.ChatEvent{Type: "text", Delta: "lo"},
				agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "hello"}})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "full history", agent.OneShotOpts{
		SessionKey: "slack-thread", SystemPromptExtra: "slack context",
		History: []chathistory.HistoryMessage{
			{MessageID: "m1", UserID: "U1", Text: "earlier"},
		},
		HistorySelfUserID:                 "B1",
		DisableKojoAttachmentInstructions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.ChatEvent
	for evt := range events {
		got = append(got, evt)
	}
	if len(got) != 3 || got[0].Delta != "hel" || got[1].Delta != "lo" || got[2].Type != "done" ||
		got[2].Message == nil || got[2].Message.Content != "hello" {
		t.Fatalf("events = %#v", got)
	}
	select {
	case req := <-requestSeen:
		if req.Message != "full history" || req.SessionKey != "slack-thread" {
			t.Fatalf("request = %#v", req)
		}
		if req.FreshSessionContext == "" || req.ResumeSessionContext == "" ||
			!strings.Contains(req.FreshSessionContext, "earlier") ||
			!strings.Contains(req.ResumeSessionContext, "earlier") {
			t.Fatalf("request history contexts = %#v", req)
		}
		if !req.DisableAttachments || req.SystemPromptExtra != "slack context" {
			t.Fatalf("request options = %#v", req)
		}
	default:
		t.Fatal("holder did not receive request")
	}
}

func TestExternalChatRouterUploadsAttachmentsToRemoteHolder(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	sourcePath := filepath.Join(uploadDir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("peer attachment"), 0o600); err != nil {
		t.Fatal(err)
	}

	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case r.URL.Path == "/api/v1/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f, h, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer f.Close()
			raw, _ := io.ReadAll(f)
			if !bytes.Equal(raw, []byte("peer attachment")) || h.Filename != "source.txt" {
				t.Errorf("uploaded file = %q filename=%q", raw, h.Filename)
			}
			_ = json.NewEncoder(w).Encode(agent.MessageAttachment{
				Path: "/holder/upload/source.txt", Name: h.Filename, Mime: h.Header.Get("Content-Type"), Size: int64(len(raw)),
			})
		case r.Method == http.MethodPost:
			var req externalChatTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode external chat: %v", err)
				return
			}
			requestSeen <- req
			writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "read this", agent.OneShotOpts{
		Attachments: []agent.MessageAttachment{{Path: sourcePath, Name: "source.txt", Mime: "text/plain", Size: 15}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	select {
	case req := <-requestSeen:
		if len(req.Attachments) != 1 || req.Attachments[0].Path != "/holder/upload/source.txt" {
			t.Fatalf("relayed attachments = %#v", req.Attachments)
		}
	case <-time.After(time.Second):
		t.Fatal("holder did not receive external chat request")
	}
}

func TestExternalChatFileRequiresActiveMatchingRelay(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	path := filepath.Join(uploadDir, "result.txt")
	if err := os.WriteFile(path, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{externalChatRelays: newExternalChatRelayRegistry()}
	release := s.externalChatRelays.acquire("ag_test", "hub")
	defer release()

	call := func(peerID string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"path": path})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/ag_test/external-chat/file", bytes.NewReader(body))
		req.SetPathValue("id", "ag_test")
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: peerID}))
		rr := httptest.NewRecorder()
		s.handleExternalChatFile(rr, req)
		return rr
	}
	if rr := call("other"); rr.Code != http.StatusForbidden {
		t.Fatalf("other peer status = %d", rr.Code)
	}
	if rr := call("hub"); rr.Code != http.StatusOK || rr.Body.String() != "result" {
		t.Fatalf("hub response status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestExternalChatHubAddressRequiresAllowedProxyPeer(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	a, err := s.agents.Create(agent.AgentConfig{Name: "remote MCP trust"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"hub", "other"} {
		if _, err := s.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
			DeviceID: id, Name: id, URL: "https://" + id + ".example:443", Status: store.PeerStatusOnline,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}

	resolve := func(peerID string) (string, error) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/external-chat/text", nil)
		req.SetPathValue("id", a.ID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: peerID}))
		_, addr, err := s.externalChatHubAddress(ctx, req, "")
		return addr, err
	}
	if _, err := resolve("other"); err == nil {
		t.Fatal("unrelated paired peer was accepted as the Hub")
	}
	addr, err := resolve("hub")
	if err != nil || addr != "https://hub.example:443" {
		t.Fatalf("allowed Hub addr=%q err=%v", addr, err)
	}
}

func TestAgentWebSocketRoutingRejectsUnrelatedPeer(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "holder", Name: "Holder"}
	a, err := s.agents.Create(agent.AgentConfig{Name: "remote WebUI trust"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID+"/ws", nil)
	req.SetPathValue("id", a.ID)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: "other"}))
	rr := httptest.NewRecorder()
	s.handleAgentWebSocketRouting(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestExternalChatRouterBoundsHistoryBeforeRemoteRelay(t *testing.T) {
	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		if r.ContentLength >= externalChatTextBodyLimit {
			t.Errorf("relayed body = %d bytes, limit = %d", r.ContentLength, externalChatTextBodyLimit)
		}
		var req externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requestSeen <- req
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(holder.Close)

	history := make([]chathistory.HistoryMessage, 6000)
	for i := range history {
		history[i] = chathistory.HistoryMessage{
			MessageID: fmt.Sprintf("m-%04d", i),
			UserID:    "U1",
			UserName:  "User",
			Text:      fmt.Sprintf("message-%04d-%s", i, strings.Repeat("x", 1000)),
		}
	}
	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "current", agent.OneShotOpts{
		History: history, HistorySelfUserID: "B1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	select {
	case req := <-requestSeen:
		if len(req.FreshSessionContext) > chathistory.DefaultMaxChars+len("\n---\n\n") {
			t.Fatalf("fresh context length = %d", len(req.FreshSessionContext))
		}
		if !strings.Contains(req.FreshSessionContext, "message-5999") ||
			strings.Contains(req.FreshSessionContext, "message-0000") {
			t.Fatal("fresh context did not preserve the bounded recent window")
		}
		if !strings.Contains(req.ResumeSessionContext, "message-0000") ||
			!strings.Contains(req.ResumeSessionContext, "message-5999") ||
			strings.Contains(req.ResumeSessionContext, "message-3000") {
			t.Fatal("resume context did not preserve bounded head and tail windows")
		}
	default:
		t.Fatal("holder did not receive request")
	}
}

func TestExternalChatRouterWaitsInMemoryDuringHandoff(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					Switching: true, HolderPeer: "holder", Unavailable: "device switch in progress",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", externalChatTextContentType)
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(30 * time.Millisecond)
		evt := agent.ChatEvent{Type: "done"}
		if err := json.NewEncoder(w).Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err != nil {
			t.Errorf("encode done: %v", err)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	seenDone := false
	for evt := range events {
		seenDone = seenDone || evt.Type == "done"
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want 1", posts.Load())
	}
	if !seenDone {
		t.Fatal("handoff-dispatched stream was cancelled before done")
	}
}

func TestExternalChatRouterWaitsForRuntimeAfterLockArrival(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				// Lock already points here, but finalize has not activated the
				// runtime yet. No explicit Switching flag exists on the target.
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					HolderPeer: "holder", Unavailable: "agent runtime is not active on this peer",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want 1 after runtime activation", posts.Load())
	}
}

func TestExternalChatRouterDoesNotRetryAfterUncertainPOST(t *testing.T) {
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	if _, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{}); err == nil {
		t.Fatal("expected transport error")
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want exactly 1", posts.Load())
	}
}

func TestExternalChatRouterRediscoversBeforePOSTAttempt(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	newHolder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				// Discovery reached the new lock holder before its runtime was
				// activated. It must stay in the bounded handoff wait.
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					HolderPeer: "new-holder", Unavailable: "agent runtime is not active on this peer",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "new-holder"})
			return
		}
		posts.Add(1)
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(newHolder.Close)

	srv, router, agentID := prepareRemoteExternalChat(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "holder", Name: "Old holder", URL: "http://127.0.0.1:1", Status: store.PeerStatusOffline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "new-holder", Name: "New holder", URL: newHolder.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(ctx, agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if posts.Load() != 1 {
		t.Fatalf("new holder POST count = %d, want 1", posts.Load())
	}
}

func TestExternalChatRouterHandoffWaitTimesOutWithoutDispatch(t *testing.T) {
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
				Switching: true, HolderPeer: "holder", Unavailable: "device switch in progress",
			})
			return
		}
		posts.Add(1)
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 35 * time.Millisecond
	if _, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{}); err == nil {
		t.Fatal("expected handoff timeout")
	}
	if posts.Load() != 0 {
		t.Fatalf("POST count = %d, want 0", posts.Load())
	}
}

func TestExternalChatRouterCancellationReachesHolder(t *testing.T) {
	cancelled := make(chan struct{})
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		w.Header().Set("Content-Type", externalChatTextContentType)
		_ = json.NewEncoder(w).Encode(externalChatTextEnvelope{
			Kind: "event", Event: &agent.ChatEvent{Type: "text", Delta: "started"},
		})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-ticker.C:
				if _, err := w.Write([]byte("{\"kind\":\"heartbeat\"}\n")); err != nil {
					close(cancelled)
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := router.ChatOneShot(ctx, agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if evt := <-events; evt.Delta != "started" {
		t.Fatalf("first event = %#v", evt)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("holder request context was not cancelled")
	}
}

func TestExternalChatHandlerRejectsNonPeer(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_x/external-chat/ready", nil)
	req.SetPathValue("id", "ag_x")
	rr := httptest.NewRecorder()
	srv.handleExternalChatReady(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}
