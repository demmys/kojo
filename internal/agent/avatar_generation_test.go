package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearAvatarKeyFallbacks(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KOJO_GEMINI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_API_KEY",
	} {
		t.Setenv(name, "")
	}
}

func TestResolveAvatarProvider(t *testing.T) {
	cs := setupCredentialStore(t)
	clearAvatarKeyFallbacks(t)
	if err := cs.SetToken("gemini", "", "", "api_key", "gem-key", time.Time{}); err != nil {
		t.Fatal(err)
	}

	provider, key, err := resolveAvatarProvider(cs, "")
	if err != nil || provider != AvatarProviderGemini || key != "gem-key" {
		t.Fatalf("Gemini-only resolve = (%q, %q, %v)", provider, key, err)
	}

	if err := cs.SetToken("openai", "", "", "api_key", "openai-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	provider, key, err = resolveAvatarProvider(cs, "")
	if err == nil || !strings.Contains(err.Error(), "provider is required") || provider != "" || key != "" {
		t.Fatalf("both-key resolve = (%q, %q, %v), want provider-required error", provider, key, err)
	}
	provider, key, err = resolveAvatarProvider(cs, "gemini")
	if err != nil || provider != AvatarProviderGemini || key != "gem-key" {
		t.Fatalf("explicit Gemini resolve = (%q, %q, %v)", provider, key, err)
	}
}

func TestLoadGeminiAPIKeyUsesCredentialStoreBeforeEnvironment(t *testing.T) {
	cs := setupCredentialStore(t)
	clearAvatarKeyFallbacks(t)
	t.Setenv("GEMINI_API_KEY", "env-key")
	if err := cs.SetToken("gemini", "", "", "api_key", "stored-key", time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadGeminiAPIKey(cs)
	if err != nil || got != "stored-key" {
		t.Fatalf("LoadGeminiAPIKey = (%q, %v), want stored-key", got, err)
	}
}

func TestAPIKeyLoadersIgnoreWhitespaceStoredKeys(t *testing.T) {
	cs := setupCredentialStore(t)
	clearAvatarKeyFallbacks(t)
	if err := cs.SetToken("gemini", "", "", "api_key", "   ", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := cs.SetToken("openai", "", "", "api_key", "\n", time.Time{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_API_KEY", "gem-env")
	t.Setenv("OPENAI_API_KEY", "openai-env")
	if got, err := LoadGeminiAPIKey(cs); err != nil || got != "gem-env" {
		t.Fatalf("LoadGeminiAPIKey = (%q, %v)", got, err)
	}
	if got, err := LoadOpenAIAPIKey(cs); err != nil || got != "openai-env" {
		t.Fatalf("LoadOpenAIAPIKey = (%q, %v)", got, err)
	}
}

func TestGenerateAvatarWithOpenAI(t *testing.T) {
	wantImage := []byte("fake-png-body")
	var gotBody map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(wantImage)}},
		})
	}))
	defer stub.Close()

	path, err := generateAvatarWithOpenAI(
		context.Background(), "test-key", "portrait prompt", stub.Client(), stub.URL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	gotImage, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotImage) != string(wantImage) {
		t.Fatalf("image body = %q", gotImage)
	}
	for key, want := range map[string]any{
		"model": openAIImageModel, "prompt": "portrait prompt", "size": "1024x1024",
		"quality": "low", "output_format": "png", "background": "opaque",
	} {
		if gotBody[key] != want {
			t.Errorf("request[%s] = %#v, want %#v", key, gotBody[key], want)
		}
	}
}

func TestGenerateAvatarWithGemini(t *testing.T) {
	wantImage := []byte("fake-webp-body")
	var gotBody map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "gem-key" {
			t.Errorf("x-goog-api-key = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"steps": []any{
				map[string]any{
					"type": "model_output",
					"content": []any{map[string]any{
						"type": "image", "mime_type": "image/webp",
						"data": base64.StdEncoding.EncodeToString(wantImage),
					}},
				},
			},
		})
	}))
	defer stub.Close()

	path, err := generateAvatarWithGemini(
		context.Background(), "gem-key", geminiImageModel, "portrait prompt", stub.Client(), stub.URL, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	if filepath.Ext(path) != ".webp" {
		t.Fatalf("extension = %q", filepath.Ext(path))
	}
	gotImage, err := os.ReadFile(path)
	if err != nil || string(gotImage) != string(wantImage) {
		t.Fatalf("image = %q, err=%v", gotImage, err)
	}
	if gotBody["model"] != geminiImageModel {
		t.Fatalf("model = %#v", gotBody["model"])
	}
	format, _ := gotBody["response_format"].(map[string]any)
	if format["type"] != "image" || format["aspect_ratio"] != "1:1" {
		t.Fatalf("response_format = %#v", format)
	}
}

func TestGenerateAvatarWithGeminiClassifiesProviderErrors(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted"}}`)
	}))
	defer stub.Close()

	_, err := generateAvatarWithGemini(
		context.Background(), "key", geminiImageModel, "prompt", stub.Client(), stub.URL, nil,
	)
	var generationErr *AvatarGenerationError
	if !errors.As(err, &generationErr) || generationErr.Code != "avatar_rate_limited" || generationErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("classified error = %#v (%v)", generationErr, err)
	}
}

func TestGenerateAvatarWithOpenAIReportsAPIAndDecodeErrors(t *testing.T) {
	t.Run("API error", func(t *testing.T) {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","code":"rate_limit"}}`)
		}))
		defer stub.Close()
		_, err := generateAvatarWithOpenAI(context.Background(), "key", "prompt", stub.Client(), stub.URL, nil)
		if err == nil || !strings.Contains(err.Error(), "HTTP 429: rate limited") {
			t.Fatalf("error = %v", err)
		}
		var generationErr *AvatarGenerationError
		if !errors.As(err, &generationErr) || generationErr.Code != "avatar_rate_limited" || generationErr.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("classified error = %#v", generationErr)
		}
	})

	t.Run("invalid base64", func(t *testing.T) {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"b64_json":"%%%"}]}`)
		}))
		defer stub.Close()
		_, err := generateAvatarWithOpenAI(context.Background(), "key", "prompt", stub.Client(), stub.URL, nil)
		if err == nil || !strings.Contains(err.Error(), "decode image") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("moderation", func(t *testing.T) {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"request blocked by content policy","code":"content_policy_violation"}}`)
		}))
		defer stub.Close()
		_, err := generateAvatarWithOpenAI(context.Background(), "key", "prompt", stub.Client(), stub.URL, nil)
		var generationErr *AvatarGenerationError
		if !errors.As(err, &generationErr) || generationErr.Code != "avatar_moderation_blocked" {
			t.Fatalf("classified error = %#v, err=%v", generationErr, err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, err := generateAvatarWithOpenAI(ctx, "key", "prompt", http.DefaultClient, "http://127.0.0.1/unused", nil)
		var generationErr *AvatarGenerationError
		if !errors.As(err, &generationErr) || generationErr.Code != "avatar_timeout" {
			t.Fatalf("classified error = %#v, err=%v", generationErr, err)
		}
	})
}

func TestBuildAvatarPromptIncludesUserDirectionAndCapsPersona(t *testing.T) {
	prompt := buildAvatarPrompt("Toumon", strings.Repeat("界", 4500), "not humanoid")
	if !strings.Contains(prompt, "Additional art direction from the user:\nnot humanoid") {
		t.Fatalf("user direction missing: %q", prompt)
	}
	if strings.Count(prompt, "界") != 4000 {
		t.Fatalf("persona rune count = %d, want 4000", strings.Count(prompt, "界"))
	}
	if !strings.Contains(prompt, "readable at small icon sizes") {
		t.Fatal("avatar constraints missing")
	}
}
