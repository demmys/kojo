package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gzipMinSize is the smallest body worth compressing. Below this the
// gzip header/trailer overhead and CPU cost outweigh the saving.
const gzipMinSize = 1024

// acceptsGzip reports whether the request's Accept-Encoding permits
// gzip. An explicit "gzip" entry takes precedence over "*"; q=0 forbids.
func acceptsGzip(r *http.Request) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "gzip" && name != "*" {
				continue
			}
			q := 1.0
			for _, prm := range strings.Split(params, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(prm), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					}
				}
			}
			if name == "gzip" {
				gzipQ = q
			} else {
				starQ = q
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

func addVary(h http.Header, v string) {
	for _, cur := range h.Values("Vary") {
		for _, p := range strings.Split(cur, ",") {
			if strings.EqualFold(strings.TrimSpace(p), v) {
				return
			}
		}
	}
	h.Add("Vary", v)
}

// ---------------------------------------------------------------------
// Static assets: gzip each compressible embedded file once (lazily, on
// first request) and keep the result in memory. The embedded FS is
// immutable for the process lifetime, so the cache never needs
// invalidation.
// ---------------------------------------------------------------------

var staticCompressibleExt = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".json": true,
	".svg": true, ".txt": true, ".map": true, ".webmanifest": true,
	".xml": true, ".ico": true, ".ttf": true, ".otf": true, ".wasm": true,
}

type gzEntry struct {
	once sync.Once
	data []byte // nil when compression is not worthwhile
	etag string
}

type staticGzipCache struct {
	fsys    fs.FS
	entries sync.Map // path -> *gzEntry
}

func newStaticGzipCache(fsys fs.FS) *staticGzipCache {
	return &staticGzipCache{fsys: fsys}
}

func (c *staticGzipCache) get(name string) *gzEntry {
	v, _ := c.entries.LoadOrStore(name, &gzEntry{})
	e := v.(*gzEntry)
	e.once.Do(func() {
		raw, err := fs.ReadFile(c.fsys, name)
		if err != nil || len(raw) < gzipMinSize {
			return
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(raw); err != nil {
			return
		}
		if err := zw.Close(); err != nil {
			return
		}
		if buf.Len() >= len(raw) {
			return
		}
		sum := sha256.Sum256(raw)
		e.data = buf.Bytes()
		e.etag = `"` + hex.EncodeToString(sum[:8]) + `-gz"`
	})
	return e
}

// serve writes the gzip variant of name if the client accepts it and a
// worthwhile variant exists. It returns false when the caller should
// fall back to the identity response. Range requests are always served
// uncompressed so byte offsets refer to the real file.
func (c *staticGzipCache) serve(w http.ResponseWriter, r *http.Request, name string) bool {
	if !staticCompressibleExt[strings.ToLower(path.Ext(name))] {
		return false
	}
	addVary(w.Header(), "Accept-Encoding")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Range") != "" || !acceptsGzip(r) {
		return false
	}
	e := c.get(name)
	if e.data == nil {
		return false
	}
	h := w.Header()
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Encoding", "gzip")
	h.Set("ETag", e.etag)
	// ServeContent handles If-None-Match / HEAD / Content-Length. We
	// pass a zero modtime (embed.FS has none) and strip Range above.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(e.data))
	return true
}

// ---------------------------------------------------------------------
// API responses: conservative gzip middleware.
// ---------------------------------------------------------------------

// gzipExcludedPrefixes lists API paths that must never be compressed:
// WebSockets, streaming, raw file / blob downloads (Range), MCP
// (streamable HTTP / SSE) and the whole peer-to-peer surface, which
// has its own wire-encoding conventions. /api/v1/blob serves binary
// content with Range / strong-ETag (If-Match) semantics.
var gzipExcludedPrefixes = []string{
	"/api/v1/ws",
	"/api/v1/events",
	"/api/v1/peers",
	"/api/v1/files/raw",
	"/api/v1/files/thumb",
	"/api/v1/blob",
}

func gzipAPIEligible(r *http.Request) bool {
	if r.Method != http.MethodGet {
		// Only GET: keeps idempotency replay caches (POST etc.) free of
		// encoded bodies and avoids touching upload/sync handlers.
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") {
		return false
	}
	if strings.HasSuffix(p, "/mcp") || strings.Contains(p, "/mcp/") {
		return false
	}
	for _, pre := range gzipExcludedPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") || strings.HasPrefix(p, pre+"?") {
			return false
		}
	}
	if r.Header.Get("Upgrade") != "" || r.Header.Get("Range") != "" {
		return false
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		return false
	}
	return acceptsGzip(r)
}

func gzipCompressibleType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch {
	case mt == "text/event-stream":
		return false
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/json", strings.HasSuffix(mt, "+json"),
		mt == "application/javascript", mt == "application/xml",
		strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// gzipAPIMiddleware compresses eligible API responses. The body is
// buffered up to gzipMinSize before deciding; anything that looks like
// streaming (Flush / Hijack before the threshold), already carries a
// Content-Encoding, sets Content-Range, or has a non-compressible type
// is passed through untouched.
func gzipAPIMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gzipAPIEligible(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{rw: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

type gzipState int

const (
	gzUndecided gzipState = iota
	gzPassthrough
	gzCompress
)

type gzipResponseWriter struct {
	rw          http.ResponseWriter
	state       gzipState
	status      int
	wroteHeader bool // WriteHeader called by handler (buffered)
	buf         []byte
	zw          *gzip.Writer
	hijacked    bool
}

var gzipWriterPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return zw
}}

func (g *gzipResponseWriter) Header() http.Header { return g.rw.Header() }

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader || g.state != gzUndecided {
		if g.state == gzPassthrough && !g.wroteHeader {
			g.wroteHeader = true
			g.rw.WriteHeader(code)
		}
		return
	}
	g.wroteHeader = true
	g.status = code
	// Informational responses are forwarded immediately.
	if code >= 100 && code < 200 {
		g.wroteHeader = false
		g.rw.WriteHeader(code)
		return
	}
	if !g.compressible() {
		g.passthrough()
	}
}

func (g *gzipResponseWriter) compressible() bool {
	h := g.rw.Header()
	if g.status != 0 && (g.status < 200 || g.status == http.StatusNoContent ||
		g.status == http.StatusNotModified || g.status == http.StatusPartialContent) {
		return false
	}
	if h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return false
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		// Undetermined yet; decide once bytes are available.
		return true
	}
	return gzipCompressibleType(ct)
}

func (g *gzipResponseWriter) passthrough() {
	g.state = gzPassthrough
	if g.status != 0 {
		g.rw.WriteHeader(g.status)
	}
	if len(g.buf) > 0 {
		_, _ = g.rw.Write(g.buf)
		g.buf = nil
	}
}

func (g *gzipResponseWriter) startCompress() {
	h := g.rw.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", http.DetectContentType(g.buf))
	}
	if !gzipCompressibleType(h.Get("Content-Type")) {
		g.passthrough()
		return
	}
	g.state = gzCompress
	h.Set("Content-Encoding", "gzip")
	addVary(h, "Accept-Encoding")
	h.Del("Content-Length")
	// ETag is intentionally left unchanged: kojo handlers compare
	// If-None-Match / If-Match against their own strong tags (weak tags
	// are rejected), so weakening would break 304s and conditional
	// writes. Vary: Accept-Encoding keeps caches from mixing variants.
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.rw.WriteHeader(g.status)
	zw := gzipWriterPool.Get().(*gzip.Writer)
	zw.Reset(g.rw)
	g.zw = zw
	_, _ = zw.Write(g.buf)
	g.buf = nil
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	switch g.state {
	case gzPassthrough:
		if !g.wroteHeader && g.status == 0 {
			g.wroteHeader = true
		}
		return g.rw.Write(p)
	case gzCompress:
		return g.zw.Write(p)
	}
	if !g.compressible() {
		g.passthrough()
		return g.rw.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMinSize {
		g.startCompress()
	}
	return len(p), nil
}

// Flush: a flush before the threshold means the handler streams, so
// give up on compression and pass through; after compression started,
// flush the gzip stream too.
func (g *gzipResponseWriter) Flush() {
	switch g.state {
	case gzUndecided:
		g.passthrough()
	case gzCompress:
		_ = g.zw.Flush()
	}
	if f, ok := g.rw.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if g.state != gzUndecided {
		return nil, nil, errors.New("gzip: cannot hijack after response started")
	}
	hj, ok := g.rw.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	g.state = gzPassthrough
	g.hijacked = true
	return hj.Hijack()
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.rw }

func (g *gzipResponseWriter) finish() {
	if g.hijacked {
		return
	}
	switch g.state {
	case gzUndecided:
		g.passthrough()
	case gzCompress:
		_ = g.zw.Close()
		g.zw.Reset(io.Discard)
		gzipWriterPool.Put(g.zw)
		g.zw = nil
	}
}
