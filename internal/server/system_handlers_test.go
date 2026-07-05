package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

func newRestartRequest(p auth.Principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/restart", nil)
	return authedRequest(r, p)
}

func TestSystemRestart_ForbiddenForRegularAgent(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	srv.SetRestartTrigger(func() bool { return true })
	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSystemRestart_UnsupportedWithoutTrigger(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
}

func TestSystemRestart_PrivAgentTriggersAfterDrain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := newChunkedSyncTestServer(t)
	fired := make(chan struct{})
	srv.SetRestartTrigger(func() bool { close(fired); return true })

	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, newRestartRequest(auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	readJSONResponse(t, rr, &body)
	if body["status"] != "pending" {
		t.Fatalf("status field = %v, want pending", body["status"])
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("trigger did not fire after idle drain")
	}

	// Second request while pending → already_pending, trigger not re-armed.
	rr2 := httptest.NewRecorder()
	srv.handleSystemRestart(rr2, newRestartRequest(auth.Principal{Role: auth.RoleOwner}))
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("dup status = %d, want 202", rr2.Code)
	}
	var body2 map[string]any
	readJSONResponse(t, rr2, &body2)
	if body2["status"] != "already_pending" {
		t.Fatalf("dup status field = %v, want already_pending", body2["status"])
	}
}

func TestSystemRestart_WakeArmsMarker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := newChunkedSyncTestServer(t)
	a, err := srv.agents.Create(agent.AgentConfig{Name: "wake-test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	fired := make(chan struct{})
	srv.SetRestartTrigger(func() bool { close(fired); return true })

	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/restart",
		strings.NewReader(`{"wake":true}`))
	r = authedRequest(r, auth.Principal{Role: auth.RolePrivAgent, AgentID: a.ID})
	rr := httptest.NewRecorder()
	srv.handleSystemRestart(rr, r)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("trigger did not fire")
	}
	// The marker is armed AFTER the trigger returns (trigger-accepted
	// ordering), so poll briefly instead of reading immediately.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := srv.agents.Store().GetKV(context.Background(), "system", "restart_wake")
		if err == nil {
			if rec.Value != a.ID {
				t.Fatalf("marker agent = %q, want %q", rec.Value, a.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wake marker not written: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSystemRestart_WakeValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := newChunkedSyncTestServer(t)
	srv.SetRestartTrigger(func() bool { return true })

	cases := []struct {
		name string
		p    auth.Principal
		body string
		want int
	}{
		{"owner wake without agentId", auth.Principal{Role: auth.RoleOwner}, `{"wake":true}`, http.StatusBadRequest},
		{"agent wakes someone else", auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_self"}, `{"wake":true,"agentId":"ag_other"}`, http.StatusForbidden},
		{"unknown wake agent", auth.Principal{Role: auth.RoleOwner}, `{"wake":true,"agentId":"ag_nope"}`, http.StatusNotFound},
		{"malformed body", auth.Principal{Role: auth.RoleOwner}, `{"wake":`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/system/restart", strings.NewReader(c.body))
			r = authedRequest(r, c.p)
			rr := httptest.NewRecorder()
			srv.handleSystemRestart(rr, r)
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, c.want, rr.Body.String())
			}
			if srv.restartPending.Load() {
				t.Fatal("validation failure must not leave restartPending set")
			}
		})
	}
}
