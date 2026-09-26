package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withGeminiStub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := geminiAPIBase
	geminiAPIBase = srv.URL
	t.Cleanup(func() {
		geminiAPIBase = orig
		srv.Close()
	})
}

func TestGeminiInteractionsSynthesize(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	var gotBody map[string]any
	var gotKey string
	withGeminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/interactions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotKey = r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"steps": []any{map[string]any{
				"type": "model_output",
				"content": []any{map[string]any{
					"type":        "audio",
					"mime_type":   "audio/l16; rate=24000; channels=1",
					"sample_rate": 24000,
					"data":        base64.StdEncoding.EncodeToString(pcm),
				}},
			}},
		})
	})

	svc := NewService(func() (string, error) { return "gem-key", nil }, nil)
	res, err := svc.Synthesize(context.Background(), SynthesizeRequest{
		Model:       "gemini-3.8-flash-lite-tts",
		Voice:       "Kore",
		StylePrompt: "淡々と",
		Text:        "テスト",
		Format:      "wav",
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if gotKey != "gem-key" {
		t.Errorf("api key header = %q", gotKey)
	}
	if gotBody["model"] != "gemini-3.8-flash-lite-tts" {
		t.Errorf("model = %v", gotBody["model"])
	}
	// Text must be the verbatim transcript; style travels in metadata.
	input := gotBody["input"].([]any)[0].(map[string]any)
	part := input["content"].([]any)[0].(map[string]any)
	if part["text"] != "テスト" {
		t.Errorf("text = %q, want verbatim transcript", part["text"])
	}
	ann := part["annotations"].([]any)[0].(map[string]any)
	if ann["type"] != "speech_metadata" || ann["style"] != "淡々と" {
		t.Errorf("annotation = %v", ann)
	}
	rf := gotBody["response_format"].(map[string]any)
	if rf["mime_type"] != "audio/l16" {
		t.Errorf("response_format = %v", rf)
	}
	sc := gotBody["generation_config"].(map[string]any)["speech_config"].([]any)[0].(map[string]any)
	if sc["voice"] != "Kore" {
		t.Errorf("speech_config = %v", sc)
	}
	for _, k := range []string{"system_instruction", "safety_settings", "safetySettings"} {
		if _, ok := gotBody[k]; ok {
			t.Errorf("unexpected field %q sent to interactions API", k)
		}
	}
	if res.Format != "wav" || string(res.AudioBytes[44:]) != string(pcm) {
		t.Errorf("result = %s %v", res.Format, res.AudioBytes)
	}
}

func TestGeminiInteractionsRejectsWAVPayload(t *testing.T) {
	withGeminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"steps": []any{map[string]any{"content": []any{map[string]any{
				"type": "audio", "mime_type": "audio/wav", "data": base64.StdEncoding.EncodeToString([]byte("RIFF")),
			}}}},
		})
	})
	svc := NewService(func() (string, error) { return "k", nil }, nil)
	_, err := svc.callGemini(context.Background(), "gemini-3.8-flash-tts", "Kore", "", "x")
	if err == nil || !strings.Contains(err.Error(), "unexpected audio mime") {
		t.Fatalf("err = %v", err)
	}
}

func TestGeminiInteractionsIncompleteIsError(t *testing.T) {
	withGeminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "incomplete",
			"steps": []any{map[string]any{"content": []any{map[string]any{
				"type": "audio", "mime_type": "audio/l16; rate=24000; channels=1", "data": base64.StdEncoding.EncodeToString([]byte{0, 0}),
			}}}},
		})
	})
	svc := NewService(func() (string, error) { return "k", nil }, nil)
	_, err := svc.callGemini(context.Background(), "gemini-3.8-flash-tts", "Kore", "", "x")
	if err == nil || !strings.Contains(err.Error(), "status=incomplete") {
		t.Fatalf("partial audio from an incomplete interaction must be an error, got %v", err)
	}
}

func TestGeminiInteractionsError(t *testing.T) {
	withGeminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad","code":"invalid_request"}}`))
	})
	svc := NewService(func() (string, error) { return "k", nil }, nil)
	_, err := svc.callGemini(context.Background(), "gemini-3.8-flash-tts", "Kore", "", "x")
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != "invalid_request" || apiErr.Message != "bad" || apiErr.HTTPStatus != 400 {
		t.Fatalf("err = %#v", err)
	}
	// Not the generateContent safety-threshold rejection.
	if rejectsSafetyThreshold(err) {
		t.Errorf("interactions error must not trigger the BLOCK_NONE retry")
	}
}

func TestGeminiLegacyModelUsesGenerateContent(t *testing.T) {
	var gotPath, gotText string
	withGeminiStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var b struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotText = b.Contents[0].Parts[0].Text
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
				map[string]any{"inlineData": map[string]any{"mimeType": "audio/l16", "data": base64.StdEncoding.EncodeToString([]byte{0, 0})}},
			}}}},
		})
	})
	svc := NewService(func() (string, error) { return "k", nil }, nil)
	if _, err := svc.callGemini(context.Background(), "gemini-3.1-flash-tts-preview", "Kore", "style", "本文"); err != nil {
		t.Fatalf("callGemini: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-3.1-flash-tts-preview:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotText, SystemInstruction) || !strings.HasSuffix(gotText, "style\n\n本文") {
		t.Errorf("legacy prompt shape changed: %q", gotText)
	}
}

func TestDefaultModelIsInteractions(t *testing.T) {
	if !usesInteractionsAPI(DefaultModel) {
		t.Errorf("DefaultModel %q should use the Interactions API", DefaultModel)
	}
	if usesInteractionsAPI("gemini-3.1-flash-tts-preview") || usesInteractionsAPI("gemini-2.5-pro-preview-tts") {
		t.Errorf("legacy models must stay on generateContent")
	}
}
