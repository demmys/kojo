package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
)

func TestHandoffCapabilityRejectsTypedNilReservation(t *testing.T) {
	s := &Server{}
	var r *fakeArrivalReservation
	if cap := s.mintHandoffArrivalCapability("ag_1", "slack:C:T", r); cap != "" {
		t.Fatal("advertised typed nil as an executable capability")
	}
	// Also defend the admission boundary if an invalid adapter was cached.
	s.handoffArrivalCaps = map[string]*handoffArrivalCapability{"cap": {AgentID: "ag_1", SessionKey: "slack:C:T", OpID: "op", HolderID: "hub", ExpiresAt: time.Now().Add(time.Hour), Reservation: r}}
	req := handoffArrivalRequest{AgentID: "ag_1", SessionKey: "slack:C:T", OpID: "op", HolderDeviceID: "hub", Capability: "cap"}
	for range 2 {
		if err := s.activateHandoffArrivalCapability(context.Background(), req, "arrived"); err != errHandoffCapabilityInvalid {
			t.Fatalf("typed nil admission: %v", err)
		}
	}
	if s.handoffArrivalCaps["cap"].Admitting {
		t.Fatal("invalid reservation stranded an admission")
	}
}

type countedHandoffReservation struct{ calls atomic.Int32 }

func (r *countedHandoffReservation) Activate(context.Context, string, string) error {
	r.calls.Add(1)
	return nil
}
func (r *countedHandoffReservation) Release() {}

// The Hub-return branch invokes the adapter locally inside the real finalize
// HTTP handler. It must finish the durable receipt and never admit twice.
func TestLocalHandoffFinalizeHTTPCompletesReceiptAndDeduplicates(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		name := "normal"
		if failCommit {
			name = "pending-commit-failure"
		}
		t.Run(name, func(t *testing.T) { testLocalHandoffFinalizeHTTP(t, failCommit) })
	}
}

func testLocalHandoffFinalizeHTTP(t *testing.T, failCommit bool) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	pending, db := newPendingSyncTestServer(t)
	srv.pendingSyncDB, srv.pendingSyncKEK = db, pending.pendingSyncKEK
	srv.peerID = &peer.Identity{DeviceID: "hub"}
	id, op := group.Members[0].AgentID, "op-return"
	ctx := context.Background()
	if _, err := srv.agents.Store().AcquireAgentLock(ctx, id, "windows", 0, 60000); err != nil {
		t.Fatal(err)
	}
	r := &countedHandoffReservation{}
	cap := srv.mintHandoffArrivalCapability(id, "slack:C:T", r)
	if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{AgentID: id, OpID: op, SessionKey: "slack:C:T", SourceDeviceID: "windows", TargetDeviceID: "hub", Capability: cap}); err != nil {
		t.Fatal(err)
	}
	prepareFencedIncomingForTest(t, srv, id, op, "windows")
	if err := srv.recordPendingAgentSync(ctx, id, op, pendingSyncEntry{SourceDeviceID: "windows", IncomingFenced: true}); err != nil {
		t.Fatal(err)
	}
	if failCommit {
		if _, err := db.DB().Exec(`CREATE TRIGGER fail_pending_commit BEFORE DELETE ON kv WHEN OLD.namespace='handoff' BEGIN SELECT RAISE(ABORT, 'test pending commit failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		srv.handlePeerAgentSyncFinalize(w, authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "windows"}))
	}))
	defer server.Close()
	body, _ := json.Marshal(peerAgentSyncFinalizeRequest{AgentID: id, OpID: op, SourceDeviceID: "windows", Continuation: &handoffContinuation{OriginPeerID: "hub", SessionKey: "slack:C:T", Capability: cap}})
	if failCommit {
		resp, err := server.Client().Post(server.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Fatalf("expected commit failure, got %d: %s", resp.StatusCode, data)
		}
		if r.calls.Load() != 1 {
			t.Fatal("failure did not follow admission")
		}
		fresh := &Server{pendingSyncDB: db, pendingSyncKEK: pending.pendingSyncKEK}
		entry, ok, err := fresh.consumePendingAgentSync(ctx, id, op)
		if err != nil || !ok || !entry.ArrivalHandled || entry.ArrivalUncertain {
			t.Fatalf("admission result not durable: %+v %v %v", entry, ok, err)
		}
		if _, err := db.DB().Exec("DROP TRIGGER fail_pending_commit"); err != nil {
			t.Fatal(err)
		}
		// Force the retry to reload its receipt from disk, as after restart.
		srv.pendingTokensMu.Lock()
		srv.pendingAgentSyncs = nil
		srv.pendingTokensMu.Unlock()
	}
	for attempt := range 2 {
		resp, err := server.Client().Post(server.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("finalize transport failed (including EOF): %v", err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := http.StatusOK
		if attempt == 1 {
			want = http.StatusNotFound // committed op: orchestrator's idempotent result
		}
		if resp.StatusCode != want {
			t.Fatalf("finalize %d: %s", resp.StatusCode, data)
		}
	}
	if r.calls.Load() != 1 {
		t.Fatalf("arrival calls=%d", r.calls.Load())
	}
	lock, err := srv.agents.Store().GetAgentLock(ctx, id)
	if err != nil || lock.HolderPeer != "hub" || lock.AllowedProxyPeer != "hub" {
		t.Fatalf("holder/proxy: %+v %v", lock, err)
	}
	receipt, err := srv.agents.Store().GetIncomingHandoff(ctx, id, op)
	if err != nil || string(receipt.Phase) != "done" {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	if _, ok, err := srv.consumePendingAgentSync(ctx, id, op); err != nil || ok {
		t.Fatalf("pending survived: %v %v", ok, err)
	}
}

func TestRouterDoesNotExportTypedNilHandoffCapability(t *testing.T) {
	seen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONResponse(w, 200, externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		var req externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		seen <- req
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	defer holder.Close()
	_, router, id := prepareRemoteExternalChat(t, holder.URL)
	var reservation *fakeArrivalReservation
	events, err := router.ChatOneShot(context.Background(), id, "arrived", agent.OneShotOpts{SessionKey: "slack:C:T", HandoffArrivalReservation: reservation})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	req := <-seen
	if req.HandoffCapability != "" {
		t.Fatal("router exported a nil adapter capability")
	}
}
