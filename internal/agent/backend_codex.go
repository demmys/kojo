package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// CodexBackend implements ChatBackend for the Codex CLI.
type CodexBackend struct {
	logger *slog.Logger
}

func NewCodexBackend(logger *slog.Logger) *CodexBackend {
	return &CodexBackend{logger: logger}
}

func (b *CodexBackend) Name() string { return "codex" }

func (b *CodexBackend) Available() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

func (b *CodexBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string) (<-chan ChatEvent, error) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex not found in PATH")
	}

	// Build command: codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox -C <dir> "<message>"
	dir := agentDir(agent.ID)
	os.MkdirAll(dir, 0o755)

	args := []string{
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", dir,
	}

	if agent.Model != "" {
		args = append(args, "-m", agent.Model)
	}

	// Prepend system prompt to user message since codex doesn't have --system-prompt.
	// Pass via stdin to avoid exposing the full prompt in process args (visible in ps).
	fullMessage := userMessage
	if systemPrompt != "" {
		fullMessage = systemPrompt + "\n\n---\n\n" + userMessage
	}

	// "-" tells codex to read the prompt from stdin
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(fullMessage)
	cmd.Env = filterEnv([]string{"AGENT_BROWSER_SESSION"}, agent.ID)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}

	ch := make(chan ChatEvent, 64)

	go func() {
		defer close(ch)

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
		var thinking strings.Builder
		var toolUses []ToolUse
		var usage *Usage

		// Buffer agent_message texts. When a tool_call follows, the buffered
		// texts are emitted as "thinking" events (intermediate reasoning).
		// Texts remaining after the last tool call are the final response.
		var pendingTexts []string
		turnCompleted := false

		flushAsThinking := func() {
			for _, t := range pendingTexts {
				thinking.WriteString(t)
				send(ChatEvent{Type: "thinking", Delta: t})
			}
			pendingTexts = nil
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var event codexStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				b.logger.Debug("failed to parse codex stream event", "line", line, "err", err)
				continue
			}

			switch event.Type {
			case "thread.started":
				if !send(ChatEvent{Type: "status", Status: "thinking"}) {
					cmd.Wait()
					return
				}

			case "turn.started":
				// Turn started, keep thinking

			case "item.completed":
				if event.Item.Type == "reasoning" && event.Item.Text != "" {
					// Dedicated reasoning items go directly to thinking
					thinking.WriteString(event.Item.Text)
					send(ChatEvent{Type: "thinking", Delta: event.Item.Text})
				} else if event.Item.Type == "agent_message" && event.Item.Text != "" {
					pendingTexts = append(pendingTexts, event.Item.Text)
				} else if event.Item.Type == "tool_call" {
					// Buffered texts before a tool call are intermediate thinking
					flushAsThinking()
					tu := ToolUse{
						ID:    event.Item.ID,
						Name:  event.Item.Name,
						Input: truncate(event.Item.Arguments, 2000),
					}
					toolUses = append(toolUses, tu)
					if !send(ChatEvent{Type: "tool_use", ToolName: event.Item.Name, ToolInput: truncate(event.Item.Arguments, 2000)}) {
						cmd.Wait()
						return
					}
				} else if event.Item.Type == "tool_call_output" {
					// Texts between tool calls are also thinking
					flushAsThinking()
					if !send(ChatEvent{Type: "tool_result", ToolName: event.Item.Name, ToolOutput: truncate(event.Item.Output, 2000)}) {
						cmd.Wait()
						return
					}
					matchToolOutput(toolUses, event.Item.ID, event.Item.Name, truncate(event.Item.Output, 2000))
				}

			case "turn.completed":
				turnCompleted = true
				// Remaining buffered texts are the final response
				for _, t := range pendingTexts {
					fullText.WriteString(t)
					if !send(ChatEvent{Type: "text", Delta: t}) {
						cmd.Wait()
						return
					}
				}
				pendingTexts = nil

				if event.Usage.OutputTokens > 0 {
					usage = &Usage{
						InputTokens:  event.Usage.InputTokens,
						OutputTokens: event.Usage.OutputTokens,
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			b.logger.Warn("codex stream scanner error", "err", err)
		}

		// Flush remaining buffered texts
		for _, t := range pendingTexts {
			if turnCompleted {
				// turn.completed was received — remaining texts are final response
				fullText.WriteString(t)
				send(ChatEvent{Type: "text", Delta: t})
			} else {
				// Abnormal exit — treat as thinking to avoid leaking reasoning into content
				thinking.WriteString(t)
				send(ChatEvent{Type: "thinking", Delta: t})
			}
		}
		pendingTexts = nil

		if err := cmd.Wait(); err != nil {
			b.logger.Warn("codex process exited with error", "err", err)
			if fullText.Len() == 0 && len(toolUses) == 0 {
				send(ChatEvent{Type: "error", ErrorMessage: fmt.Sprintf("codex exited with error: %v", err)})
				return
			}
		}

		msg := newAssistantMessage()
		msg.Content = fullText.String()
		msg.Thinking = thinking.String()
		msg.ToolUses = toolUses
		msg.Usage = usage

		send(ChatEvent{Type: "done", Message: msg, Usage: usage})
	}()

	return ch, nil
}

// codexStreamEvent represents a Codex CLI JSONL event.
type codexStreamEvent struct {
	Type string `json:"type"`

	// thread.started
	ThreadID string `json:"thread_id,omitempty"`

	// item.completed
	Item struct {
		ID        string `json:"id,omitempty"`
		Type      string `json:"type,omitempty"`      // "agent_message", "tool_call", "tool_call_output", "reasoning"
		Text      string `json:"text,omitempty"`       // for agent_message / reasoning
		Name      string `json:"name,omitempty"`       // for tool_call / tool_call_output
		Arguments string `json:"arguments,omitempty"`  // for tool_call
		Output    string `json:"output,omitempty"`     // for tool_call_output
	} `json:"item,omitempty"`

	// turn.completed
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}
