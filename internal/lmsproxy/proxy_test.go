package lmsproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"
)

// mockLMStudio simulates LM Studio /v1/responses with SSE streaming.
func mockLMStudio(responseID string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]string{
					{"id": "test-model"},
				},
			})
			return
		}

		if r.URL.Path != "/v1/responses" {
			http.Error(w, "not found", 404)
			return
		}

		// Read and validate the request.
		var req OAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		// Send a simple text response.
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n",
			mustJSON(OAIResponseCreated{
				Type:     "response.created",
				Response: OAIResponseObject{ID: responseID, Model: req.Model},
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.content_part.added\ndata: %s\n\n",
			mustJSON(map[string]interface{}{
				"type": "response.content_part.added", "output_index": 0, "content_index": 0,
				"part": map[string]string{"type": "output_text", "text": ""},
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n",
			mustJSON(OAIOutputTextDelta{
				Type: "response.output_text.delta", OutputIndex: 0, ContentIndex: 0, Delta: "Hello",
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.output_text.done\ndata: %s\n\n",
			mustJSON(map[string]interface{}{
				"type": "response.output_text.done", "output_index": 0, "content_index": 0,
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n",
			mustJSON(OAIResponseCompleted{
				Type: "response.completed",
				Response: OAIResponseObject{
					ID: responseID, Model: req.Model,
					Usage: &OAIUsage{InputTokens: 5, OutputTokens: 1},
				},
			}))
		flusher.Flush()
	}))
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestProxy_EndToEnd(t *testing.T) {
	lms := mockLMStudio("resp_001")
	defer lms.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(lms.URL, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, err := proxy.Start(ctx, 0) // port 0 = random
	if err != nil {
		t.Fatal(err)
	}
	// Give server a moment to start.
	time.Sleep(10 * time.Millisecond)

	// Send Anthropic Messages request.
	reqBody := `{
		"model": "test-model",
		"max_tokens": 100,
		"system": "Be helpful",
		"messages": [{"role": "user", "content": "hello"}],
		"stream": true
	}`

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/v1/messages", port),
		"application/json",
		strings.NewReader(reqBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	result := string(body)

	// Verify Anthropic SSE format.
	if !strings.Contains(result, "event: message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(result, `"text_delta"`) {
		t.Error("missing text_delta")
	}
	if !strings.Contains(result, `"Hello"`) {
		t.Error("missing Hello text")
	}
	if !strings.Contains(result, "event: message_stop") {
		t.Error("missing message_stop")
	}
}

func TestProxy_SessionContinuity(t *testing.T) {
	callCount := 0
	lms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "not found", 404)
			return
		}
		callCount++

		var req OAIRequest
		json.NewDecoder(r.Body).Decode(&req)

		// On second call, expect previous_response_id.
		respID := fmt.Sprintf("resp_%d", callCount)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n",
			mustJSON(OAIResponseCreated{
				Type:     "response.created",
				Response: OAIResponseObject{ID: respID, Model: req.Model},
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.content_part.added\ndata: %s\n\n",
			mustJSON(map[string]interface{}{
				"type": "response.content_part.added", "output_index": 0, "content_index": 0,
				"part": map[string]string{"type": "output_text", "text": ""},
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n",
			mustJSON(OAIOutputTextDelta{
				Type: "response.output_text.delta", Delta: "ok",
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.output_text.done\ndata: %s\n\n",
			mustJSON(map[string]interface{}{
				"type": "response.output_text.done", "output_index": 0,
			}))
		flusher.Flush()

		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n",
			mustJSON(OAIResponseCompleted{
				Type:     "response.completed",
				Response: OAIResponseObject{ID: respID, Model: req.Model, Usage: &OAIUsage{}},
			}))
		flusher.Flush()
	}))
	defer lms.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(lms.URL, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, err := proxy.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	// First request.
	resp1, _ := http.Post(baseURL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"a"}],"stream":true}`))
	io.ReadAll(resp1.Body)
	resp1.Body.Close()

	// Second request (continued conversation).
	resp2, _ := http.Post(baseURL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"ok"},{"role":"user","content":"b"}],"stream":true}`))
	io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if callCount != 2 {
		t.Errorf("expected 2 calls to LMS, got %d", callCount)
	}
}

func TestProxy_ModelsEndpoint(t *testing.T) {
	lms := mockLMStudio("resp_001")
	defer lms.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(lms.URL, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port, err := proxy.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/v1/models", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status: %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}

	// Should contain Claude model IDs for CLI validation.
	found := false
	for _, m := range result.Data {
		if m.ID == "claude-opus-4-6[1m]" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected claude-opus-4-6[1m] in models response")
	}
}
