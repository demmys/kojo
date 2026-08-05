package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

// newAttentionTestServer reuses the group-DM fixture (it already builds a
// real Manager with three agents) and hands back two distinct agents so
// the cross-agent authz cases have someone to impersonate.
func newAttentionTestServer(t *testing.T) (*Server, string, string, *agent.Agent) {
	t.Helper()
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)
	var other string
	for _, a := range srv.agents.List() {
		if a.ID != outsider.ID {
			other = a.ID
			break
		}
	}
	if other == "" {
		t.Fatal("fixture produced no second agent")
	}
	return srv, outsider.ID, other, outsider
}

func decodeAttention(t *testing.T, rr *httptest.ResponseRecorder) attentionResponse {
	t.Helper()
	var got attentionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return got
}

func postAttention(t *testing.T, srv *Server, agentID, body string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/attention", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/attention", bytes.NewBufferString(body))
	}
	r.SetPathValue("id", agentID)
	r = authedRequest(r, p)
	rr := httptest.NewRecorder()
	srv.handleRaiseAgentAttention(rr, r)
	return rr
}

func deleteAttention(t *testing.T, srv *Server, agentID string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentID+"/attention", nil)
	r.SetPathValue("id", agentID)
	r = authedRequest(r, p)
	rr := httptest.NewRecorder()
	srv.handleClearAgentAttention(rr, r)
	return rr
}

// TestAttention_RaiseThenClear covers the whole lifecycle: raising surfaces
// the reason on the manager, and the clear reports that something was
// actually retracted.
func TestAttention_RaiseThenClear(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	rr := postAttention(t, srv, ag.ID, `{"reason":"デプロイの承認が要る"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("raise status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := decodeAttention(t, rr)
	if !got.Attention || got.Reason != "デプロイの承認が要る" || got.At == 0 {
		t.Fatalf("raise response = %+v", got)
	}

	raised, reason, at := srv.agents.AttentionFor(ag.ID)
	if !raised || reason != "デプロイの承認が要る" || at.IsZero() {
		t.Fatalf("manager state: raised=%v reason=%q at=%v", raised, reason, at)
	}

	rr = deleteAttention(t, srv, ag.ID, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got = decodeAttention(t, rr); got.Attention || !got.Cleared {
		t.Fatalf("clear response = %+v", got)
	}
	if raised, _, _ := srv.agents.AttentionFor(ag.ID); raised {
		t.Fatal("attention still raised after clear")
	}
}

// TestAttention_ClearIsIdempotent — the UI clears on every chat open, so
// "nothing was set" must be a 200 with cleared=false, not an error.
func TestAttention_ClearIsIdempotent(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	rr := deleteAttention(t, srv, ag.ID, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := decodeAttention(t, rr); got.Cleared || got.Attention {
		t.Fatalf("response = %+v", got)
	}
}

// TestAttention_EmptyBody — a bare page with no note is legitimate and must
// not 400 on the missing/undecodable body. All three shapes a client can
// produce are covered, including the chunked request whose ContentLength
// is -1 (emptiness is only knowable after the read).
func TestAttention_EmptyBody(t *testing.T) {
	for _, body := range []string{"", "{}", "   \n"} {
		srv, _, _, ag := newAttentionTestServer(t)
		rr := postAttention(t, srv, ag.ID, body, auth.Principal{Role: auth.RoleOwner})
		if rr.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d, body = %s", body, rr.Code, rr.Body.String())
		}
		if got := decodeAttention(t, rr); !got.Attention || got.Reason != "" {
			t.Fatalf("body %q: response = %+v", body, got)
		}
	}
}

// TestAttention_ChunkedEmptyBody — a client that streams the request (no
// Content-Length) with nothing in it must be treated as "page me, no
// note", not as malformed JSON.
func TestAttention_ChunkedEmptyBody(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+ag.ID+"/attention", strings.NewReader(""))
	r.ContentLength = -1
	r.SetPathValue("id", ag.ID)
	r = authedRequest(r, auth.Principal{Role: auth.RoleOwner})
	rr := httptest.NewRecorder()
	srv.handleRaiseAgentAttention(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := decodeAttention(t, rr); !got.Attention {
		t.Fatalf("response = %+v", got)
	}
}

// TestAttention_MalformedBodyRejected — tolerating an *empty* body must
// not turn into tolerating garbage.
func TestAttention_MalformedBodyRejected(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	rr := postAttention(t, srv, ag.ID, `{"reason":`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if raised, _, _ := srv.agents.AttentionFor(ag.ID); raised {
		t.Fatal("a rejected request still raised a page")
	}
}

// TestAttention_ClearedAlwaysSerialised — "there was nothing to clear" is
// the informative answer, so the field must survive JSON encoding rather
// than being omitted as a zero value.
func TestAttention_ClearedAlwaysSerialised(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	rr := deleteAttention(t, srv, ag.ID, auth.Principal{Role: auth.RoleOwner})
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["cleared"]; !present {
		t.Fatalf("cleared omitted from the response: %s", rr.Body.String())
	}
}

// TestAttention_ForeignAgentForbidden — an agent token may only page on its
// own behalf; lighting up another agent's row is not allowed.
func TestAttention_ForeignAgentForbidden(t *testing.T) {
	srv, selfID, otherID, _ := newAttentionTestServer(t)

	rr := postAttention(t, srv, otherID, `{"reason":"x"}`, auth.Principal{Role: auth.RoleAgent, AgentID: selfID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if raised, _, _ := srv.agents.AttentionFor(otherID); raised {
		t.Fatal("foreign raise leaked into manager state")
	}

	rr = deleteAttention(t, srv, otherID, auth.Principal{Role: auth.RoleAgent, AgentID: selfID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("delete status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestAttention_SelfAgentAllowed — the normal path: the agent pages for
// itself with its own token.
func TestAttention_SelfAgentAllowed(t *testing.T) {
	srv, selfID, _, _ := newAttentionTestServer(t)

	rr := postAttention(t, srv, selfID, `{"reason":"見て"}`, auth.Principal{Role: auth.RoleAgent, AgentID: selfID})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if raised, reason, _ := srv.agents.AttentionFor(selfID); !raised || reason != "見て" {
		t.Fatalf("manager state: raised=%v reason=%q", raised, reason)
	}
}

// TestAttention_UnknownAgent404 keeps the endpoint from silently tracking
// pages for agents that don't exist.
func TestAttention_UnknownAgent404(t *testing.T) {
	srv, _, _, _ := newGroupDMHandlerTestServer(t)

	rr := postAttention(t, srv, "ag_nope", `{"reason":"x"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestAttention_LongReasonTruncated — the note renders inline in the
// dashboard row, so an overlong one is trimmed rather than rejected.
func TestAttention_LongReasonTruncated(t *testing.T) {
	srv, _, _, ag := newAttentionTestServer(t)

	long := strings.Repeat("あ", 500)
	rr := postAttention(t, srv, ag.ID, `{"reason":"`+long+`"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := decodeAttention(t, rr)
	if n := len([]rune(got.Reason)); n > 201 { // 200 + the ellipsis
		t.Fatalf("reason not truncated: %d runes", n)
	}
	if !strings.HasSuffix(got.Reason, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got.Reason)
	}
}
