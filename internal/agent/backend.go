package agent

import "context"

// ChatBackend abstracts a CLI tool for agent chat.
type ChatBackend interface {
	// Chat sends a message and returns a channel of streaming events.
	// The channel is closed when the response is complete.
	Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string) (<-chan ChatEvent, error)

	// Name returns the tool identifier (e.g. "claude", "codex", "gemini").
	Name() string

	// Available returns true if the CLI tool is installed and accessible.
	Available() bool
}
