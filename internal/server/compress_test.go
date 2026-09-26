package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                  false,
		"gzip":              true,
		"br, gzip, deflate": true,
		"gzip;q=0":          false,
		"gzip; q=0.0":       false,
		"gzip;q=0.5":        true,
		"*":                 true,
		"identity":          false,
		"*;q=0, gzip;q=1":   true,
		"gzip;q=0, *":       false,
	}
	for ae, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		if got := acceptsGzip(r); got != want {
			t.Errorf("%q: got %v want %v", ae, got, want)
		}
	}
}

func newStaticTestMux() *http.ServeMux {
	js := strings.Repeat("console.log('hello world');\n", 200)
	fsys := fstest.MapFS{
		"index.html":          {Data: []byte("<html>" + strings.Repeat("<div>x</div>", 200) + "</html>")},
		"assets/index-abc.js": {Data: []byte(js)},
		"assets/tiny-abc.js":  {Data: []byte("x=1")},
		"assets/logo-abc.png": {Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 1000)},
	}
	s := &Server{}
	mux := http.NewServeMux()
	s.registerStaticFiles(mux, fsys)
	return mux
}

func TestStaticGzip(t *testing.T) {
	mux := newStaticTestMux()

	r := httptest.NewRequest("GET", "/assets/index-abc.js", nil)
	r.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	res := w.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip, got %q", res.Header.Get("Content-Encoding"))
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("content-type %q", res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("cache-control %q", res.Header.Get("Cache-Control"))
	}
	if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
		t.Errorf("vary missing")
	}
	body := w.Body.Bytes()
	if !strings.HasPrefix(string(gunzip(t, body)), "console.log") {
		t.Errorf("bad body")
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no etag")
	}

	// Conditional request -> 304.
	r = httptest.NewRequest("GET", "/assets/index-abc.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d", w.Code)
	}

	// No Accept-Encoding -> identity, still Vary.
	r = httptest.NewRequest("GET", "/assets/index-abc.js", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(w.Body.String(), "console.log") {
		t.Errorf("identity response expected")
	}
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Errorf("vary missing on identity")
	}

	// Range -> identity 206.
	r = httptest.NewRequest("GET", "/assets/index-abc.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Range", "bytes=0-6")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Header().Get("Content-Encoding") != "" || w.Body.String() != "console" {
		t.Errorf("range: code=%d ce=%q body=%q", w.Code, w.Header().Get("Content-Encoding"), w.Body.String())
	}

	// Tiny / non-compressible files -> identity.
	for _, p := range []string{"/assets/tiny-abc.js", "/assets/logo-abc.png"} {
		r = httptest.NewRequest("GET", p, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s should not be compressed", p)
		}
	}

	// SPA fallback -> gzipped index.html with no-cache.
	r = httptest.NewRequest("GET", "/agents/foo", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("spa fallback: ce=%q cc=%q", w.Header().Get("Content-Encoding"), w.Header().Get("Cache-Control"))
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("spa content-type %q", w.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(string(gunzip(t, w.Body.Bytes())), "<html>") {
		t.Errorf("spa body")
	}
}

func serveAPI(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Accept-Encoding", "gzip")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var bigJSON = `{"items":"` + strings.Repeat("abcdefgh", 500) + `"}`

func TestGzipAPIMiddlewareCompressesJSON(t *testing.T) {
	h := gzipAPIMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999999")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, bigJSON)
	}))
	w := serveAPI(h, "GET", "/api/v1/agents", nil)
	if w.Code != 201 || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("code=%d ce=%q", w.Code, w.Header().Get("Content-Encoding"))
	}
	if w.Header().Get("ETag") != `"abc"` {
		t.Errorf("etag must be preserved, got %q", w.Header().Get("ETag"))
	}
	if w.Header().Get("Content-Length") != "" {
		t.Errorf("content-length should be dropped")
	}
	if string(gunzip(t, w.Body.Bytes())) != bigJSON {
		t.Errorf("roundtrip mismatch")
	}

	// No Accept-Encoding: identity.
	r := httptest.NewRequest("GET", "/api/v1/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != bigJSON {
		t.Errorf("identity expected without Accept-Encoding")
	}
}

func TestGzipAPIMiddlewarePassthrough(t *testing.T) {
	jsonH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bigJSON)
	})
	h := gzipAPIMiddleware(jsonH)
	for _, tc := range []struct {
		name, method, path string
		hdr                map[string]string
	}{
		{"small-excluded-post", "POST", "/api/v1/agents", nil},
		{"ws", "GET", "/api/v1/ws", map[string]string{"Upgrade": "websocket"}},
		{"events", "GET", "/api/v1/events", nil},
		{"peers", "GET", "/api/v1/peers/blobs/x", nil},
		{"raw", "GET", "/api/v1/files/raw", nil},
		{"blob", "GET", "/api/v1/blob/global/x.json", nil},
		{"mcp", "GET", "/api/v1/agents/a/mcp", nil},
		{"range", "GET", "/api/v1/agents", map[string]string{"Range": "bytes=0-1"}},
		{"sse-accept", "GET", "/api/v1/agents", map[string]string{"Accept": "text/event-stream"}},
		{"non-api", "GET", "/index.html", nil},
	} {
		w := serveAPI(h, tc.method, tc.path, tc.hdr)
		if w.Header().Get("Content-Encoding") != "" || w.Body.String() != bigJSON {
			t.Errorf("%s: should pass through", tc.name)
		}
	}

	cases := map[string]http.HandlerFunc{
		"small": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		},
		"already-encoded": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, bigJSON)
		},
		"binary": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, bigJSON)
		},
		"sse": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, bigJSON)
		},
		"flush-early": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, bigJSON[1:])
		},
		"not-modified": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		},
	}
	for name, fn := range cases {
		w := serveAPI(gzipAPIMiddleware(fn), "GET", "/api/v1/x", nil)
		ce := w.Header().Get("Content-Encoding")
		if name == "already-encoded" {
			if ce != "gzip" || w.Body.String() != bigJSON {
				t.Errorf("%s: body/header altered", name)
			}
			continue
		}
		if ce != "" {
			t.Errorf("%s: unexpectedly compressed", name)
		}
		if name == "not-modified" {
			if w.Code != 304 {
				t.Errorf("304 lost: %d", w.Code)
			}
			continue
		}
		if name != "small" && w.Body.String() != bigJSON {
			t.Errorf("%s: body mismatch", name)
		}
	}
}

func TestGzipAPIMiddlewareFlushAfterStart(t *testing.T) {
	h := gzipAPIMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bigJSON)
		http.NewResponseController(w).Flush()
		_, _ = io.WriteString(w, bigJSON)
	}))
	w := serveAPI(h, "GET", "/api/v1/x", nil)
	if w.Header().Get("Content-Encoding") != "gzip" || string(gunzip(t, w.Body.Bytes())) != bigJSON+bigJSON {
		t.Errorf("flush after start broken")
	}
}

func TestGzipAPIMiddlewareRealServer(t *testing.T) {
	srv := httptest.NewServer(gzipAPIMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bigJSON)
	})))
	defer srv.Close()
	// Go's transport transparently decompresses when it added the header itself.
	res, err := http.Get(srv.URL + "/api/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !res.Uncompressed || string(b) != bigJSON {
		t.Errorf("uncompressed=%v len=%d", res.Uncompressed, len(b))
	}
}
