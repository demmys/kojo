package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
)

// newAttachCacheTestServer wires the minimum Server the attach-cache
// handlers need: an agent store with two agents (so prefix isolation can be
// asserted) and a blob store rooted in a temp dir.
// The agents are created through Manager.Create (not a raw store insert) so
// they are present in the manager's in-memory map, which is what the handler
// resolves against. IDs are generated, so they are returned to the caller.
func newAttachCacheTestServer(t *testing.T, names ...string) (*Server, []string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mgr, err := agent.NewManager(slog.Default())
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ids := make([]string, 0, len(names))
	for _, name := range names {
		a, err := mgr.Create(agent.AgentConfig{Name: name})
		if err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
		ids = append(ids, a.ID)
	}
	return &Server{
		agents: mgr,
		logger: slog.Default(),
		blob:   blob.New(t.TempDir(), blob.WithRefs(blob.NewStoreRefs(mgr.Store(), "dev_test"))),
	}, ids
}

func putBlob(t *testing.T, srv *Server, path, body string) {
	t.Helper()
	if _, err := srv.blob.Put(blob.ScopeGlobal, path, strings.NewReader(body), blob.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func attachCacheCall(t *testing.T, srv *Server, method, agentID string) (*httptest.ResponseRecorder, attachCacheResponse) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/agents/"+agentID+"/attach-cache", nil)
	req.SetPathValue("id", agentID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		srv.handleGetAgentAttachCache(rec, req)
	} else {
		srv.handleDeleteAgentAttachCache(rec, req)
	}
	var out attachCacheResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// TestAttachCachePurge_OnlyTargetAgent is the load-bearing case: the purge
// must delete everything under the agent's attach prefix and nothing else —
// not another agent's attachments, and not the agent's own non-attach blobs
// (avatar, workspace files) which live under the same agents/{id}/ root.
func TestAttachCachePurge_OnlyTargetAgent(t *testing.T) {
	srv, ids := newAttachCacheTestServer(t, "target", "other")
	target, other := ids[0], ids[1]

	putBlob(t, srv, "agents/"+target+"/attach/m_1/a.txt", "aaaa")
	putBlob(t, srv, "agents/"+target+"/attach/m_2/b.txt", "bb")
	putBlob(t, srv, "agents/"+target+"/avatar.png", "keepme")
	putBlob(t, srv, "agents/"+other+"/attach/m_1/c.txt", "cccc")
	// Prefix-boundary decoy: an ID that starts with the target's would be
	// swept too if the trailing "/" ever fell out of agentAttachPrefix.
	putBlob(t, srv, "agents/"+target+"x/attach/m_1/d.txt", "dddd")

	// GET reports the cache without touching it.
	_, probe := attachCacheCall(t, srv, http.MethodGet, target)
	if probe.Deleted != 2 || probe.Bytes != 6 {
		t.Errorf("GET = %+v, want deleted=2 bytes=6", probe)
	}

	rec, res := attachCacheCall(t, srv, http.MethodDelete, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if res.Deleted != 2 || res.Bytes != 6 || res.Failed != 0 {
		t.Errorf("DELETE = %+v, want deleted=2 bytes=6 failed=0", res)
	}

	remaining, err := srv.blob.List(blob.ScopeGlobal, "agents/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(remaining))
	for _, o := range remaining {
		got = append(got, o.Path)
	}
	want := map[string]bool{
		"agents/" + target + "/avatar.png":        true,
		"agents/" + other + "/attach/m_1/c.txt":   true,
		"agents/" + target + "x/attach/m_1/d.txt": true,
	}
	if len(got) != len(want) {
		t.Fatalf("remaining = %v, want exactly %d blobs", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("blob %q was deleted or unexpected; want survivors %v", p, want)
		}
	}
}

// TestAttachCachePurge_DropsBlobRefs pins that the purge leaves the ref
// table in lock-step with the disk. A surviving row would point at a file
// that no longer exists — exactly the inconsistency the scrubber exists to
// repair, and not something a routine button press should manufacture.
func TestAttachCachePurge_DropsBlobRefs(t *testing.T) {
	srv, ids := newAttachCacheTestServer(t, "refs")
	id := ids[0]
	path := "agents/" + id + "/attach/m_1/a.txt"
	putBlob(t, srv, path, "aaaa")

	uri := blob.BuildURI(blob.ScopeGlobal, path)
	refs := blob.NewStoreRefs(srv.agents.Store(), "dev_test")
	if _, err := refs.Get(context.Background(), uri); err != nil {
		t.Fatalf("precondition: ref for %s missing: %v", uri, err)
	}

	if rec, _ := attachCacheCall(t, srv, http.MethodDelete, id); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rec.Code)
	}
	if _, err := refs.Get(context.Background(), uri); !errors.Is(err, blob.ErrRefNotFound) {
		t.Errorf("ref Get after purge = %v, want ErrRefNotFound", err)
	}
}

// TestAttachCachePurge_Idempotent pins that pressing the button twice is
// harmless and that an empty cache reports zeros rather than erroring.
func TestAttachCachePurge_Idempotent(t *testing.T) {
	srv, ids := newAttachCacheTestServer(t, "empty")
	id := ids[0]

	for i := 0; i < 2; i++ {
		rec, res := attachCacheCall(t, srv, http.MethodDelete, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("pass %d: status = %d, want 200", i, rec.Code)
		}
		if res.Deleted != 0 || res.Bytes != 0 {
			t.Errorf("pass %d: %+v, want zeros", i, res)
		}
	}
}

// TestAttachCachePurge_PeerForbidden pins the handler-level owner check.
// The policy layer admits RolePeer to the whole /api/v1/agents/ surface and
// this route is exempt from proxying, so the handler is the only thing
// standing between a paired peer and this device's attachment blobs.
func TestAttachCachePurge_PeerForbidden(t *testing.T) {
	srv, ids := newAttachCacheTestServer(t, "peer")
	id := ids[0]
	path := "agents/" + id + "/attach/m_1/a.txt"
	putBlob(t, srv, path, "aaaa")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+id+"/attach-cache", nil)
	req.SetPathValue("id", id)
	req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "dev_other"})
	rec := httptest.NewRecorder()
	srv.handleDeleteAgentAttachCache(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if _, err := srv.blob.Head(blob.ScopeGlobal, path); err != nil {
		t.Errorf("blob was removed despite 403: %v", err)
	}
}

// TestAttachCache_UnknownAgent404 keeps the purge from being usable as a
// blind blob-namespace sweep for an ID that has no agent behind it.
func TestAttachCache_UnknownAgent404(t *testing.T) {
	srv, _ := newAttachCacheTestServer(t)
	rec, _ := attachCacheCall(t, srv, http.MethodDelete, "ag_nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
