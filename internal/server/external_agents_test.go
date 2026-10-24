package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

type fakeWhoIs struct {
	byAddr map[string]store.ExternalNodeIdentity
}

func (f *fakeWhoIs) fn(_ context.Context, remoteAddr string) (store.ExternalNodeIdentity, error) {
	if id, ok := f.byAddr[remoteAddr]; ok {
		return id, nil
	}
	return store.ExternalNodeIdentity{}, context.DeadlineExceeded
}

func newExternalTestServer(t *testing.T) (*Server, *fakeWhoIs, http.Handler) {
	t.Helper()
	srv, _, _, _ := newGroupDMHandlerTestServer(t)
	srv.external = newExternalState()
	w := &fakeWhoIs{byAddr: map[string]store.ExternalNodeIdentity{
		"100.64.0.5:4000": {StableNodeID: "nA", NodeName: "bot-a", TailnetIPs: []string{"100.64.0.5"}},
		"100.64.0.6:4000": {StableNodeID: "nB", NodeName: "bot-b", TailnetIPs: []string{"100.64.0.6"}},
	}}
	srv.SetExternalWhoIs(w.fn)
	return srv, w, srv.ExternalHandler()
}

func xcall(t *testing.T, h http.Handler, method, path, remote, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

func ownerCall(t *testing.T, h http.HandlerFunc, method, body string, pv map[string]string) map[string]any {
	t.Helper()
	rr := extCall(t, h, method, "/x", body, pv)
	if rr.Code >= 300 {
		t.Fatalf("%s: %d %s", method, rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func TestExternalPairAndAuth(t *testing.T) {
	srv, _, h := newExternalTestServer(t)
	created := ownerCall(t, srv.handleCreateExternalAgent, "POST", `{"name":"Bot A"}`, nil)
	id := created["id"].(string)
	pc := ownerCall(t, srv.handleExternalPairingCode, "POST", "", map[string]string{"id": id})
	code := pc["code"].(string)

	// unknown node: whois fails → 503, no pairing burnt
	if rr, _ := xcall(t, h, "POST", "/v1/pair", "100.64.0.9:1", "", `{"code":"`+code+`"}`); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown node pair = %d", rr.Code)
	}
	if rr, _ := xcall(t, h, "POST", "/v1/pair", "100.64.0.5:4000", "", `{"code":"WRONG-CODE-XXXX"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("wrong code = %d", rr.Code)
	}
	rr, out := xcall(t, h, "POST", "/v1/pair", "100.64.0.5:4000", "", `{"code":"`+code+`"}`)
	if rr.Code != 200 {
		t.Fatalf("pair = %d %s", rr.Code, rr.Body.String())
	}
	tok := out["token"].(string)
	// code is single-use
	if rr, _ := xcall(t, h, "POST", "/v1/pair", "100.64.0.5:4000", "", `{"code":"`+code+`"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("reuse = %d", rr.Code)
	}
	if !srv.externalDeniedAddr("100.64.0.5:443") || srv.externalDeniedAddr("100.64.0.6:443") {
		t.Fatal("denylist not applied to paired node only")
	}

	if rr, me := xcall(t, h, "GET", "/v1/me", "100.64.0.5:4000", tok, ""); rr.Code != 200 || me["id"] != id {
		t.Fatalf("me = %d %v", rr.Code, me)
	}
	if rr, _ := xcall(t, h, "GET", "/v1/me", "100.64.0.6:4000", tok, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("other node = %d", rr.Code)
	}
	if rr, _ := xcall(t, h, "GET", "/v1/me", "100.64.0.5:4000", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rr.Code)
	}

	ownerCall(t, srv.handlePatchExternalAgent, "PATCH", `{"enabled":false}`, map[string]string{"id": id})
	if rr, _ := xcall(t, h, "GET", "/v1/me", "100.64.0.5:4000", tok, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("disabled = %d", rr.Code)
	}
	ownerCall(t, srv.handlePatchExternalAgent, "PATCH", `{"enabled":true}`, map[string]string{"id": id})
	ownerCall(t, srv.handleRevokeExternalAgent, "POST", "", map[string]string{"id": id})
	if rr, _ := xcall(t, h, "GET", "/v1/me", "100.64.0.5:4000", tok, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked = %d", rr.Code)
	}

	// node stays denied after delete; unblock is refused while bound, ok after delete
	rr2 := extCall(t, srv.handleDeleteExternalNode, "DELETE", "/x", "", map[string]string{"node": "nA"})
	if rr2.Code != http.StatusConflict {
		t.Fatalf("unblock bound = %d", rr2.Code)
	}
	if rr := extCall(t, srv.handleDeleteExternalAgent, "DELETE", "/x", "", map[string]string{"id": id}); rr.Code != 204 {
		t.Fatalf("delete = %d", rr.Code)
	}
	if !srv.externalDeniedAddr("100.64.0.5:1") {
		t.Fatal("deleted agent's node must stay denied")
	}
	if rr := extCall(t, srv.handleDeleteExternalNode, "DELETE", "/x", "", map[string]string{"node": "nA"}); rr.Code != 204 {
		t.Fatalf("unblock = %d %s", rr.Code, rr.Body.String())
	}
	if srv.externalDeniedAddr("100.64.0.5:1") {
		t.Fatal("unblocked node still denied")
	}
}

func TestExternalPairRateLimit(t *testing.T) {
	_, _, h := newExternalTestServer(t)
	for i := 0; i < externalPairAttempts; i++ {
		xcall(t, h, "POST", "/v1/pair", "100.64.0.6:4000", "", `{"code":"AAAA-BBBB-CCCC"}`)
	}
	if rr, _ := xcall(t, h, "POST", "/v1/pair", "100.64.0.6:4000", "", `{"code":"AAAA-BBBB-CCCC"}`); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d", rr.Code)
	}
}
