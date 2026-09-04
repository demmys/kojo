package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func llamaPropsServer(t *testing.T, nCtx int, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"default_generation_settings":{"n_ctx":%d},"total_slots":4}`, nCtx)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func forgetContextWindowCache(base string) {
	root := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	customContextWindowMu.Lock()
	delete(customContextWindowCache, root)
	customContextWindowMu.Unlock()
}

func TestProbeCustomContextWindow_LlamaCppProps(t *testing.T) {
	hits := 0
	srv := llamaPropsServer(t, 65536, &hits)
	forgetContextWindowCache(srv.URL)

	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 65536 {
		t.Fatalf("window = %d, want 65536", got)
	}
	// The second call is served from cache, not from the endpoint.
	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 65536 {
		t.Fatalf("cached window = %d, want 65536", got)
	}
	if hits != 1 {
		t.Errorf("endpoint hits = %d, want 1", hits)
	}
}

// kojo stores the server root, and codex is handed the "/v1" form. Either
// spelling has to reach the same /props URL and the same cache entry.
func TestProbeCustomContextWindow_StripsV1Suffix(t *testing.T) {
	hits := 0
	srv := llamaPropsServer(t, 8192, &hits)
	forgetContextWindowCache(srv.URL)

	if got := probeCustomContextWindow(context.Background(), srv.URL+"/v1", nil); got != 8192 {
		t.Fatalf("window = %d, want 8192", got)
	}
	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 8192 {
		t.Fatalf("window via root = %d, want 8192", got)
	}
	if hits != 1 {
		t.Errorf("endpoint hits = %d, want 1", hits)
	}
}

// An endpoint that does not implement /props leaves codex on its own default
// rather than being configured with a guess.
func TestProbeCustomContextWindow_UnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	forgetContextWindowCache(srv.URL)

	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 0 {
		t.Errorf("window = %d, want 0", got)
	}
}

func TestProbeCustomContextWindow_EmptyBase(t *testing.T) {
	if got := probeCustomContextWindow(context.Background(), "   ", nil); got != 0 {
		t.Errorf("window = %d, want 0", got)
	}
}

// A cancelled parent context must not stall the turn on the probe.
func TestProbeCustomContextWindow_CancelledContext(t *testing.T) {
	srv := llamaPropsServer(t, 65536, nil)
	forgetContextWindowCache(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := probeCustomContextWindow(ctx, srv.URL, nil); got != 0 {
		t.Fatalf("window = %d, want 0", got)
	}
	// A cancelled turn says nothing about the endpoint, so the next turn
	// probes again instead of reusing the failure.
	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 65536 {
		t.Errorf("window after cancellation = %d, want 65536", got)
	}
}

func TestProbeCustomContextWindow_CacheExpires(t *testing.T) {
	hits := 0
	srv := llamaPropsServer(t, 4096, &hits)
	root := strings.TrimRight(srv.URL, "/")
	forgetContextWindowCache(root)

	if got := probeCustomContextWindow(context.Background(), root, nil); got != 4096 {
		t.Fatalf("window = %d, want 4096", got)
	}
	customContextWindowMu.Lock()
	customContextWindowCache[root] = customContextWindowEntry{
		window:  4096,
		fetched: time.Now().Add(-2 * customContextWindowCacheTTL),
	}
	customContextWindowMu.Unlock()

	if got := probeCustomContextWindow(context.Background(), root, nil); got != 4096 {
		t.Fatalf("window after expiry = %d, want 4096", got)
	}
	if hits != 2 {
		t.Errorf("endpoint hits = %d, want 2", hits)
	}
}

// A brief outage must not cost compaction for the full success TTL.
func TestProbeCustomContextWindow_FailureExpiresQuickly(t *testing.T) {
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":16384}}`)
	}))
	defer srv.Close()
	root := strings.TrimRight(srv.URL, "/")
	forgetContextWindowCache(root)

	if got := probeCustomContextWindow(context.Background(), root, nil); got != 0 {
		t.Fatalf("window while down = %d, want 0", got)
	}
	up = true
	// Still inside the failure TTL, so the cached miss holds.
	if got := probeCustomContextWindow(context.Background(), root, nil); got != 0 {
		t.Fatalf("window inside failure TTL = %d, want 0", got)
	}
	customContextWindowMu.Lock()
	customContextWindowCache[root] = customContextWindowEntry{
		window:  0,
		fetched: time.Now().Add(-2 * customContextWindowFailureTTL),
	}
	customContextWindowMu.Unlock()

	if got := probeCustomContextWindow(context.Background(), root, nil); got != 16384 {
		t.Errorf("window after failure TTL = %d, want 16384", got)
	}
}

// The base URL is operator supplied and not restricted to loopback, so a
// redirect it serves must not send kojo's own process anywhere else.
func TestProbeCustomContextWindow_DoesNotFollowRedirect(t *testing.T) {
	target := llamaPropsServer(t, 65536, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/props", http.StatusFound)
	}))
	defer srv.Close()
	forgetContextWindowCache(srv.URL)

	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 0 {
		t.Errorf("window = %d, want 0 (redirect must not be followed)", got)
	}
}

// A body far larger than any real /props response is cut off rather than read
// into memory whole.
func TestProbeCustomContextWindow_OversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"junk":"`)
		chunk := strings.Repeat("x", 64*1024)
		for i := 0; i < 64; i++ {
			fmt.Fprint(w, chunk)
		}
		fmt.Fprint(w, `","default_generation_settings":{"n_ctx":65536}}`)
	}))
	defer srv.Close()
	forgetContextWindowCache(srv.URL)

	if got := probeCustomContextWindow(context.Background(), srv.URL, nil); got != 0 {
		t.Errorf("window = %d, want 0", got)
	}
}

// Concurrent turns against one endpoint must agree, and must not be left with
// a torn cache entry.
func TestProbeCustomContextWindow_Concurrent(t *testing.T) {
	srv := llamaPropsServer(t, 32768, nil)
	forgetContextWindowCache(srv.URL)

	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			results <- probeCustomContextWindow(context.Background(), srv.URL, nil)
		}()
	}
	for i := 0; i < 8; i++ {
		if got := <-results; got != 32768 {
			t.Errorf("concurrent window = %d, want 32768", got)
		}
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("http://user:hunter2@example.com:8190"); strings.Contains(got, "hunter2") || strings.Contains(got, "user") {
		t.Errorf("redactURL = %q, still contains userinfo", got)
	}
	// A token pasted as the username alone is the secret, so it goes too.
	if got := redactURL("http://sk-secret-token@example.com:8190"); strings.Contains(got, "sk-secret-token") {
		t.Errorf("redactURL = %q, still contains the token", got)
	}
	if got := redactURL("http://127.0.0.1:8190"); got != "http://127.0.0.1:8190" {
		t.Errorf("redactURL = %q, want it unchanged", got)
	}
}

func TestCustomCodexContextOverrides(t *testing.T) {
	got := customCodexContextOverrides(65536)
	want := []string{
		"model_context_window=65536",
		"model_auto_compact_token_limit=45871",
	}
	if len(got) != len(want) {
		t.Fatalf("overrides = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("overrides[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A context small enough to round the compaction limit to zero still yields a
// limit codex accepts.
func TestCustomCodexContextOverrides_TinyWindow(t *testing.T) {
	got := customCodexContextOverrides(1)
	if got[1] != "model_auto_compact_token_limit=1" {
		t.Errorf("limit override = %q, want model_auto_compact_token_limit=1", got[1])
	}
}
