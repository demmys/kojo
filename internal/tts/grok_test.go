package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// TestMain points the on-disk cache at a throwaway temp dir so synthesize
// tests don't pollute the real config directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kojo-tts-test-*")
	if err != nil {
		panic(err)
	}
	configdir.Set(dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestIsValidGrokVoice(t *testing.T) {
	if !IsValidGrokVoice("eve") {
		t.Errorf("eve should be valid")
	}
	if !IsValidGrokVoice("Eve") {
		t.Errorf("voice ids are case-insensitive")
	}
	if !IsValidGrokVoice("my-custom_voice1") {
		t.Errorf("custom voice id should be accepted")
	}
	if IsValidGrokVoice("") {
		t.Errorf("empty should be invalid")
	}
	if IsValidGrokVoice("bad voice!") {
		t.Errorf("illegal charset should be rejected")
	}
}

func TestGrokSynthesize(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tts" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("FAKEMP3BYTES"))
	}))
	defer srv.Close()

	orig := grokTTSBase
	grokTTSBase = srv.URL
	defer func() { grokTTSBase = orig }()

	svc := NewService(nil, func() (string, error) { return "test-key", nil })

	res, err := svc.Synthesize(context.Background(), SynthesizeRequest{
		Provider:    ProviderGrok,
		Voice:       "Eve",
		StylePrompt: "should be ignored",
		Text:        "テスト",
		Format:      "opus", // opus must map to mp3 for grok
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	// Request shape.
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotBody["voice_id"] != "Eve" {
		t.Errorf("voice_id = %v, want Eve", gotBody["voice_id"])
	}
	if gotBody["text"] != "テスト" {
		t.Errorf("text = %v", gotBody["text"])
	}
	// Style must NOT leak into the spoken text or any field.
	if s, _ := gotBody["text"].(string); s != "テスト" {
		t.Errorf("style leaked into text: %q", s)
	}
	if _, ok := gotBody["style"]; ok {
		t.Errorf("unexpected style field sent to grok")
	}
	of, _ := gotBody["output_format"].(map[string]any)
	if of["codec"] != "mp3" {
		t.Errorf("codec = %v, want mp3", of["codec"])
	}

	// Result + cache write.
	if res.Format != "mp3" {
		t.Errorf("result format = %q, want mp3", res.Format)
	}
	if string(res.AudioBytes) != "FAKEMP3BYTES" {
		t.Errorf("audio bytes = %q", res.AudioBytes)
	}
	if res.Cached {
		t.Errorf("first call should not be cached")
	}
	data, ok := cacheGet(res.Hash, "mp3")
	if !ok || string(data) != "FAKEMP3BYTES" {
		t.Errorf("cache write missing/incorrect")
	}

	// Second call hits cache (case-insensitive voice → same key).
	res2, err := svc.Synthesize(context.Background(), SynthesizeRequest{
		Provider: ProviderGrok,
		Voice:    "eve",
		Text:     "テスト",
		Format:   "mp3",
	})
	if err != nil {
		t.Fatalf("second synthesize: %v", err)
	}
	if !res2.Cached {
		t.Errorf("second call should be cached")
	}
	if res2.Hash != res.Hash {
		t.Errorf("case-insensitive voice should share cache key: %s vs %s", res.Hash, res2.Hash)
	}
}

func TestGrokProviderCacheKeyDistinct(t *testing.T) {
	g := hashRequest(ProviderGemini, "", "eve", "", "テスト", "mp3", true)
	k := hashRequest(ProviderGrok, "", "eve", "", "テスト", "mp3", true)
	if g == k {
		t.Errorf("gemini and grok cache keys must differ")
	}
}
