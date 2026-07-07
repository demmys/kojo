package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
)

// TestHandleAnswerAgentQuestion_NotBusy verifies a 409 not_busy when the agent
// has no turn in progress.
func TestHandleAnswerAgentQuestion_NotBusy(t *testing.T) {
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"requestId":"req-1","answers":{"色":"青"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+outsider.ID+"/answer", body)
	req.SetPathValue("id", outsider.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleAnswerAgentQuestion(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAnswerAgentQuestion_MissingRequestID verifies a 400 when requestId
// is blank.
func TestHandleAnswerAgentQuestion_MissingRequestID(t *testing.T) {
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"requestId":"  ","answers":{"色":"青"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+outsider.ID+"/answer", body)
	req.SetPathValue("id", outsider.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleAnswerAgentQuestion(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAnswerAgentQuestion_MissingAnswers verifies a 400 when neither
// answers nor deny is supplied.
func TestHandleAnswerAgentQuestion_MissingAnswers(t *testing.T) {
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"requestId":"req-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+outsider.ID+"/answer", body)
	req.SetPathValue("id", outsider.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleAnswerAgentQuestion(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
