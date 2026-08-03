package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
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
				agent.ChatEvent{Type: "done"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "full history", agent.OneShotOpts{
		SessionKey: "slack-thread", ResumeMessage: "head tail", SystemPromptExtra: "slack context",
		DisableKojoAttachmentInstructions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.ChatEvent
	for evt := range events {
		got = append(got, evt)
	}
	if len(got) != 3 || got[0].Delta != "hel" || got[1].Delta != "lo" || got[2].Type != "done" {
		t.Fatalf("events = %#v", got)
	}
	select {
	case req := <-requestSeen:
		if req.Message != "full history" || req.ResumeMessage != "head tail" || req.SessionKey != "slack-thread" {
			t.Fatalf("request = %#v", req)
		}
		if !req.DisableAttachments || req.SystemPromptExtra != "slack context" {
			t.Fatalf("request options = %#v", req)
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
