package agent

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// ClaudeBackend implements ChatBackend using the Claude CLI with stream-json output.
type ClaudeBackend struct {
	logger *slog.Logger
}

func NewClaudeBackend(logger *slog.Logger) *ClaudeBackend {
	return &ClaudeBackend{logger: logger}
}

func (b *ClaudeBackend) Name() string { return "claude" }

func (b *ClaudeBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *ClaudeBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string) (<-chan ChatEvent, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not found in PATH")
	}

	args := []string{
		"-p", userMessage,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}

	// Use session-id for conversation continuity.
	// Claude CLI requires UUID format, so derive one from agent ID.
	sessionID := agentIDToUUID(agent.ID)
	args = append(args, "--session-id", sessionID)

	cmd := exec.CommandContext(ctx, claudePath, args...)

	// Clear CLAUDE_CODE environment variables to avoid nested detection
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CODE") || strings.HasPrefix(e, "CLAUDECODE") {
			continue
		}
		filtered = append(filtered, e)
	}
	cmd.Env = filtered

	// Set working directory to agent's data directory
	dir := agentDir(agent.ID)
	os.MkdirAll(dir, 0o755)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)

		// send is a helper that respects context cancellation to avoid goroutine leaks.
		send := func(e ChatEvent) bool {
			select {
			case ch <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var fullText strings.Builder
		var toolUses []ToolUse
		var usage *Usage

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event claudeStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				b.logger.Debug("failed to parse claude stream event", "line", line, "err", err)
				continue
			}

			switch event.Type {
			case "system":
				if !send(ChatEvent{Type: "status", Status: "thinking"}) {
					cmd.Wait()
					return
				}

			case "assistant":
				if event.Message.StopReason != "" {
					if event.Message.Usage.OutputTokens > 0 {
						usage = &Usage{
							InputTokens:  event.Message.Usage.InputTokens,
							OutputTokens: event.Message.Usage.OutputTokens,
						}
					}
				}

			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					if !send(ChatEvent{Type: "tool_use", ToolName: event.ContentBlock.Name}) {
						cmd.Wait()
						return
					}
				}

			case "content_block_delta":
				if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					fullText.WriteString(event.Delta.Text)
					if !send(ChatEvent{Type: "text", Delta: event.Delta.Text}) {
						cmd.Wait()
						return
					}
				}

			case "content_block_stop":
				// If it was a tool use block, we'll get the result next

			case "result":
				if event.Result != "" {
					if fullText.Len() == 0 {
						fullText.WriteString(event.Result)
						if !send(ChatEvent{Type: "text", Delta: event.Result}) {
							cmd.Wait()
							return
						}
					}
				}

			case "tool_use":
				tu := ToolUse{
					Name:  event.Name,
					Input: truncate(event.Input, 2000),
				}
				toolUses = append(toolUses, tu)
				if !send(ChatEvent{Type: "tool_use", ToolName: event.Name, ToolInput: truncate(event.Input, 2000)}) {
					cmd.Wait()
					return
				}

			case "tool_result":
				if !send(ChatEvent{Type: "tool_result", ToolName: event.Name, ToolOutput: truncate(event.Content, 2000)}) {
					cmd.Wait()
					return
				}
				for i := len(toolUses) - 1; i >= 0; i-- {
					if toolUses[i].Name == event.Name && toolUses[i].Output == "" {
						toolUses[i].Output = truncate(event.Content, 2000)
						break
					}
				}
			}
		}

		// Check for scanner errors
		if err := scanner.Err(); err != nil {
			b.logger.Warn("claude stream scanner error", "err", err)
		}

		// Check process exit status
		if err := cmd.Wait(); err != nil {
			b.logger.Warn("claude process exited with error", "err", err)
			if fullText.Len() == 0 && len(toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: fmt.Sprintf("claude exited with error: %v", err)})
				return
			}
		}

		msg := newAssistantMessage()
		msg.Content = fullText.String()
		msg.ToolUses = toolUses
		msg.Usage = usage

		send(ChatEvent{Type: "done", Message: msg, Usage: usage})
	}()

	return ch, nil
}

// Claude stream-json event types
type claudeStreamEvent struct {
	Type string `json:"type"`

	// "system" event
	Subtype string `json:"subtype,omitempty"`

	// "assistant" event
	Message struct {
		StopReason string `json:"stop_reason,omitempty"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`

	// "content_block_start" event
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	} `json:"content_block,omitempty"`

	// "content_block_delta" event
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta,omitempty"`

	// "result" event
	Result    string `json:"result,omitempty"`
	Duration  int    `json:"duration_ms,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// "tool_use" / "tool_result" events
	Name    string `json:"name,omitempty"`
	Input   string `json:"input,omitempty"`
	Content string `json:"content,omitempty"`
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// agentIDToUUID converts an agent ID (e.g. "ag_8cf247118ad856e8") to a
// deterministic UUID v3 string that claude CLI accepts as --session-id.
func agentIDToUUID(agentID string) string {
	h := md5.Sum([]byte(agentID))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
