package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// customCodexProviderID is the `model_providers.<id>` key kojo synthesizes
// for the operator's endpoint. A fixed id is fine because the overrides are
// passed per-process on the command line, never written to config.toml —
// two agents pointing at different endpoints never share a codex process.
//
// Underscored rather than hyphenated so the `-c` dotted path stays an
// unambiguous TOML bare key.
const customCodexProviderID = "kojo_custom"

// CustomCodexBackend implements ChatBackend for OpenAI-compatible endpoints
// driven by the codex CLI. It delegates to CodexBackend with a synthesized
// `model_providers.*` entry pointed at Agent.CustomBaseURL, which gives the
// endpoint the full codex harness (tools, sandboxed shell, MCP, resumable
// threads) — the difference from custom-bare, where kojo posts a single
// stateless completion itself.
//
// Unlike custom-bare, the base URL is NOT restricted to loopback: the
// request is issued by the codex CLI subprocess, exactly like custom-claude
// issues its own through the claude CLI, so kojo's process is not the one
// being pointed at an arbitrary host.
type CustomCodexBackend struct {
	logger *slog.Logger
}

func NewCustomCodexBackend(logger *slog.Logger) *CustomCodexBackend {
	return &CustomCodexBackend{logger: logger}
}

func (b *CustomCodexBackend) Name() string { return ToolCustomCodex }

// Available returns true if the codex CLI is in PATH (required as the client).
func (b *CustomCodexBackend) Available() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

// customCodexBaseURL normalizes the operator-supplied base URL into what
// codex expects: the OpenAI API root, i.e. the prefix it concatenates
// "/responses" onto. kojo stores the server root (custom-bare appends
// "/v1/chat/completions" itself), so the "/v1" segment is added here when it
// isn't already present — verified against codex 0.145.0, which posts to
// "<base_url>/responses" verbatim.
func customCodexBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed
	}
	return trimmed + "/v1"
}

func (b *CustomCodexBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	if agent.CustomBaseURL == "" {
		return nil, fmt.Errorf("customBaseURL is required for the custom-codex backend")
	}
	base := customCodexBaseURL(agent.CustomBaseURL)
	if base == "" {
		return nil, fmt.Errorf("customBaseURL is required for the custom-codex backend")
	}

	cb := NewCodexBackend(b.logger)
	cb.SetConfigOverrides(customCodexOverrides(base))
	return cb.Chat(ctx, agent, userMessage, systemPrompt, opts)
}

// customCodexOverrides builds the `-c key=value` list that points codex at
// the operator's endpoint.
func customCodexOverrides(base string) []string {
	// %q renders a TOML basic string, which is what codex parses the value
	// half of `-c key=value` as. No env_key is declared: local endpoints
	// (llama-server, LM Studio) take no API key, and codex omits the
	// Authorization header entirely when the provider declares none —
	// declaring one would make the env var mandatory and hard-fail at start.
	//
	// wire_api must be "responses": codex dropped `wire_api = "chat"` in
	// rust-v0.95.0 and now hard-errors on it, so the endpoint has to speak
	// /v1/responses (llama-server does since its partial implementation
	// landed; older builds need a bridge in front).
	return []string{
		fmt.Sprintf("model_providers.%s.name=%q", customCodexProviderID, "kojo custom endpoint"),
		fmt.Sprintf("model_providers.%s.base_url=%q", customCodexProviderID, base),
		fmt.Sprintf("model_providers.%s.wire_api=%q", customCodexProviderID, "responses"),
		fmt.Sprintf("model_provider=%q", customCodexProviderID),
	}
}
