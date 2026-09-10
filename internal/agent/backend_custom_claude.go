package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/loppo-llc/kojo/internal/customapi"
)

// CustomClaudeBackend implements ChatBackend for custom Anthropic Messages API
// endpoints (e.g., llama-server). It delegates to ClaudeBackend with
// ANTHROPIC_BASE_URL pointed at the custom endpoint.
type CustomClaudeBackend struct {
	logger *slog.Logger
	creds  *CredentialStore
}

func NewCustomClaudeBackend(logger *slog.Logger, creds *CredentialStore) *CustomClaudeBackend {
	return &CustomClaudeBackend{logger: logger, creds: creds}
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
	apiKey, err := LoadCustomAPIKey(b.creds, agent.ID, agent.CustomBaseURL)
	if err != nil {
		return nil, fmt.Errorf("load custom API key: %w", err)
	}
	// Pin the operator-configured endpoint to loopback/Tailscale and keep the
	// real credential out of the Claude subprocess environment.
	relay, err := customapi.StartLocalRelay(ctx, agent.CustomBaseURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("customBaseURL: %w", err)
	}
	cb := NewClaudeBackend(b.logger)
	cb.SetProxyURL(relay.URL)
	// This is a throwaway backend created per turn — it is not the Manager's
	// shared instance, so the Manager can neither reuse nor close a persistent
	// process it might spawn. Force the per-turn spawn model to avoid orphaned
	// long-lived processes that pile up until idle-reap.
	cb.ephemeral = true
	events, err := cb.Chat(ctx, agent, userMessage, systemPrompt, opts)
	if err != nil {
		relay.Close()
		return nil, err
	}
	out := make(chan ChatEvent, 64)
	go func() {
		defer close(out)
		defer relay.Close()
		for event := range events {
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
