package server

import (
	"bytes"
	"context"
	"github.com/loppo-llc/kojo/internal/auth"
	"net/http/httptest"
	"testing"
)

func TestGoalSnapshotRejectsForeignAgent(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	r := httptest.NewRequest("GET", "/api/v1/agents/ag_other/goal", nil)
	r.SetPathValue("id", "ag_other")
	r = r.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleAgent, AgentID: "ag_self"}))
	w := httptest.NewRecorder()
	s.handleGetGoal(w, r)
	if w.Code != 403 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
func TestGoalRecoveryRejectsWrongPeer(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	r := httptest.NewRequest("POST", "/api/v1/peers/goals/resume", bytes.NewBufferString(`{"agentId":"ag_1","holderId":"peer-real"}`))
	r = r.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RolePeer, PeerID: "peer-other"}))
	w := httptest.NewRecorder()
	s.handlePeerGoalResume(w, r)
	if w.Code != 403 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
