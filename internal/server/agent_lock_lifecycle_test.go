package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func TestLocalAgentActivationHook_CreateForkUnarchive(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "peer-local"}
	guard := peer.NewAgentLockGuard(srv.agents.Store(), srv.peerID, slog.Default())
	t.Cleanup(guard.Stop)
	disabledCron := ""

	srv.SetOnLocalAgentActivated(func(ctx context.Context, agentID string) {
		if err := srv.agents.MarkAgentArrivedHere(ctx, agentID, srv.peerID.DeviceID); err != nil {
			t.Fatalf("MarkAgentArrivedHere(%s): %v", agentID, err)
		}
		guard.AddAgent(ctx, agentID)
	})
	srv.SetOnLocalAgentDeactivated(func(ctx context.Context, agentID string) {
		if err := srv.agents.ClearAgentArrivedHere(ctx, agentID); err != nil {
			t.Fatalf("ClearAgentArrivedHere(%s): %v", agentID, err)
		}
		guard.RemoveAgent(ctx, agentID)
	})

	createBody, err := json.Marshal(agent.AgentConfig{
		Name:     "created",
		Tool:     "claude",
		CronExpr: &disabledCron,
	})
	if err != nil {
		t.Fatal(err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(createBody))
	createReq = authedRequest(createReq, auth.Principal{Role: auth.RoleOwner})
	createRR := httptest.NewRecorder()
	srv.handleCreateAgent(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create status = %d body = %s", createRR.Code, createRR.Body.String())
	}
	var created agent.Agent
	readJSONResponse(t, createRR, &created)
	assertLocalAgentLock(t, srv, created.ID, true)
	assertArrivedMarker(t, srv, created.ID, true)

	forkBody := []byte(`{"name":"forked","includeTranscript":false}`)
	forkReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+created.ID+"/fork", bytes.NewReader(forkBody))
	forkReq.SetPathValue("id", created.ID)
	forkReq = authedRequest(forkReq, auth.Principal{Role: auth.RoleOwner})
	forkRR := httptest.NewRecorder()
	srv.handleForkAgent(forkRR, forkReq)
	if forkRR.Code != http.StatusOK {
		t.Fatalf("fork status = %d body = %s", forkRR.Code, forkRR.Body.String())
	}
	var forked agent.Agent
	readJSONResponse(t, forkRR, &forked)
	assertLocalAgentLock(t, srv, forked.ID, true)
	assertArrivedMarker(t, srv, forked.ID, true)

	archiveReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+created.ID+"?archive=true", nil)
	archiveReq.SetPathValue("id", created.ID)
	archiveReq = authedRequest(archiveReq, auth.Principal{Role: auth.RoleOwner})
	archiveRR := httptest.NewRecorder()
	srv.handleDeleteAgent(archiveRR, archiveReq)
	if archiveRR.Code != http.StatusOK {
		t.Fatalf("archive status = %d body = %s", archiveRR.Code, archiveRR.Body.String())
	}
	assertLocalAgentLock(t, srv, created.ID, false)
	assertArrivedMarker(t, srv, created.ID, false)
	unarchiveReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+created.ID+"/unarchive", nil)
	unarchiveReq.SetPathValue("id", created.ID)
	unarchiveReq = authedRequest(unarchiveReq, auth.Principal{Role: auth.RoleOwner})
	unarchiveRR := httptest.NewRecorder()
	srv.handleUnarchiveAgent(unarchiveRR, unarchiveReq)
	if unarchiveRR.Code != http.StatusOK {
		t.Fatalf("unarchive status = %d body = %s", unarchiveRR.Code, unarchiveRR.Body.String())
	}
	assertLocalAgentLock(t, srv, created.ID, true)
	assertArrivedMarker(t, srv, created.ID, true)
}

func assertLocalAgentLock(t *testing.T, srv *Server, agentID string, want bool) {
	t.Helper()
	lock, err := srv.agents.Store().GetAgentLock(context.Background(), agentID)
	if want {
		if err != nil {
			t.Fatalf("GetAgentLock(%s): %v", agentID, err)
		}
		if lock.HolderPeer != srv.peerID.DeviceID {
			t.Fatalf("lock holder = %q, want %q", lock.HolderPeer, srv.peerID.DeviceID)
		}
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgentLock(%s) err = %v, want ErrNotFound", agentID, err)
	}
}

func assertArrivedMarker(t *testing.T, srv *Server, agentID string, want bool) {
	t.Helper()
	arrived, err := srv.agents.ListArrivedAgents(context.Background())
	if err != nil {
		t.Fatalf("ListArrivedAgents: %v", err)
	}
	got := false
	for _, id := range arrived {
		if id == agentID {
			got = true
			break
		}
	}
	if got != want {
		t.Fatalf("arrived marker for %s = %v, want %v (all=%v)", agentID, got, want, arrived)
	}
}

func TestSwitchDeviceRejectsMissingAgentLockBeforeBegin(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "peer-src"}
	srv.blob = blob.New(t.TempDir())
	ctx := context.Background()
	st := srv.agents.Store()

	now := time.Now().UnixMilli()
	for _, rec := range []*store.PeerRecord{
		{DeviceID: "peer-src", Name: "source", URL: "http://source.example", Status: store.PeerStatusOnline, LastSeen: now},
		{DeviceID: "peer-tgt", Name: "target", URL: "http://target.example", Status: store.PeerStatusOnline, LastSeen: now},
	} {
		if _, err := st.UpsertPeer(ctx, rec); err != nil {
			t.Fatalf("upsert peer %s: %v", rec.DeviceID, err)
		}
	}

	disabledCron := ""
	a, err := srv.agents.Create(agent.AgentConfig{
		Name:     "new without lock",
		Tool:     "claude",
		CronExpr: &disabledCron,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.GetAgentLock(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition lock lookup err = %v, want ErrNotFound", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/handoff/switch", bytes.NewReader([]byte(`{"target_peer_id":"target"}`)))
	req.SetPathValue("id", a.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleAgentHandoffSwitch(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	readJSONResponse(t, rr, &body)
	if body.Error.Code != "lock_missing" {
		t.Fatalf("error code = %q body = %s", body.Error.Code, rr.Body.String())
	}
}
