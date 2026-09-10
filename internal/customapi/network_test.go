package customapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsAllowedIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"100.128.0.1", false},
		{"fd7a:115c:a1e0::1", true},
		{"fd7a:115c:a1e1::1", false},
		{"192.168.1.10", false},
		{"8.8.8.8", false},
	}
	for _, tc := range cases {
		if got := isAllowedIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isAllowedIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestLocalRelayForwardsAuthAndJoinsBasePath(t *testing.T) {
	const apiKey = "sk-unsloth-relay"
	var expectedHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/messages" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("Authorization=%q", got)
		}
		if got := r.Host; got != expectedHost {
			t.Errorf("Host=%q", got)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	expectedHost = strings.TrimPrefix(upstream.URL, "http://")

	relay, err := StartLocalRelay(context.Background(), upstream.URL+"/prefix", apiKey)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	req, _ := http.NewRequest(http.MethodPost, relay.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer attacker-controlled")
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestLocalRelayRejectsUpstreamRedirect(t *testing.T) {
	var redirectTargetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetHit.Store(true)
	}))
	defer target.Close()
	upstream := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer upstream.Close()

	relay, err := StartLocalRelay(context.Background(), upstream.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	resp, err := (&http.Client{Timeout: time.Second}).Get(relay.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", resp.StatusCode)
	}
	if redirectTargetHit.Load() {
		t.Fatal("redirect target was reached")
	}
}

func TestNewLocalOrTailnetClientRejectsPublicAddress(t *testing.T) {
	t.Parallel()
	_, err := NewLocalOrTailnetClient(context.Background(), "http://8.8.8.8:8888", 0)
	if err == nil {
		t.Fatal("expected public address to be rejected")
	}
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"ftp://localhost:8888",
		"http://user:pass@localhost:8888",
		"http://localhost:8888?token=secret",
	} {
		if _, err := ParseBaseURL(raw); err == nil {
			t.Errorf("ParseBaseURL(%q) unexpectedly succeeded", raw)
		}
	}
	if _, err := ParseBaseURL("https://peer.tailnet.ts.net:8888"); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
}
