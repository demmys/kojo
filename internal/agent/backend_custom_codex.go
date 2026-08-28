package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
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

	overrides := customCodexOverrides(base)
	if n := probeCustomContextWindow(ctx, agent.CustomBaseURL, b.logger); n > 0 {
		overrides = append(overrides, customCodexContextOverrides(n)...)
	}

	cb := NewCodexBackend(b.logger)
	cb.SetConfigOverrides(overrides)
	return cb.Chat(ctx, agent, userMessage, systemPrompt, opts)
}

// The share of the endpoint's context that may be filled before codex
// compacts. The remainder has to cover the reply codex is about to generate,
// including reasoning tokens, so a share near 1 would let a turn overflow the
// server mid-response. Kept as a ratio of ints so the arithmetic stays exact
// at any window size.
const (
	customCodexAutoCompactNum = 7
	customCodexAutoCompactDen = 10
)

// customCodexContextOverrides tells codex how much context the endpoint
// actually has. Without them codex falls back to the window of whatever
// built-in model its id resembles — 258400 tokens for an unknown local model —
// so its auto-compaction never fires before the server itself rejects the
// request. A local endpoint typically has far less, and the failure the
// operator sees is codex's generic "experiencing high demand" rather than
// anything naming the real cause.
func customCodexContextOverrides(window int) []string {
	limit := window / customCodexAutoCompactDen * customCodexAutoCompactNum
	if limit < 1 {
		limit = 1
	}
	return []string{
		fmt.Sprintf("model_context_window=%d", window),
		fmt.Sprintf("model_auto_compact_token_limit=%d", limit),
	}
}

const (
	// customContextWindowCacheTTL keeps the probe off the hot path of every
	// turn while still noticing an endpoint restarted with a different size.
	customContextWindowCacheTTL = 5 * time.Minute
	// customContextWindowFailureTTL is short because a failure is as likely to
	// mean "the endpoint was briefly down" as "this endpoint has no /props",
	// and the cost of being wrong is losing compaction for the whole TTL.
	customContextWindowFailureTTL = 30 * time.Second
)

type customContextWindowEntry struct {
	window  int
	fetched time.Time
}

func (e customContextWindowEntry) fresh(now time.Time) bool {
	ttl := customContextWindowCacheTTL
	if e.window == 0 {
		ttl = customContextWindowFailureTTL
	}
	return now.Sub(e.fetched) < ttl
}

var (
	customContextWindowMu    sync.Mutex
	customContextWindowCache = map[string]customContextWindowEntry{}
)

// probeCustomContextWindow asks the endpoint how large its context is, and
// returns 0 when it cannot say. Only llama.cpp's `/props` is consulted: it is
// the endpoint kojo's local-model setups actually use, and no equivalent
// exists in the OpenAI API itself, so anything else simply goes unconfigured
// rather than being configured wrongly.
func probeCustomContextWindow(ctx context.Context, rawBase string, logger *slog.Logger) int {
	root := strings.TrimRight(strings.TrimSpace(rawBase), "/")
	if root == "" {
		return 0
	}
	root = strings.TrimSuffix(root, "/v1")

	customContextWindowMu.Lock()
	entry, ok := customContextWindowCache[root]
	customContextWindowMu.Unlock()
	if ok && entry.fresh(time.Now()) {
		return entry.window
	}

	window := fetchLlamaCppContextWindow(ctx, root)
	if window == 0 {
		if logger != nil {
			logger.Debug("custom-codex: context window probe found nothing, codex will use its default window",
				"base_url", redactURL(root))
		}
		if ctx.Err() != nil {
			// The turn was cancelled, so this says nothing about the
			// endpoint. Caching it would suppress the override for every
			// turn until the entry expired.
			return 0
		}
	}

	now := time.Now()
	customContextWindowMu.Lock()
	// Concurrent probes of the same endpoint race here. A failure must not
	// overwrite a fresh success, since the two disagree only when the failure
	// is the transient one.
	if prev, ok := customContextWindowCache[root]; !ok || window > 0 || prev.window == 0 || !prev.fresh(now) {
		customContextWindowCache[root] = customContextWindowEntry{window: window, fetched: now}
	} else {
		window = prev.window
	}
	customContextWindowMu.Unlock()
	return window
}

// redactURL strips any userinfo before a URL reaches a log line. An operator
// may well have typed credentials into the base URL, and this is the one place
// kojo writes it out. url.URL.Redacted is not enough: it masks the password
// but keeps the username, which for a token-in-userinfo endpoint is the
// secret itself.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	parsed.User = nil
	return parsed.String()
}

// customContextProbeClient never follows a redirect: the base URL is operator
// supplied and not restricted to loopback, so following one would let that
// endpoint steer kojo's own process at an arbitrary host. A redirect is
// treated as "this endpoint does not answer /props".
var customContextProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func fetchLlamaCppContextWindow(ctx context.Context, root string) int {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, root+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := customContextProbeClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}

	var props struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&props); err != nil {
		return 0
	}
	if props.DefaultGenerationSettings.NCtx <= 0 {
		return 0
	}
	return props.DefaultGenerationSettings.NCtx
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
	//
	// features.view_image=false disables codex's view_image tool. It returns
	// its result as a function_call_output whose output is an array holding
	// an {"type":"input_image"} part, and llama-server's /v1/responses parser
	// accepts only input_text parts there — anything else fails the whole
	// request with 400 "Output of tool call should be 'Input text'", killing
	// the turn. Dropping the tool costs image viewing and keeps the backend
	// usable; revisit once the endpoint accepts image parts.
	// The features.* table landed in codex rust-v0.147.0; older CLIs ignore
	// the unknown key silently, which just leaves the 400 in place.
	return []string{
		fmt.Sprintf("model_providers.%s.name=%q", customCodexProviderID, "kojo custom endpoint"),
		fmt.Sprintf("model_providers.%s.base_url=%q", customCodexProviderID, base),
		fmt.Sprintf("model_providers.%s.wire_api=%q", customCodexProviderID, "responses"),
		fmt.Sprintf("model_provider=%q", customCodexProviderID),
		"features.view_image=false",
	}
}
