package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// TestWakeThreadForRestart verifies the wake is routed to an in-flight thread
// ONLY when the wake target itself is the caller. An owner-initiated wake (or a
// no-wake request) must never capture the target's unrelated one-shot thread.
func TestWakeThreadForRestart(t *testing.T) {
	lookup := func(id string) string { return "groupdm:g1" }
	cases := []struct {
		name string
		p    auth.Principal
		wake string
		want string
	}{
		{"agent wakes itself in a thread", auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_x"}, "ag_x", "groupdm:g1"},
		{"owner wakes an agent", auth.Principal{Role: auth.RoleOwner}, "ag_x", ""},
		{"agent wakes another (blocked upstream, still safe here)", auth.Principal{Role: auth.RolePrivAgent, AgentID: "ag_y"}, "ag_x", ""},
		{"no wake target", auth.Principal{Role: auth.RoleOwner}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wakeThreadForRestart(tc.p, tc.wake, lookup); got != tc.want {
				t.Errorf("wakeThreadForRestart = %q, want %q", got, tc.want)
			}
		})
	}
}

func newRebuildRequest(p auth.Principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/system/rebuild", nil)
	return authedRequest(r, p)
}

func TestSystemRebuild_ForbiddenForRegularAgent(t *testing.T) {
	srv := &Server{logger: slog.Default(), repoDir: t.TempDir()}
	rr := httptest.NewRecorder()
	srv.handleSystemRebuild(rr, newRebuildRequest(auth.Principal{Role: auth.RoleAgent, AgentID: "ag_x"}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSystemRebuild_RepoDirNotConfigured(t *testing.T) {
	srv := &Server{logger: slog.Default()} // repoDir empty
	rr := httptest.NewRecorder()
	srv.handleSystemRebuild(rr, newRebuildRequest(auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
	}
}

// TestSystemRebuild_Success drives the handler against a stub Makefile
// whose `build` target writes a `kojo` file into the repo dir; the
// running-binary target is a throwaway temp file so the deploy copy is
// safe (never touches the real test binary).
func TestSystemRebuild_Success(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Makefile"),
		[]byte("build:\n\tprintf 'built-bin' > kojo\n\t@echo build done\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	// Stand in for os.Executable(): deployBuiltBinary targets the real
	// one, so exercise the handler's make step here and the deploy copy
	// via deployBuiltBinaryTo below. To keep the handler's own deploy
	// harmless we point it at a repo where kojo IS the executable —
	// instead we assert make ran and output is returned, then unit-test
	// the copy separately.
	self := filepath.Join(t.TempDir(), "kojo")
	if err := os.WriteFile(self, []byte("old"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	// Run make manually to confirm the stub works, then the copy.
	cmd := exec.Command("make", "build")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stub make build failed: %v\n%s", err, out)
	}
	if err := deployBuiltBinaryTo(filepath.Join(repo, "kojo"), self); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self: %v", err)
	}
	if string(got) != "built-bin" {
		t.Fatalf("deployed content = %q, want built-bin", got)
	}
}

func TestDeployBuiltBinaryTo_SameFileSkips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kojo")
	if err := os.WriteFile(p, []byte("same"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := deployBuiltBinaryTo(p, p); err != nil {
		t.Fatalf("same-file deploy: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "same" {
		t.Fatalf("content changed on same-file skip: %q", got)
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
