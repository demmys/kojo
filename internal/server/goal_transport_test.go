package server

import (
	"context"
	"encoding/json"
	"github.com/loppo-llc/kojo/internal/agent"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteGoalStopTombstoneSurvivesFailedHolderRPC(t *testing.T) {
	runs := make(chan string, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/goal-stop") {
			w.WriteHeader(503)
			return
		}
		var q externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		runs <- q.GoalRunID
		w.Header().Set("Content-Type", externalChatTextContentType)
		_ = json.NewEncoder(w).Encode(externalChatTextEnvelope{Kind: "event", Event: &agent.ChatEvent{Type: "text", Delta: "started"}})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer holder.Close()
	s, router, id := prepareRemoteExternalChat(t, holder.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := router.ChatOneShot(ctx, id, "goal", agent.OneShotOpts{SessionKey: "key", Goal: &agent.GoalRequest{Action: "start", Objective: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if e := <-events; e.Delta != "started" {
		t.Fatalf("event: %+v", e)
	}
	run := <-runs
	if len(run) != 64 {
		t.Fatalf("missing run nonce: %q", run)
	}
	cancel()
	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(15 * time.Second):
		t.Fatal("stop did not drain")
	}
	if err = s.checkGoalStop(context.Background(), id, run); err == nil {
		t.Fatal("failed stop ACK enabled recovery")
	}
	gen := int64(1)
	result := router.dispatch(context.Background(), context.Background(), id, "holder", false, externalChatTextRequest{Goal: &agent.GoalRequest{Action: "resume", ExpectedRunID: run, ExpectedGeneration: &gen}})
	if result.err == nil {
		t.Fatal("queued recovery bypassed durable stop")
	}
}

func TestGoalStopRecoveryFenceIsRunScoped(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	if err := s.recordGoalStop("ag_a", "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.checkGoalStop(context.Background(), "ag_a", "old"); err == nil {
		t.Fatal("old run revived")
	}
	if err := s.checkGoalStop(context.Background(), "ag_a", "new"); err != nil {
		t.Fatal("explicit new resume fenced", err)
	}
	if err := s.checkGoalStop(context.Background(), "ag_b", "old"); err != nil {
		t.Fatal("cross-agent fence", err)
	}
}

func TestGoalStopRetainsMemoryFenceWhenPersistenceFails(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	_ = s.agents.Store().Close()
	if err := s.recordGoalStop("ag_a", "failed"); err == nil {
		t.Fatal("expected failed persistence")
	}
	err := s.checkGoalStop(context.Background(), "ag_a", "failed")
	if err == nil || !strings.Contains(err.Error(), "stopped at its origin") {
		t.Fatalf("missing emergency stop fence: %v", err)
	}
}

func TestRemoteGoalReplyStopTombstoneSurvivesFailedHolderRPC(t *testing.T) {
	runs := make(chan string, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/goal-stop") {
			w.WriteHeader(503)
			return
		}
		var q externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		runs <- q.GoalRunID
		w.Header().Set("Content-Type", externalChatTextContentType)
		_ = json.NewEncoder(w).Encode(externalChatTextEnvelope{Kind: "event", Event: &agent.ChatEvent{Type: "text", Delta: "started"}})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer holder.Close()
	s, router, id := prepareRemoteExternalChat(t, holder.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := router.ChatOneShot(ctx, id, "goal", agent.OneShotOpts{SessionKey: id + ":slack:channel:thread", GoalUserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if e := <-events; e.Delta != "started" {
		t.Fatalf("event: %+v", e)
	}
	run := <-runs
	if len(run) != 64 {
		t.Fatalf("missing run nonce: %q", run)
	}
	cancel()
	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(15 * time.Second):
		t.Fatal("stop did not drain")
	}
	if err = s.checkGoalStop(context.Background(), id, run); err == nil {
		t.Fatal("failed stop ACK enabled recovery")
	}
	gen := int64(1)
	result := router.dispatch(context.Background(), context.Background(), id, "holder", false, externalChatTextRequest{Goal: &agent.GoalRequest{Action: "resume", ExpectedRunID: run, ExpectedGeneration: &gen}})
	if result.err == nil {
		t.Fatal("queued recovery bypassed durable stop")
	}
}
