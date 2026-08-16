package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
)

// CustomClaudeBackend implements ChatBackend for custom Anthropic Messages API
// endpoints (e.g., llama-server). It delegates to ClaudeBackend with
// ANTHROPIC_BASE_URL pointed at the custom endpoint.
type CustomClaudeBackend struct {
	logger *slog.Logger
}

func NewCustomClaudeBackend(logger *slog.Logger) *CustomClaudeBackend {
	return &CustomClaudeBackend{logger: logger}
}

func (b *CustomClaudeBackend) Name() string { return ToolCustomClaude }

// Available returns true if the claude CLI is in PATH (required as the client).
func (b *CustomClaudeBackend) Available() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func (b *CustomClaudeBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	if agent.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for the custom-claude backend")
	}
	cb := NewClaudeBackend(b.logger)
	cb.SetProxyURL(agent.CustomBaseURL)
	// This is a throwaway backend created per turn — it is not the Manager's
	// shared instance, so the Manager can neither reuse nor close a persistent
	// process it might spawn. Force the per-turn spawn model to avoid orphaned
	// long-lived processes that pile up until idle-reap.
	cb.ephemeral = true
	return cb.Chat(ctx, agent, userMessage, systemPrompt, opts)
}
