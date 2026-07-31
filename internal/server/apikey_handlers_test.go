package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
)

func TestHandleGetAPIKeyReportsStoredAndEnvironmentAvailability(t *testing.T) {
	srv := newSTTTestServer(t)
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	if err := srv.agents.Credentials().SetToken("gemini", "", "", "api_key", "stored-gemini-key", time.Time{}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		provider    string
		configured  bool
		hasFallback bool
	}{
		{provider: "gemini", configured: true, hasFallback: false},
		{provider: "openai", configured: false, hasFallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/api-keys/"+tt.provider, nil)
			req.SetPathValue("provider", tt.provider)
			rr := httptest.NewRecorder()
			srv.handleGetAPIKey(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var got struct {
				Configured  bool `json:"configured"`
				HasFallback bool `json:"hasFallback"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Configured != tt.configured || got.HasFallback != tt.hasFallback {
				t.Fatalf("response = %+v, want configured=%v fallback=%v", got, tt.configured, tt.hasFallback)
			}
		})
	}
}

func TestHandleGetAPIKeyReportsFallbackWithoutCredentialStore(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-openai-key")
	srv := &Server{agents: &agent.Manager{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-keys/openai", nil)
	req.SetPathValue("provider", "openai")
	rr := httptest.NewRecorder()
	srv.handleGetAPIKey(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Configured  bool `json:"configured"`
		HasFallback bool `json:"hasFallback"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Configured || !got.HasFallback {
		t.Fatalf("response = %+v", got)
	}
}

func TestHandleSetAPIKeyRejectsOversizedBody(t *testing.T) {
	srv := newSTTTestServer(t)
	body := []byte(`{"apiKey":"` + string(bytes.Repeat([]byte("x"), 20<<10)) + `"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/api-keys/openai", bytes.NewReader(body))
	req.SetPathValue("provider", "openai")
	rr := httptest.NewRecorder()
	srv.handleSetAPIKey(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSetAPIKeyRejectsTrailingDataPastLimit(t *testing.T) {
	srv := newSTTTestServer(t)
	body := `{"apiKey":"valid"}` + string(bytes.Repeat([]byte(" "), 20<<10))
	req := httptest.NewRequest(http.MethodPut, "/api/v1/api-keys/openai", strings.NewReader(body))
	req.SetPathValue("provider", "openai")
	rr := httptest.NewRecorder()
	srv.handleSetAPIKey(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
