package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LlamaCppBackend implements ChatBackend by talking directly to llama-server's
// OpenAI-compatible /v1/chat/completions endpoint via HTTP SSE streaming.
// No CLI dependency — just needs HTTP access to the server.
type LlamaCppBackend struct {
	logger *slog.Logger
	client *http.Client
}

func NewLlamaCppBackend(logger *slog.Logger) *LlamaCppBackend {
	return &LlamaCppBackend{
		logger: logger,
		client: &http.Client{Timeout: 0},
	}
}

func (b *LlamaCppBackend) Name() string { return "llama.cpp" }

func (b *LlamaCppBackend) Available() bool { return true }

func (b *LlamaCppBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	if agent.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for llama.cpp backend")
	}
	if err := validateLoopbackURL(agent.CustomBaseURL); err != nil {
		return nil, fmt.Errorf("llama.cpp customBaseURL: %w", err)
	}

	messages := []llamaCppMessage{}
	if systemPrompt != "" {
		messages = append(messages, llamaCppMessage{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, llamaCppMessage{Role: "user", Content: userMessage})

	reqBody := llamaCppRequest{
		Model:    agent.Model,
		Messages: messages,
		Stream:   true,
		StreamOptions: &llamaCppStreamOptions{
			IncludeUsage: true,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := strings.TrimRight(agent.CustomBaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer no-key")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("llama-server returned %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()
		b.streamSSE(ctx, ch, resp.Body)
	}()

	return ch, nil
}

// validateLoopbackURL checks that the URL points to a loopback address.
func validateLoopbackURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("only loopback addresses are allowed, got %q", host)
	}
	return nil
}

func (b *LlamaCppBackend) streamSSE(ctx context.Context, ch chan<- ChatEvent, body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

	var content strings.Builder
	var thinking strings.Builder
	var usage *Usage
	started := false

	send := func(e ChatEvent) bool {
		select {
		case ch <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// SSE spec: events are delimited by blank lines.
	// Each event may have multiple "data:" lines that are concatenated with "\n".
	var dataBuf strings.Builder

	// dispatch processes an accumulated SSE data buffer.
	// Returns: done (true if stream finished), cancelled (true if context was cancelled).
	dispatch := func(data string) (done, cancelled bool) {
		if data == "[DONE]" {
			return true, false
		}

		var chunk llamaCppChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			b.logger.Warn("failed to parse SSE chunk", "err", err, "data", data)
			return false, false
		}

		if chunk.Usage != nil {
			usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}

		if len(chunk.Choices) == 0 {
			return false, false
		}
		delta := chunk.Choices[0].Delta

		if !started {
			if !send(ChatEvent{Type: "status", Status: "thinking"}) {
				return false, true
			}
			started = true
		}

		if delta.ReasoningContent != "" {
			thinking.WriteString(delta.ReasoningContent)
			if !send(ChatEvent{Type: "thinking", Delta: delta.ReasoningContent}) {
				return false, true
			}
		}

		if delta.Content != "" {
			content.WriteString(delta.Content)
			if !send(ChatEvent{Type: "text", Delta: delta.Content}) {
				return false, true
			}
		}

		return false, false
	}

	streamDone := false
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if dataBuf.Len() == 0 {
				continue
			}
			data := dataBuf.String()
			dataBuf.Reset()

			done, cancelled := dispatch(data)
			if cancelled {
				emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
				return
			}
			if done {
				streamDone = true
				break
			}
			continue
		}

		// Accumulate data fields (handle both "data: " and "data:" per SSE spec)
		if strings.HasPrefix(line, "data:") {
			val := line[5:]
			if len(val) > 0 && val[0] == ' ' {
				val = val[1:]
			}
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
		}
	}

	// Flush any remaining buffered data (stream closed without trailing blank line)
	if !streamDone && dataBuf.Len() > 0 {
		data := dataBuf.String()
		_, cancelled := dispatch(data)
		if cancelled {
			emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
			return
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		b.logger.Warn("SSE scan error", "err", err)
		send(ChatEvent{Type: "error", ErrorMessage: err.Error()})
		return
	}

	if ctx.Err() != nil {
		emitCancelDone(ctx, ch, content.String(), thinking.String(), nil, usage)
		return
	}

	msg := newAssistantMessage()
	msg.Content = content.String()
	msg.Thinking = thinking.String()
	msg.Usage = usage
	msg.Timestamp = time.Now().Format(time.RFC3339)

	ch <- ChatEvent{
		Type:    "done",
		Message: msg,
		Usage:   usage,
	}
}

// --- Request/Response types ---

type llamaCppMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llamaCppStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type llamaCppRequest struct {
	Model         string                 `json:"model"`
	Messages      []llamaCppMessage      `json:"messages"`
	Stream        bool                   `json:"stream"`
	StreamOptions *llamaCppStreamOptions `json:"stream_options,omitempty"`
}

type llamaCppChunk struct {
	Choices []llamaCppChoice `json:"choices"`
	Usage   *llamaCppUsage   `json:"usage,omitempty"`
}

type llamaCppChoice struct {
	Delta        llamaCppDelta `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

type llamaCppDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type llamaCppUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
