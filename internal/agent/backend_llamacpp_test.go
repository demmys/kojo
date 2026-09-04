package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomBareBackendUsesStoredBearerKey(t *testing.T) {
	const wantKey = "sk-unsloth-chat"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	creds := setupCredentialStore(t)
	if err := StoreCustomAPIKey(creds, "ag_unsloth", upstream.URL, wantKey); err != nil {
		t.Fatal(err)
	}
	backend := NewCustomBareBackend(slog.New(slog.NewTextHandler(io.Discard, nil)), creds)
	ch, err := backend.Chat(context.Background(), &Agent{
		ID: "ag_unsloth", Model: "test-model", CustomBaseURL: upstream.URL,
	}, "hello", "", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range ch {
	}
}

func TestThinkStripper_GemmaChannelMarker(t *testing.T) {
	s := &thinkStripper{}
	got := s.Process("<|channel>thought<channel|>Hello world")
	if got != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", got)
	}
}

func TestThinkStripper_GemmaChannelMarkerSplit(t *testing.T) {
	s := &thinkStripper{}
	out := s.Process("<|channel>th")
	if out != "" {
		t.Errorf("expected empty during buffering, got %q", out)
	}
	out = s.Process("ought<channel|>Hello")
	if out != "Hello" {
		t.Errorf("expected 'Hello', got %q", out)
	}
}

func TestThinkStripper_ThinkBlock(t *testing.T) {
	s := &thinkStripper{}
	got := s.Process("<think>reasoning here</think>actual response")
	if got != "actual response" {
		t.Errorf("expected 'actual response', got %q", got)
	}
}

func TestThinkStripper_ThinkBlockSplitAcrossChunks(t *testing.T) {
	s := &thinkStripper{}
	out := s.Process("<think>reas")
	if out != "" {
		t.Errorf("expected empty (in block), got %q", out)
	}
	out = s.Process("oning</think>response")
	if out != "response" {
		t.Errorf("expected 'response', got %q", out)
	}
}

func TestThinkStripper_CloseTagSplitAcrossChunks(t *testing.T) {
	s := &thinkStripper{}
	s.Process("<think>thinking content")
	out := s.Process("more</thi")
	if out != "" {
		t.Errorf("expected empty (partial close tag buffered), got %q", out)
	}
	out = s.Process("nk>response text")
	if out != "response text" {
		t.Errorf("expected 'response text', got %q", out)
	}
}

func TestThinkStripper_NoMarker(t *testing.T) {
	s := &thinkStripper{}
	got := s.Process("Hello world")
	if got != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", got)
	}
}

func TestThinkStripper_InlineChannelMarker(t *testing.T) {
	s := &thinkStripper{}
	s.prefixDone = true
	got := s.Process("before<|channel>response<channel|>after")
	if got != "beforeafter" {
		t.Errorf("expected 'beforeafter', got %q", got)
	}
}

func TestThinkStripper_InlineChannelMarkerSplit(t *testing.T) {
	s := &thinkStripper{}
	s.prefixDone = true
	out := s.Process("text<|channel>resp")
	if out != "text" {
		t.Errorf("expected 'text' (tail buffered), got %q", out)
	}
	out = s.Process("onse<channel|>more")
	if out != "more" {
		t.Errorf("expected 'more', got %q", out)
	}
}

func TestThinkStripper_PartialTagAtEnd(t *testing.T) {
	s := &thinkStripper{}
	s.prefixDone = true
	out := s.Process("hello<")
	if out != "hello" {
		t.Errorf("expected 'hello' (< buffered), got %q", out)
	}
	out = s.Process("not a tag")
	if out != "<not a tag" {
		t.Errorf("expected '<not a tag', got %q", out)
	}
}

func TestThinkStripper_FlushTailBuf(t *testing.T) {
	s := &thinkStripper{}
	s.prefixDone = true
	s.Process("hello<")
	got := s.Flush()
	if got != "<" {
		t.Errorf("expected '<', got %q", got)
	}
}

func TestThinkStripper_FlushPrefixBuf(t *testing.T) {
	s := &thinkStripper{}
	s.Process("<")
	got := s.Flush()
	if got != "<" {
		t.Errorf("expected '<', got %q", got)
	}
}

func TestLongestPartialTag(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"hello", 0},
		{"hello<", 1},
		{"hello<t", 2},
		{"hello<th", 3},
		{"hello<thi", 4},
		{"hello<thin", 5},
		{"hello<think", 6},
		{"hello<think>", 0},
		{"hello</", 2},
		{"hello</t", 3},
		{"hello<|", 2},
		{"hello<|c", 3},
	}
	for _, tt := range tests {
		got := longestPartialTag(tt.text)
		if got != tt.want {
			t.Errorf("longestPartialTag(%q) = %d, want %d", tt.text, got, tt.want)
		}
	}
}
