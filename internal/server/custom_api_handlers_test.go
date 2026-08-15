package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

func createCustomAPITestAgent(t *testing.T, srv *Server) *agent.Agent {
	t.Helper()
	a, err := srv.agents.Create(agent.AgentConfig{Name: "custom-test", Tool: "claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func TestValidatePeerAgentSyncRejectsOversizeCustomAPIKey(t *testing.T) {
	srv := newSTTTestServer(t)
	key := strings.Repeat("x", agent.CustomAPIKeyMaxBytes+1)
	req := &peerAgentSyncRequest{
		SourceDeviceID: "source-peer",
		OpID:           "op-oversize-key",
		Agent:          &store.AgentRecord{ID: "ag_safe"},
		CustomAPIKey:   &key,
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/peers/agent-sync", nil)
	rr := httptest.NewRecorder()
	if srv.validatePeerAgentSyncRequest(rr, httpReq, req,
		auth.Principal{Role: auth.RolePeer, PeerID: "source-peer"}) {
		t.Fatal("oversized custom key unexpectedly accepted")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCustomAPIKeyHandlersRoundTripWithoutReveal(t *testing.T) {
	srv := newSTTTestServer(t)
	a := createCustomAPITestAgent(t, srv)
	baseURL := "http://localhost:8080"
	if _, err := srv.agents.Update(a.ID, agent.AgentUpdateConfig{CustomBaseURL: &baseURL}); err != nil {
		t.Fatal(err)
	}

	setReq := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+a.ID+"/custom-api-key",
		strings.NewReader(`{"baseURL":"http://localhost:8080","apiKey":"sk-unsloth-secret"}`))
	setReq.SetPathValue("id", a.ID)
	setReq = authedRequest(setReq, auth.Principal{Role: auth.RoleOwner})
	setRR := httptest.NewRecorder()
	srv.handleSetCustomAPIKey(setRR, setReq)
	if setRR.Code != http.StatusOK {
		t.Fatalf("set status=%d body=%s", setRR.Code, setRR.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID+"/custom-api-key", nil)
	getReq.SetPathValue("id", a.ID)
	getReq = authedRequest(getReq, auth.Principal{Role: auth.RoleOwner})
	getRR := httptest.NewRecorder()
	srv.handleGetCustomAPIKey(getRR, getReq)
	if getRR.Code != http.StatusOK || !strings.Contains(getRR.Body.String(), `"configured":true`) {
		t.Fatalf("get status=%d body=%s", getRR.Code, getRR.Body.String())
	}
	if strings.Contains(getRR.Body.String(), "sk-unsloth") {
		t.Fatalf("GET revealed secret: %s", getRR.Body.String())
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+a.ID+"/custom-api-key", nil)
	delReq.SetPathValue("id", a.ID)
	delReq = authedRequest(delReq, auth.Principal{Role: auth.RoleOwner})
	delRR := httptest.NewRecorder()
	srv.handleDeleteCustomAPIKey(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", delRR.Code, delRR.Body.String())
	}
	key, err := agent.LoadCustomAPIKey(srv.agents.Credentials(), a.ID, "http://localhost:8080")
	if err != nil || key != "" {
		t.Fatalf("key after delete=%q err=%v", key, err)
	}
}

func TestSetCustomAPIKeyRejectsUnsavedBaseURL(t *testing.T) {
	srv := newSTTTestServer(t)
	a := createCustomAPITestAgent(t, srv)
	baseURL := "http://localhost:8080"
	if _, err := srv.agents.Update(a.ID, agent.AgentUpdateConfig{CustomBaseURL: &baseURL}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+a.ID+"/custom-api-key",
		strings.NewReader(`{"baseURL":"http://localhost:9999","apiKey":"new-secret"}`))
	req.SetPathValue("id", a.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleSetCustomAPIKey(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleCustomModelsUsesStoredBearerKey(t *testing.T) {
	const wantKey = "sk-unsloth-models"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("Authorization=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "unsloth/Qwen-test-GGUF"}},
		})
	}))
	defer upstream.Close()

	srv := newSTTTestServer(t)
	a := createCustomAPITestAgent(t, srv)
	baseURL := upstream.URL
	if _, err := srv.agents.Update(a.ID, agent.AgentUpdateConfig{CustomBaseURL: &baseURL}); err != nil {
		t.Fatal(err)
	}
	if err := agent.StoreCustomAPIKey(srv.agents.Credentials(), a.ID, upstream.URL, wantKey); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/"+a.ID+"/custom-models?baseURL="+url.QueryEscape(upstream.URL), nil)
	req.SetPathValue("id", a.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleCustomModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unsloth/Qwen-test-GGUF") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestHandleCustomModelsDoesNotSendStoredKeyToEditedURL(t *testing.T) {
	const storedKey = "sk-unsloth-stored"
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer upstream.Close()

	srv := newSTTTestServer(t)
	a := createCustomAPITestAgent(t, srv)
	configuredURL := "http://localhost:9876"
	if _, err := srv.agents.Update(a.ID, agent.AgentUpdateConfig{CustomBaseURL: &configuredURL}); err != nil {
		t.Fatal(err)
	}
	if err := agent.StoreCustomAPIKey(srv.agents.Credentials(), a.ID, configuredURL, storedKey); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/"+a.ID+"/custom-models?baseURL="+url.QueryEscape(upstream.URL), nil)
	req.SetPathValue("id", a.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleCustomModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotAuthorization != "" {
		t.Fatalf("stored key leaked to edited URL: %q", gotAuthorization)
	}
}

func TestHandleCustomModelsRejectsNonTailnetAddress(t *testing.T) {
	srv := newSTTTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/custom-models?baseURL=http://8.8.8.8:8888", nil)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleCustomModels(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
