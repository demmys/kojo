package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/slackbot"
	"github.com/loppo-llc/kojo/internal/store"
)

func seedTransferSkips(t *testing.T, srv *Server, agentID, generation string) {
	t.Helper()
	_, err := srv.agents.Store().UpdateAgent(context.Background(), agentID, "", func(rec *store.AgentRecord) error {
		if rec.Settings == nil {
			rec.Settings = map[string]any{}
		}
		rec.Settings["lastTransferSkips"] = []any{map[string]any{
			"path": "sessions/old.jsonl", "reason": "capacity", "sizeBytes": float64(1024),
		}}
		rec.Settings["lastTransferSkipsOpID"] = generation
		delete(rec.Settings, "lastTransferSkipsDismissedGeneration")
		return nil
	})
	if err != nil {
		t.Fatalf("seed transfer skips: %v", err)
	}
}

func dismissTransferSkips(t *testing.T, srv *Server, agentID, generation string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+
		"/transfer-skips/dismiss?generation="+generation, nil)
	r.SetPathValue("id", agentID)
	r = authedRequest(r, p)
	rr := httptest.NewRecorder()
	srv.handleDismissTransferSkips(rr, r)
	return rr
}

func decodeDismissTransferSkips(t *testing.T, rr *httptest.ResponseRecorder) dismissTransferSkipsResponse {
	t.Helper()
	var got dismissTransferSkipsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return got
}

func TestDismissTransferSkipsClearsStoreAndLocalCache(t *testing.T) {
	srv, _, _, ag := newGroupDMHandlerTestServer(t)
	seedTransferSkips(t, srv, ag.ID, "op-old")
	if err := srv.agents.ReloadAgentFromStore(ag.ID); err != nil {
		t.Fatalf("reload seeded agent: %v", err)
	}
	if got, ok := srv.agents.Get(ag.ID); !ok || len(got.LastTransferSkips) != 1 {
		t.Fatalf("seeded cache = %+v, ok=%v", got, ok)
	}

	rr := dismissTransferSkips(t, srv, ag.ID, "op-old", auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := decodeDismissTransferSkips(t, rr); !got.Dismissed {
		t.Fatalf("response = %+v", got)
	}
	if got, ok := srv.agents.Get(ag.ID); !ok || len(got.LastTransferSkips) != 0 {
		t.Fatalf("cache after dismiss = %+v, ok=%v", got, ok)
	}
	rec, err := srv.agents.Store().GetAgent(context.Background(), ag.ID)
	if err != nil {
		t.Fatalf("get stored agent: %v", err)
	}
	if got := rec.Settings["lastTransferSkipsDismissedGeneration"]; got != "op-old" {
		t.Fatalf("dismissed generation = %v, settings=%+v", got, rec.Settings)
	}
}

func TestDismissTransferSkipsIsIdempotent(t *testing.T) {
	srv, _, _, ag := newGroupDMHandlerTestServer(t)
	seedTransferSkips(t, srv, ag.ID, "op-idempotent")

	rr := dismissTransferSkips(t, srv, ag.ID, "op-idempotent", auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := decodeDismissTransferSkips(t, rr); !got.Dismissed {
		t.Fatalf("first response = %+v, want dismissed=true", got)
	}
	rr = dismissTransferSkips(t, srv, ag.ID, "op-idempotent", auth.Principal{Role: auth.RoleOwner})
	if got := decodeDismissTransferSkips(t, rr); got.Dismissed {
		t.Fatalf("response = %+v, want dismissed=false", got)
	}
}

func TestDismissTransferSkipsAuthAndNotFound(t *testing.T) {
	srv, _, _, ag := newGroupDMHandlerTestServer(t)
	other := ""
	for _, candidate := range srv.agents.List() {
		if candidate.ID != ag.ID {
			other = candidate.ID
			break
		}
	}
	if other == "" {
		t.Fatal("missing second agent")
	}

	rr := dismissTransferSkips(t, srv, other, "op", auth.Principal{Role: auth.RoleAgent, AgentID: ag.ID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rr = dismissTransferSkips(t, srv, ag.ID, "op", auth.Principal{Role: auth.RoleAgent, AgentID: ag.ID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("self-agent status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rr = dismissTransferSkips(t, srv, "ag_missing", "op", auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestDismissTransferSkipsRejectsStaleGeneration(t *testing.T) {
	srv, _, _, ag := newGroupDMHandlerTestServer(t)
	seedTransferSkips(t, srv, ag.ID, "op-new")
	if err := srv.agents.ReloadAgentFromStore(ag.ID); err != nil {
		t.Fatal(err)
	}

	rr := dismissTransferSkips(t, srv, ag.ID, "op-old", auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, ok := srv.agents.Get(ag.ID); !ok || got.LastTransferSkipsGeneration != "op-new" || len(got.LastTransferSkips) != 1 {
		t.Fatalf("new warning was hidden: got=%+v ok=%v", got, ok)
	}
}

func TestRemoteTransferSkipDismissStaysOnHub(t *testing.T) {
	const agentID = "ag_remote_skips"
	srv := newRemoteAgentPatchServer(t, agentID, store.PeerStatusOnline)
	seedTransferSkips(t, srv, agentID, "op-remote")
	h := srv.remoteAgentProxyMiddleware(http.HandlerFunc(srv.handleDismissTransferSkips))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+
		"/transfer-skips/dismiss?generation=op-remote", nil)
	r.SetPathValue("id", agentID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := decodeDismissTransferSkips(t, rr); !got.Dismissed {
		t.Fatalf("response = %+v", got)
	}
	if got := srv.agents.GetRemote(agentID); got == nil || len(got.LastTransferSkips) != 0 {
		t.Fatalf("remote mirror still shows warning: %+v", got)
	}
}

func TestApplyTransferSkipsRetryPreservesDismissal(t *testing.T) {
	srv, _, _, ag := newGroupDMHandlerTestServer(t)
	srv.slackHub = &slackbot.Hub{}
	seedTransferSkips(t, srv, ag.ID, "op-retry")
	if _, err := srv.agents.Store().UpdateAgent(context.Background(), ag.ID, "", func(rec *store.AgentRecord) error {
		rec.Settings["slackBot"] = map[string]any{"enabled": true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr := dismissTransferSkips(t, srv, ag.ID, "op-retry", auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d, body = %s", rr.Code, rr.Body.String())
	}

	current, err := srv.agents.Store().GetAgent(context.Background(), ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	incoming := *current
	incoming.Settings = make(map[string]any, len(current.Settings))
	for key, value := range current.Settings {
		incoming.Settings[key] = value
	}
	delete(incoming.Settings, "lastTransferSkips")
	delete(incoming.Settings, "lastTransferSkipsOpID")
	delete(incoming.Settings, "lastTransferSkipsDismissedGeneration")
	incoming.Settings["slackBot"] = map[string]any{"enabled": false}
	skips := []agent.SkippedSessionFile{{Path: "sessions/old.jsonl", Reason: "capacity", SizeBytes: 1024}}
	if err := srv.applyReceiverOwnedSettingsToSyncRecord(context.Background(), &incoming, "op-retry", skips); err != nil {
		t.Fatal(err)
	}
	if got := incoming.Settings["lastTransferSkipsDismissedGeneration"]; got != "op-retry" {
		t.Fatalf("same-op retry lost acknowledgement: settings=%+v", incoming.Settings)
	}
	if incoming.ETag != current.ETag || incoming.Version != current.Version {
		t.Fatalf("same-op retry changed metadata: got etag=%q version=%d, want etag=%q version=%d",
			incoming.ETag, incoming.Version, current.ETag, current.Version)
	}

	if err := srv.applyReceiverOwnedSettingsToSyncRecord(context.Background(), &incoming, "op-new", skips); err != nil {
		t.Fatal(err)
	}
	if _, ok := incoming.Settings["lastTransferSkipsDismissedGeneration"]; ok {
		t.Fatalf("new op preserved old acknowledgement: settings=%+v", incoming.Settings)
	}
}
