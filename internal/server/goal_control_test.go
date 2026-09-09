package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Use a non-Codex backend so admitted controls stop at backend validation,
// without launching a CLI. This exercises the real receiver, trust and manager.
func TestExternalGoalControlAllowsOnlyValidEmptyRequests(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "holder"}
	a, err := s.agents.Create(agent.AgentConfig{Name: "goal controls", Tool: agent.ToolClaude})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.agents.Store().UpsertPeer(ctx, &store.PeerRecord{DeviceID: "hub", Name: "Hub", URL: "http://hub.example:8080", Status: store.PeerStatusOnline}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = s.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "status", "clear", "budget", "resume", "start", "invalid", ""} {
		t.Run(action, func(t *testing.T) {
			q := externalChatTextRequest{SessionKey: a.ID + ":slack:C:T", GoalUserID: "UOWNER"}
			if action != "" {
				q.Goal = &agent.GoalRequest{Action: action}
			}
			if action == "budget" {
				n := int64(100)
				q.Goal.TokenBudget = &n
			}
			if action == "start" {
				q.Goal.Objective = "test"
			}
			body, _ := json.Marshal(q)
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			r.SetPathValue("id", a.ID)
			r = r.WithContext(auth.WithPrincipal(ctx, auth.Principal{Role: auth.RolePeer, PeerID: "hub"}))
			w := httptest.NewRecorder()
			s.handleExternalChatText(w, r)
			valid := action != "" && action != "invalid"
			if valid {
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "requires the native Codex") {
					t.Fatalf("valid control rejected before backend: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != 400 {
				t.Fatalf("invalid accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestGoalControlRemoteTransportPreservesEmptyMessageAndIdentity(t *testing.T) {
	seen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		var q externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		seen <- q
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	defer holder.Close()
	_, router, id := prepareRemoteExternalChat(t, holder.URL)
	ev, err := router.ChatOneShot(context.Background(), id, "", agent.OneShotOpts{SessionKey: id + ":slack:C:T", GoalUserID: "UOWNER", Goal: &agent.GoalRequest{Action: "pause", OperationID: "slack:C:M"}})
	if err != nil {
		t.Fatal(err)
	}
	for range ev {
	}
	q := <-seen
	if q.Message != "" || q.GoalUserID != "UOWNER" || q.Goal == nil || q.Goal.Action != "pause" || q.Goal.OperationID != "slack:C:M" {
		t.Fatalf("lost command: %+v", q)
	}
}

func TestGoalSteerRemoteTransportPreservesUser(t *testing.T) {
	seen := make(chan externalChatSteerRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		var q externalChatSteerRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		seen <- q
		writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	defer holder.Close()
	_, router, id := prepareRemoteExternalChat(t, holder.URL)
	if err := router.SteerOneShotAsUser(context.Background(), id, id+":slack:C:T", "reply", "UOWNER"); err != nil {
		t.Fatal(err)
	}
	q := <-seen
	if q.GoalUserID != "UOWNER" || q.Content != "reply" {
		t.Fatalf("lost identity: %+v", q)
	}
}

func TestRemoteGoalSteerOwnerRejectionIsTerminal(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		writeError(w, http.StatusForbidden, "goal_owner_forbidden", agent.ErrGoalOwnerForbidden.Error())
	}))
	defer holder.Close()
	_, router, id := prepareRemoteExternalChat(t, holder.URL)
	err := router.SteerOneShotAsUser(context.Background(), id, id+":slack:C:T", "reply", "UOTHER")
	if !errors.Is(err, agent.ErrGoalOwnerForbidden) {
		t.Fatalf("lost terminal rejection: %v", err)
	}
}
