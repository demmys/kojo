package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if rr := call(); rr.Code != http.StatusOK {
		t.Fatalf("dedup status = %d body=%s", rr.Code, rr.Body.String())
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
