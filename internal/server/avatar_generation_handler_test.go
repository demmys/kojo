package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
)

func clearServerAvatarKeyFallbacks(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KOJO_GEMINI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_API_KEY",
	} {
		t.Setenv(name, "")
	}
}

func newTempAvatar(t *testing.T, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "avatar.png")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return path
}

func TestHandleGenerateAvatarFallbackPolicy(t *testing.T) {
	srv := newSTTTestServer(t)
	clearServerAvatarKeyFallbacks(t)

	t.Run("first generation returns visible fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/generate-avatar", bytes.NewBufferString(`{"name":"Toumon"}`))
		rr := httptest.NewRecorder()
		srv.handleGenerateAvatar(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var got struct {
			AvatarPath string `json:"avatarPath"`
			Fallback   bool   `json:"fallback"`
			Warning    string `json:"warning"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Fallback || got.AvatarPath == "" || got.Warning == "" {
			t.Fatalf("response = %+v", got)
		}
		cleanupTempAvatar(got.AvatarPath)
	})

	t.Run("settings regeneration refuses fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/generate-avatar", bytes.NewBufferString(
			`{"name":"Toumon","allowFallback":false}`,
		))
		rr := httptest.NewRecorder()
		srv.handleGenerateAvatar(rr, req)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var got struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if got.Error.Code != "avatar_generation_failed" {
			t.Fatalf("error code = %q", got.Error.Code)
		}
	})
}

func TestHandleGenerateAvatarPreviousPreviewLifecycle(t *testing.T) {
	srv := newSTTTestServer(t)
	original := generateAvatarWithAI
	t.Cleanup(func() { generateAvatarWithAI = original })

	t.Run("success replaces previous preview", func(t *testing.T) {
		previous := newTempAvatar(t, "old")
		next := newTempAvatar(t, "new")
		generateAvatarWithAI = func(context.Context, *agent.CredentialStore, string, string, string, string, *slog.Logger) (string, agent.AvatarProvider, error) {
			return next, agent.AvatarProviderOpenAI, nil
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/generate-avatar", bytes.NewBufferString(
			`{"name":"Toumon","provider":"openai","previousPath":`+strconvQuote(previous)+`}`,
		))
		rr := httptest.NewRecorder()
		srv.handleGenerateAvatar(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("previous preview still exists: %v", err)
		}
		if _, err := os.Stat(next); err != nil {
			t.Fatalf("new preview missing: %v", err)
		}
	})

	t.Run("failure retains previous preview", func(t *testing.T) {
		previous := newTempAvatar(t, "old")
		generateAvatarWithAI = func(context.Context, *agent.CredentialStore, string, string, string, string, *slog.Logger) (string, agent.AvatarProvider, error) {
			return "", agent.AvatarProviderOpenAI, errors.New("upstream failed")
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/generate-avatar", bytes.NewBufferString(
			`{"name":"Toumon","provider":"openai","previousPath":`+strconvQuote(previous)+`}`,
		))
		rr := httptest.NewRecorder()
		srv.handleGenerateAvatar(rr, req)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		if _, err := os.Stat(previous); err != nil {
			t.Fatalf("previous preview removed: %v", err)
		}
	})
}

func TestHandleGenerateAvatarRejectsOversizedBody(t *testing.T) {
	srv := newSTTTestServer(t)
	body := `{"name":"Toumon","prompt":"` + strings.Repeat("x", 70<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/generate-avatar", strings.NewReader(body))
	rr := httptest.NewRecorder()
	srv.handleGenerateAvatar(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleDiscardPreviewAvatarRemovesTempDirectory(t *testing.T) {
	path, err := agent.GenerateSVGAvatarFile("Toumon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTempAvatar(path) })

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/preview-avatar?path="+url.QueryEscape(path), nil)
	rr := httptest.NewRecorder()
	(&Server{}).handleDiscardPreviewAvatar(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview still exists: %v", err)
	}
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
