package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// CustomOpenCodePreset is one OpenAI-compatible provider the
// custom-opencode tool may target. The list is a hardcoded whitelist:
// an agent picks a preset (stored as its CustomBaseURL, which must equal
// the preset's BaseURL exactly) and can never point OpenCode at an
// arbitrary endpoint.
type CustomOpenCodePreset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"baseURL"`
	// KeyOptional marks local servers that run without authentication.
	KeyOptional bool `json:"keyOptional,omitempty"`
	// PortConfigurable marks local presets whose port may differ from the
	// default: any http://<same host>:<port><same path> is accepted.
	PortConfigurable bool `json:"portConfigurable,omitempty"`
}

var customOpenCodePresets = []CustomOpenCodePreset{
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1"},
	{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com/openai/v1"},
	{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1"},
	{ID: "together", Name: "Together AI", BaseURL: "https://api.together.xyz/v1"},
	{ID: "fireworks", Name: "Fireworks AI", BaseURL: "https://api.fireworks.ai/inference/v1"},
	{ID: "mistral", Name: "Mistral", BaseURL: "https://api.mistral.ai/v1"},
	{ID: "cerebras", Name: "Cerebras", BaseURL: "https://api.cerebras.ai/v1"},
	{ID: "xai", Name: "xAI", BaseURL: "https://api.x.ai/v1"},
	{ID: "zai", Name: "Z.ai", BaseURL: "https://api.z.ai/api/paas/v4"},
	{ID: "ollama", Name: "Ollama (local)", BaseURL: "http://127.0.0.1:11434/v1", KeyOptional: true, PortConfigurable: true},
	{ID: "lmstudio", Name: "LM Studio (local)", BaseURL: "http://127.0.0.1:1234/v1", KeyOptional: true, PortConfigurable: true},
	{ID: "llamacpp", Name: "llama-server (local)", BaseURL: "http://127.0.0.1:8080/v1", KeyOptional: true, PortConfigurable: true},
}

// CustomOpenCodePresets returns a copy of the provider whitelist.
func CustomOpenCodePresets() []CustomOpenCodePreset {
	return append([]CustomOpenCodePreset(nil), customOpenCodePresets...)
}

// CustomOpenCodePresetByBaseURL finds the preset whose BaseURL equals
// baseURL (surrounding whitespace ignored, nothing else normalised).
func CustomOpenCodePresetByBaseURL(baseURL string) (CustomOpenCodePreset, bool) {
	baseURL = strings.TrimSpace(baseURL)
	for _, p := range customOpenCodePresets {
		if p.BaseURL == baseURL {
			return p, true
		}
	}
	// A port-configurable local preset also matches the same scheme, host
	// and path on another port. The first such preset wins (they share
	// host and path); only the provider id label depends on which.
	u, err := url.Parse(baseURL)
	if err != nil || u.User != nil || u.String() != baseURL || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return CustomOpenCodePreset{}, false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return CustomOpenCodePreset{}, false
	}
	for _, p := range customOpenCodePresets {
		if !p.PortConfigurable {
			continue
		}
		pu, err := url.Parse(p.BaseURL)
		if err != nil {
			continue
		}
		if u.Scheme == pu.Scheme && u.Hostname() == pu.Hostname() && u.Path == pu.Path && u.RawPath == "" {
			p.BaseURL = baseURL
			return p, true
		}
	}
	return CustomOpenCodePreset{}, false
}

// customOpenCodeModelMaxLen bounds the free-text model id.
const customOpenCodeModelMaxLen = 200

// ValidateCustomOpenCodeConfig checks a custom-opencode agent's
// (CustomBaseURL, Model) pair: the base URL must be a whitelisted preset
// and the model, when set, a plain id (it becomes a provider-config key
// and the `-m kojo-<preset>/<model>` argument). An empty model is allowed
// at save time (the turn then fails with a clear message), so a tool
// switch can be saved before the model is typed.
func ValidateCustomOpenCodeConfig(baseURL, model string) error {
	if _, ok := CustomOpenCodePresetByBaseURL(baseURL); !ok {
		return fmt.Errorf("customBaseURL %q is not an allowed custom-opencode provider", strings.TrimSpace(baseURL))
	}
	return validateCustomOpenCodeModel(model)
}

func validateCustomOpenCodeModel(model string) error {
	if len(model) > customOpenCodeModelMaxLen {
		return fmt.Errorf("model id exceeds %d bytes", customOpenCodeModelMaxLen)
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '{' || r == '}' {
			return fmt.Errorf("model id %q contains an unsupported character", model)
		}
	}
	return nil
}

// customOpenCodeProviderID is the OpenCode provider id kojo defines for a
// preset. The "kojo-" prefix keeps it clear of OpenCode's built-in
// provider ids (whose bundled definitions carry their own env keys).
func customOpenCodeProviderID(p CustomOpenCodePreset) string { return "kojo-" + p.ID }

// customOpenCodeAPIKeyEnv carries the agent's key to OpenCode. The KOJO_OC_
// prefix makes kojo's env plugin strip it from every shell command, and
// openCodeMCPEnv blanks it for MCP servers.
const customOpenCodeAPIKeyEnv = "KOJO_OC_CUSTOM_API_KEY"

// CustomOpenCodeDefaultMaxOutputTokens is the output cap when the agent has
// no MaxOutputTokens set. Kept well below the gemini default because many
// OpenAI-compatible providers reject a max_tokens above 8192.
const CustomOpenCodeDefaultMaxOutputTokens = 8192

// customOpenCodeContextLimit is the context window described to OpenCode
// for every custom model (used for its auto-compaction threshold).
const customOpenCodeContextLimit = 131072

// effectiveOpenCodeMaxOutputTokens maps the stored setting to the value
// sent for the given tool.
func effectiveOpenCodeMaxOutputTokens(tool string, v int) int {
	if NormalizeToolName(tool) == ToolCustomOpenCode && v <= 0 {
		return CustomOpenCodeDefaultMaxOutputTokens
	}
	return EffectiveGeminiMaxOutputTokens(v)
}

// customOpenCodeTurn describes the custom-opencode provider of one turn.
type customOpenCodeTurn struct {
	preset CustomOpenCodePreset
	model  string
	// hasKey: the agent has an API key, passed as {env:customOpenCodeAPIKeyEnv}.
	hasKey bool
	// keyPlaceholder is the random string the provider's apiKey holds in
	// the config map; renderOpenCodeTurnConfig swaps it for the {env:}
	// reference after marshalOpenCodeConfig escaped every other one.
	keyPlaceholder string
}

func (c *customOpenCodeTurn) providerID() string { return customOpenCodeProviderID(c.preset) }

func (c *customOpenCodeTurn) ref() string { return c.providerID() + "/" + c.model }

// prepareCustomOpenCode resolves the agent's preset, model and key.
func (b *GeminiBackend) prepareCustomOpenCode(a *Agent) (*customOpenCodeTurn, string, error) {
	preset, ok := CustomOpenCodePresetByBaseURL(a.CustomBaseURL)
	if !ok {
		return nil, "", fmt.Errorf("custom-opencode: provider %q is not in the allowed list; pick a provider in the agent settings", a.CustomBaseURL)
	}
	model := strings.TrimSpace(a.Model)
	if model == "" {
		return nil, "", errors.New("custom-opencode: no model configured; set a model id in the agent settings")
	}
	if err := validateCustomOpenCodeModel(model); err != nil {
		return nil, "", fmt.Errorf("custom-opencode: %w", err)
	}
	key, err := LoadCustomAPIKey(b.creds, a.ID, a.CustomBaseURL)
	if err != nil {
		return nil, "", fmt.Errorf("custom-opencode: load API key: %w", err)
	}
	key = strings.TrimSpace(key)
	if key == "" && !preset.KeyOptional {
		return nil, "", fmt.Errorf("custom-opencode: no API key configured for %s; set it in the agent settings", preset.Name)
	}
	if strings.ContainsAny(key, "\x00\n\r") {
		return nil, "", errors.New("custom-opencode: API key contains control characters")
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, "", err
	}
	return &customOpenCodeTurn{
		preset:         preset,
		model:          model,
		hasKey:         key != "",
		keyPlaceholder: "kojo-key-placeholder-" + hex.EncodeToString(rnd[:]),
	}, key, nil
}

// buildCustomOpenCodeTurnConfig is buildOpenCodeTurnConfig for
// custom-opencode: a single "kojo-<preset>" provider on the bundled
// @ai-sdk/openai-compatible SDK, the only enabled provider (this
// overrides the home's global ["google"]; OpenCode replaces, not
// concatenates, the list). No variants: kojo sends no reasoning effort.
// t.maxOutputTokens is the effective value (see
// effectiveOpenCodeMaxOutputTokens).
func buildCustomOpenCodeTurnConfig(t openCodeTurnConfig) map[string]any {
	c := t.custom
	ref := c.ref()
	build := map[string]any{"model": ref}
	if t.systemPrompt != "" {
		build["prompt"] = t.systemPrompt
	}
	options := map[string]any{"baseURL": c.preset.BaseURL}
	if c.hasKey {
		options["apiKey"] = c.keyPlaceholder
	}
	cfg := map[string]any{
		"model":             ref,
		"small_model":       ref,
		"enabled_providers": []string{c.providerID()},
		"agent":             map[string]any{"build": build},
		"lsp":               false,
		"formatter":         false,
		"provider": map[string]any{
			c.providerID(): map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"name":    c.preset.Name,
				"options": options,
				"models": map[string]any{
					c.model: map[string]any{
						"name":      c.model,
						"tool_call": true,
						"limit": map[string]any{
							"context": customOpenCodeContextLimit,
							"output":  t.maxOutputTokens,
						},
					},
				},
			},
		},
	}
	addOpenCodeTurnCommon(cfg, t)
	return cfg
}

// renderOpenCodeTurnConfig marshals a turn config (marshalOpenCodeConfig)
// and, for custom-opencode, turns the apiKey placeholder into the one
// {env:} reference OpenCode may substitute. The placeholder is random per
// turn, so no prompt or MCP text can contain it.
func renderOpenCodeTurnConfig(t openCodeTurnConfig) ([]byte, error) {
	body, err := marshalOpenCodeConfig(buildOpenCodeTurnConfig(t))
	if err != nil || t.custom == nil || !t.custom.hasKey {
		return body, err
	}
	quoted := []byte(`"` + t.custom.keyPlaceholder + `"`)
	if bytes.Count(body, quoted) != 1 {
		return nil, errors.New("custom-opencode: api key placeholder not found exactly once")
	}
	return bytes.Replace(body, quoted, []byte(`"{env:`+customOpenCodeAPIKeyEnv+`}"`), 1), nil
}
