package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

type fakeArrivalReservation struct {
	started chan struct{}
	count   int
}

func (r *fakeArrivalReservation) Activate(context.Context, string, string) error {
	r.count++
	select {
	case r.started <- struct{}{}:
	default:
	}
	return nil
}
func (r *fakeArrivalReservation) Release() {}

type blockingArrivalReservation struct {
	mu      sync.Mutex
	count   int
	started chan struct{}
	release chan struct{}
}

func (r *blockingArrivalReservation) Activate(ctx context.Context, _, _ string) error {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}
func (r *blockingArrivalReservation) Release() {}

func TestHandleHandoffArrivalContinuationRejectsDifferentSigner(t *testing.T) {
	srv := &Server{}
	body, _ := json.Marshal(handoffArrivalRequest{
		HolderDeviceID: "holder-a", AgentID: "ag_1", OpID: "op_1",
		SessionKey: "groupdm:gd_1", SourceDeviceID: "source", Capability: "cap",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/handoff/arrival", bytes.NewReader(body))
	req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "holder-b"})
	rr := httptest.NewRecorder()
	srv.handleHandoffArrivalContinuation(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleHandoffArrivalContinuationAcceptsHubOwnerWithMatchingPeerID(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	thread, _, err := gdm.FindOrCreateDM([]string{agentID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	reservation := &fakeArrivalReservation{started: make(chan struct{}, 1)}
	capability := srv.mintHandoffArrivalCapability(agentID, "groupdm:"+thread.ID, reservation)
	if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{
		SourceDeviceID: "source", TargetDeviceID: "holder", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:" + thread.ID, Capability: capability,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:" + thread.ID, SourceDeviceID: "source", Capability: capability,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/handoff/arrival", bytes.NewReader(body))
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner, PeerID: "holder"})
	rr := httptest.NewRecorder()
	srv.handleHandoffArrivalContinuation(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleHandoffArrivalContinuationRejectsStaleHolder(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder-current", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(handoffArrivalRequest{
		HolderDeviceID: "holder-stale", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:gd_1", SourceDeviceID: "source", Capability: "cap",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/handoff/arrival", bytes.NewReader(body))
	req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "holder-stale"})
	rr := httptest.NewRecorder()
	srv.handleHandoffArrivalContinuation(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleHandoffArrivalContinuationConsumesBoundCapability(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	thread, _, err := gdm.FindOrCreateDM([]string{agentID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	reservation := &fakeArrivalReservation{started: started}
	capability := srv.mintHandoffArrivalCapability(agentID, "groupdm:"+thread.ID, reservation)
	if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{
		SourceDeviceID: "source", TargetDeviceID: "holder", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:" + thread.ID, Capability: capability,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:" + thread.ID, SourceDeviceID: "source", Capability: capability,
	})
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/handoff/arrival", bytes.NewReader(body))
		req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "holder"})
		rr := httptest.NewRecorder()
		srv.handleHandoffArrivalContinuation(rr, req)
		return rr
	}
	if rr := call(); rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	srv.handoffArrivalMu.Lock()
	entry := srv.handoffArrivalCaps[capability]
	if entry == nil || !entry.Accepted || entry.Reservation != nil {
		t.Fatalf("accepted capability was not compacted: %#v", entry)
	}
	// Finalize delivery is durably retryable, so an accepted dedup tombstone
	// remains valid even after the original one-hour admission deadline.
	entry.ExpiresAt = time.Now().Add(-time.Hour)
	srv.handoffArrivalMu.Unlock()
	if rr := call(); rr.Code != http.StatusOK {
		t.Fatalf("late dedup status = %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("continuation did not start")
	}
	select {
	case <-started:
		t.Fatal("duplicate callback activated the reservation twice")
	case <-time.After(50 * time.Millisecond):
	}
	if reservation.count != 1 {
		t.Fatalf("activation count = %d", reservation.count)
	}
}

func TestFinishHandoffArrivalTurnDropsUnusedCapability(t *testing.T) {
	srv := &Server{}
	capability := srv.mintHandoffArrivalCapability("ag_1", "slack:C:T",
		&fakeArrivalReservation{started: make(chan struct{}, 1)})
	srv.finishHandoffArrivalTurn(capability)

	srv.handoffArrivalMu.Lock()
	defer srv.handoffArrivalMu.Unlock()
	if _, ok := srv.handoffArrivalCaps[capability]; ok {
		t.Fatal("unused capability survived its source turn")
	}
}

func TestFinishHandoffArrivalTurnDefersCleanupDuringAdmission(t *testing.T) {
	srv := &Server{}
	capability := srv.mintHandoffArrivalCapability("ag_1", "slack:C:T",
		&fakeArrivalReservation{started: make(chan struct{}, 1)})
	srv.handoffArrivalMu.Lock()
	entry := srv.handoffArrivalCaps[capability]
	entry.Admitting = true
	srv.handoffArrivalMu.Unlock()

	srv.finishHandoffArrivalTurn(capability)
	srv.completeHandoffArrivalAdmission(capability, entry, false)

	srv.handoffArrivalMu.Lock()
	defer srv.handoffArrivalMu.Unlock()
	if _, ok := srv.handoffArrivalCaps[capability]; ok {
		t.Fatal("failed admission retained a completed turn capability")
	}
}

func TestConcurrentHandoffArrivalRetriesWaitForAdmission(t *testing.T) {
	srv := &Server{}
	reservation := &blockingArrivalReservation{
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	capability := srv.mintHandoffArrivalCapability("ag_1", "slack:C:T", reservation)
	if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{
		SourceDeviceID: "source", TargetDeviceID: "holder", AgentID: "ag_1", OpID: "op_1",
		SessionKey: "slack:C:T", Capability: capability,
	}); err != nil {
		t.Fatal(err)
	}
	req := handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: "ag_1", OpID: "op_1",
		SessionKey: "slack:C:T", SourceDeviceID: "source", Capability: capability,
	}
	results := make(chan error, 2)
	go func() { results <- srv.activateHandoffArrivalCapability(context.Background(), req, "prompt") }()
	select {
	case <-reservation.started:
	case <-time.After(time.Second):
		t.Fatal("first admission did not start")
	}
	go func() { results <- srv.activateHandoffArrivalCapability(context.Background(), req, "prompt") }()
	select {
	case err := <-results:
		t.Fatalf("concurrent retry returned before admission completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(reservation.release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.count != 1 {
		t.Fatalf("activation count = %d", reservation.count)
	}
}

func TestMintHandoffArrivalCapabilityDoesNotEvictAcceptedTombstones(t *testing.T) {
	srv := &Server{handoffArrivalCaps: make(map[string]*handoffArrivalCapability)}
	for i := 0; i < maxHandoffArrivalCapabilities; i++ {
		key := string(rune(i + 1))
		srv.handoffArrivalCaps[key] = &handoffArrivalCapability{
			AgentID: "ag_1", Accepted: true,
		}
	}
	if capability := srv.mintHandoffArrivalCapability("ag_2", "slack:C:T",
		&fakeArrivalReservation{started: make(chan struct{}, 1)}); capability != "" {
		t.Fatalf("mint = %q; want bounded-capacity degradation", capability)
	}
	if len(srv.handoffArrivalCaps) != maxHandoffArrivalCapabilities {
		t.Fatalf("capability count = %d", len(srv.handoffArrivalCaps))
	}
	if _, ok := srv.handoffArrivalCaps[string(rune(1))]; !ok {
		t.Fatal("accepted tombstone was evicted")
	}
}

func TestPendingFinalizeLockSerializesSameOperationAndCleansUp(t *testing.T) {
	srv := &Server{}
	key := pendingSyncKey{AgentID: "ag_1", OpID: "op_1"}
	unlockFirst := srv.lockPendingFinalize(key)
	acquired := make(chan func(), 1)
	go func() { acquired <- srv.lockPendingFinalize(key) }()

	select {
	case unlock := <-acquired:
		unlock()
		t.Fatal("second finalize acquired the same operation concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	unlockFirst()
	var unlockSecond func()
	select {
	case unlockSecond = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second finalize did not acquire after release")
	}
	unlockSecond()

	srv.pendingFinalizeMu.Lock()
	defer srv.pendingFinalizeMu.Unlock()
	if len(srv.pendingFinalizeLocks) != 0 {
		t.Fatalf("finalize lock entries = %d", len(srv.pendingFinalizeLocks))
	}
}

func TestHandleHandoffArrivalBindRequiresCurrentSourceAndBindsExactOperation(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "source", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	capability := srv.mintHandoffArrivalCapability(agentID, "groupdm:gd_1", &fakeArrivalReservation{started: make(chan struct{}, 1)})
	payload := handoffArrivalBindRequest{
		SourceDeviceID: "source", TargetDeviceID: "target", AgentID: agentID, OpID: "op_1",
		SessionKey: "groupdm:gd_1", Capability: capability,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/peers/handoff/arrival/bind", bytes.NewReader(body))
	req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "source"})
	rr := httptest.NewRecorder()
	srv.handleHandoffArrivalBind(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if err := srv.bindHandoffArrivalCapability(handoffArrivalBindRequest{
		SourceDeviceID: "source", TargetDeviceID: "other", AgentID: agentID, OpID: "op_2",
		SessionKey: "groupdm:gd_1", Capability: capability,
	}); err == nil {
		t.Fatal("capability was rebound to another operation")
	}
}

func TestDispatchHandoffArrivalSuppressesFallbackAfterLostResponse(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(origin.Close)

	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	srv.peerID = &peer.Identity{DeviceID: "holder"}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "origin", Name: "Origin", URL: origin.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	fellBack := false
	err := srv.dispatchHandoffArrivalContinuation(context.Background(), "origin", handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: agentID, OpID: "op", SessionKey: "slack:C:T", Capability: "cap",
	}, func() { fellBack = true })
	if err == nil || fellBack {
		t.Fatalf("err = %v, fellBack = %v; want uncertain error without fallback", err, fellBack)
	}
}

func TestPostHandoffArrivalTreatsMissingCapabilityAsDefiniteFailure(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "origin"}
	uncertain, err := srv.postHandoffArrivalContinuation(context.Background(), "origin", handoffArrivalRequest{
		HolderDeviceID: "origin", AgentID: group.Members[0].AgentID, OpID: "op",
		SessionKey: "slack:C:T", SourceDeviceID: "source", Capability: "missing",
	})
	if err == nil || uncertain {
		t.Fatalf("err = %v, uncertain = %v; want definite failure", err, uncertain)
	}
}

func TestPostRemoteHandoffArrivalTreatsMissingCapabilityAsDefiniteFailure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "invalid_capability", "missing")
	}))
	t.Cleanup(origin.Close)
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "holder"}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "origin", Name: "Origin", URL: origin.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	uncertain, err := srv.postHandoffArrivalContinuation(context.Background(), "origin", handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: group.Members[0].AgentID, OpID: "op",
		SessionKey: "slack:C:T", SourceDeviceID: "source", Capability: "missing",
	})
	if err == nil || uncertain {
		t.Fatalf("err = %v, uncertain = %v; want definite failure", err, uncertain)
	}
}

func TestDispatchHandoffArrivalFallsBackAfterInvalidCapability(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "invalid_capability", "missing")
	}))
	t.Cleanup(origin.Close)
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	srv.peerID = &peer.Identity{DeviceID: "holder"}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "origin", Name: "Origin", URL: origin.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	fellBack := false
	err := srv.dispatchHandoffArrivalContinuation(context.Background(), "origin", handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: agentID, OpID: "op", SessionKey: "slack:C:T", Capability: "missing",
	}, func() { fellBack = true })
	if err != nil || !fellBack {
		t.Fatalf("err = %v, fellBack = %v; want legacy fallback", err, fellBack)
	}
}

func TestResolveAllowedProxyPeerRequiresPairedOrigin(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	req := peerAgentSyncFinalizeRequest{
		SourceDeviceID: "source",
		Continuation: &handoffContinuation{
			OriginPeerID: "origin", SessionKey: "slack:C:T", Capability: "cap",
		},
	}
	if _, err := srv.resolveAllowedProxyPeer(context.Background(), req); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown origin err = %v, want store.ErrNotFound", err)
	}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "origin", Name: "Origin", Status: store.PeerStatusOffline,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.resolveAllowedProxyPeer(context.Background(), req); err != nil || got != "origin" {
		t.Fatalf("paired origin = (%q, %v), want origin", got, err)
	}
	req.Continuation.OriginPeerID = req.SourceDeviceID
	if got, err := srv.resolveAllowedProxyPeer(context.Background(), req); err != nil || got != "source" {
		t.Fatalf("signer origin = (%q, %v), want source", got, err)
	}
}

func TestDispatchHandoffArrivalFallsBackAfterDefiniteDialFailure(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	originURL := origin.URL
	origin.Close()

	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	agentID := group.Members[0].AgentID
	srv.peerID = &peer.Identity{DeviceID: "holder"}
	if _, err := srv.agents.Store().AcquireAgentLock(context.Background(), agentID, "holder", 0, 60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "origin", Name: "Origin", URL: originURL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	fellBack := false
	err := srv.dispatchHandoffArrivalContinuation(context.Background(), "origin", handoffArrivalRequest{
		HolderDeviceID: "holder", AgentID: agentID, OpID: "op", SessionKey: "slack:C:T", Capability: "cap",
	}, func() { fellBack = true })
	if err != nil || !fellBack {
		t.Fatalf("err = %v, fellBack = %v; want definite failure fallback", err, fellBack)
	}
}
