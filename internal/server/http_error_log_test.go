package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestHTTPServerErrorLogLevels verifies the http.Server ErrorLog
// adapter demotes "TLS handshake error" lines to Debug (suppressed at
// the default Info level) and keeps everything else at Warn.
func TestHTTPServerErrorLogLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	el := httpServerErrorLog(logger)

	el.Printf("http: TLS handshake error from 100.82.3.14:54321: EOF")
	if got := buf.String(); got != "" {
		t.Fatalf("TLS handshake error should be Debug (suppressed at Info), got %q", got)
	}

	el.Printf("http: response.WriteHeader on hijacked connection")
	got := buf.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "hijacked connection") {
		t.Fatalf("non-TLS error should log at Warn, got %q", got)
	}
}
