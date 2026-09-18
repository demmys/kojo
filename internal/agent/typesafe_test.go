package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadTypeSafeAPIKeyPriority(t *testing.T) {
	cs := setupCredentialStore(t) // also points HOME at a temp dir
	t.Setenv("TYPESAFE_API_KEY", "")

	// Nothing configured → error.
	if _, err := LoadTypeSafeAPIKey(cs); err == nil {
		t.Fatal("expected error with nothing configured")
	}

	// 3. credentials file.
	dir := filepath.Join(os.Getenv("HOME"), ".config", "typesafe")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte("  file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadTypeSafeAPIKey(cs); err != nil || got != "file-key" {
		t.Fatalf("file: got (%q,%v)", got, err)
	}

	// 2. env beats file.
	t.Setenv("TYPESAFE_API_KEY", " env-key ")
	if got, err := LoadTypeSafeAPIKey(cs); err != nil || got != "env-key" {
		t.Fatalf("env: got (%q,%v)", got, err)
	}

	// 1. store beats env; whitespace-only stored key is ignored.
	if err := cs.SetToken("typesafe", "", "", "api_key", "   ", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadTypeSafeAPIKey(cs); err != nil || got != "env-key" {
		t.Fatalf("blank store: got (%q,%v)", got, err)
	}
	if err := cs.SetToken("typesafe", "", "", "api_key", "stored-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadTypeSafeAPIKey(cs); err != nil || got != "stored-key" {
		t.Fatalf("store: got (%q,%v)", got, err)
	}
	// nil store is tolerated.
	if got, err := LoadTypeSafeAPIKey(nil); err != nil || got != "env-key" {
		t.Fatalf("nil store: got (%q,%v)", got, err)
	}
}

func TestJevSystemOneRequestAndResponse(t *testing.T) {
	var gotAuth, gotCT, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.8,"b":0.2}}},"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer srv.Close()
	t.Setenv("TYPESAFE_BASE_URL", srv.URL+"/")

	resp, err := jevSystemOne(context.Background(), "k", map[string]string{"x": "y"}, map[string]jevQuestion{
		"q": {Type: "choice", Instructions: "pick", Criteria: map[string]string{"a": "A", "b": "B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" || gotCT != "application/json" || gotPath != "/v1/systemone" {
		t.Fatalf("request: auth=%q ct=%q path=%q", gotAuth, gotCT, gotPath)
	}
	if gotBody["model"] != typesafeDefaultModel {
		t.Fatalf("model = %v", gotBody["model"])
	}
	if st, _ := gotBody["state"].(map[string]any); st["x"] != "y" {
		t.Fatalf("state = %v", gotBody["state"])
	}
	if resp.Model != "jev-1.13.0" || resp.Answers["q"].Choice != "a" || resp.Answers["q"].Probabilities["b"] != 0.2 || resp.Usage.InputTokens != 12 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestJevSystemOneErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer bad":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":{"error_type":"authentication_error","message":"nope"}}`))
		case "Bearer empty":
			_, _ = w.Write([]byte(`{"model":"jev","answers":{}}`))
		default:
			_, _ = w.Write([]byte(`not json`))
		}
	}))
	defer srv.Close()
	t.Setenv("TYPESAFE_BASE_URL", srv.URL)
	q := map[string]jevQuestion{"q": {Type: "noul", Instructions: "?"}}

	if _, err := jevSystemOne(context.Background(), "", "s", q); err == nil {
		t.Fatal("missing key must error before any request")
	}
	if _, err := jevSystemOne(context.Background(), "k", "s", nil); err == nil {
		t.Fatal("no questions must error before any request")
	}
	if _, err := jevSystemOne(context.Background(), "bad", "s", q); err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("403: err = %v", err)
	}
	if _, err := jevSystemOne(context.Background(), "empty", "s", q); err == nil {
		t.Fatal("empty answers must error")
	}
	if _, err := jevSystemOne(context.Background(), "junk", "s", q); err == nil {
		t.Fatal("non-JSON must error")
	}
	// Context cancellation propagates.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := jevSystemOne(ctx, "k", "s", q); err == nil {
		t.Fatal("cancelled context must error")
	}
}
