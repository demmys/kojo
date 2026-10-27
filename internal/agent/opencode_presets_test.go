package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestCustomOpenCodePresetLookup(t *testing.T) {
	p, ok := CustomOpenCodePresetByBaseURL("  https://openrouter.ai/api/v1 ")
	if !ok || p.ID != "openrouter" {
		t.Fatalf("openrouter lookup = %+v %v", p, ok)
	}
	for _, bad := range []string{"", "https://openrouter.ai/api/v1/", "https://evil.example/v1", "http://localhost:8080"} {
		if _, ok := CustomOpenCodePresetByBaseURL(bad); ok {
			t.Errorf("%q must not match a preset", bad)
		}
	}
	seen := map[string]bool{}
	for _, p := range CustomOpenCodePresets() {
		if seen[p.ID] || p.ID == "" || p.BaseURL == "" || p.Name == "" {
			t.Errorf("bad preset %+v", p)
		}
		seen[p.ID] = true
	}
	if !seen["ollama"] {
		t.Error("ollama preset missing")
	}
}

func TestValidateCustomOpenCodeConfig(t *testing.T) {
	ok := []struct{ url, model string }{
		{"https://api.groq.com/openai/v1", "llama-3.3-70b-versatile"},
		{"https://openrouter.ai/api/v1", "meta-llama/llama-3.1-8b-instruct:free"},
		{"http://127.0.0.1:11434/v1", ""},
	}
	for _, c := range ok {
		if err := ValidateCustomOpenCodeConfig(c.url, c.model); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	bad := []struct{ url, model string }{
		{"https://evil.example/v1", "m"},
		{"", "m"},
		{"https://api.groq.com/openai/v1", "bad model"},
		{"https://api.groq.com/openai/v1", "m\n--x"},
		{"https://api.groq.com/openai/v1", "{env:HOME}"},
		{"https://api.groq.com/openai/v1", strings.Repeat("a", 201)},
	}
	for _, c := range bad {
		if err := ValidateCustomOpenCodeConfig(c.url, c.model); err == nil {
			t.Errorf("%v: expected error", c)
		}
	}
}

func testCustomTurn(hasKey bool) *customOpenCodeTurn {
	p, _ := CustomOpenCodePresetByBaseURL("https://api.deepseek.com/v1")
	return &customOpenCodeTurn{preset: p, model: "deepseek-chat", hasKey: hasKey, keyPlaceholder: "kojo-key-placeholder-abc"}
}

func TestRenderCustomOpenCodeTurnConfig(t *testing.T) {
	body, err := renderOpenCodeTurnConfig(openCodeTurnConfig{
		model:           "deepseek-chat",
		custom:          testCustomTurn(true),
		systemPrompt:    "be nice {env:KOJO_OC_CUSTOM_API_KEY} kojo-key-placeholder-ab",
		maxOutputTokens: 8192,
		skillsDir:       "/skills",
		mcpServers:      map[string]mcpServerEntry{"s": {Command: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "{env:"); n != 1 {
		t.Fatalf("want exactly one {env:} reference, got %d:\n%s", n, body)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "kojo-deepseek/deepseek-chat" || cfg["small_model"] != "kojo-deepseek/deepseek-chat" {
		t.Errorf("model refs: %v %v", cfg["model"], cfg["small_model"])
	}
	if ep, _ := json.Marshal(cfg["enabled_providers"]); string(ep) != `["kojo-deepseek"]` {
		t.Errorf("enabled_providers = %s", ep)
	}
	prov := cfg["provider"].(map[string]any)
	if _, has := prov["google"]; has || len(prov) != 1 {
		t.Errorf("providers = %v", prov)
	}
	kd := prov["kojo-deepseek"].(map[string]any)
	if kd["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %v", kd["npm"])
	}
	opts := kd["options"].(map[string]any)
	if opts["baseURL"] != "https://api.deepseek.com/v1" || opts["apiKey"] != "{env:"+customOpenCodeAPIKeyEnv+"}" {
		t.Errorf("options = %v", opts)
	}
	m := kd["models"].(map[string]any)["deepseek-chat"].(map[string]any)
	if lim := m["limit"].(map[string]any); lim["output"] != float64(8192) {
		t.Errorf("limit = %v", lim)
	}
	if _, has := m["variants"]; has {
		t.Error("custom models must define no variants")
	}
	build := cfg["agent"].(map[string]any)["build"].(map[string]any)
	if !strings.Contains(build["prompt"].(string), "kojo-key-placeholder-ab") || strings.Contains(build["prompt"].(string), "{env:") == false {
		// decoded prompt keeps its literal text (the \u007b escape decodes to "{").
		t.Errorf("prompt = %q", build["prompt"])
	}
	if !strings.Contains(string(body), `\u007benv:KOJO_OC_CUSTOM_API_KEY}`) {
		t.Error("prompt's {env:} must stay escaped")
	}
	if cfg["mcp"] == nil || cfg["skills"] == nil {
		t.Error("mcp / skills missing")
	}
}

func TestRenderCustomOpenCodeTurnConfig_NoKey(t *testing.T) {
	body, err := renderOpenCodeTurnConfig(openCodeTurnConfig{model: "x", custom: testCustomTurn(false), maxOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "apiKey") || strings.Contains(string(body), "{env:") {
		t.Errorf("no-key config must not reference a key:\n%s", body)
	}
}

func TestCustomOpenCodeEnvAndArgs(t *testing.T) {
	dir := t.TempDir()
	m := envMap(openCodeCommandEnv("ag_x", dir, customOpenCodeAPIKeyEnv, "sk-1", "/cfg.json",
		effectiveOpenCodeMaxOutputTokens(ToolCustomOpenCode, 0)))
	if m[customOpenCodeAPIKeyEnv] != "sk-1" || m["GOOGLE_GENERATIVE_AI_API_KEY"] != "" {
		t.Errorf("key env: %v", m)
	}
	if m["OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX"] != "8192" {
		t.Errorf("output max = %q", m["OPENCODE_EXPERIMENTAL_OUTPUT_TOKEN_MAX"])
	}
	if got := effectiveOpenCodeMaxOutputTokens(ToolCustomOpenCode, 4000); got != 4000 {
		t.Errorf("explicit max = %d", got)
	}
	if got := effectiveOpenCodeMaxOutputTokens(ToolGeminiAPI, 0); got != GeminiDefaultMaxOutputTokens {
		t.Errorf("gemini default = %d", got)
	}
	args := buildOpenCodeRunArgs("kojo-groq/llama", "", "")
	if !containsArg(args, "kojo-groq/llama") || containsArg(args, "--variant") {
		t.Errorf("args = %v", args)
	}
	if mcp := openCodeMCPEnv(nil); mcp[customOpenCodeAPIKeyEnv] != "" {
		t.Error("MCP env must blank the custom key")
	} else if _, ok := mcp[customOpenCodeAPIKeyEnv]; !ok {
		t.Error("MCP env must override the custom key")
	}
	if !strings.Contains(openCodeEnvPlugin, `"`+customOpenCodeAPIKeyEnv+`"`) {
		t.Error("plugin DROP list must name the custom key")
	}
}

func TestCustomOpenCodeToolPlumbing(t *testing.T) {
	b := NewCustomOpenCodeBackend(nil, nil)
	if b.Name() != ToolCustomOpenCode || !backendSupportsSessionKey(b) || !backendNeedsRecentMessagesFallback(b) {
		t.Error("custom-opencode must behave like gemini for sessions")
	}
	if !isOpenCodeTool(ToolCustomOpenCode) || extensionSkillRoot(ToolCustomOpenCode) != ".opencode" ||
		!backendSupportsDeviceSwitch(ToolCustomOpenCode) || !ToolRequiresCustomBaseURL(ToolCustomOpenCode) {
		t.Error("custom-opencode tool helpers")
	}
	if NewGeminiBackend(nil, nil).Name() != ToolGeminiAPI {
		t.Error("gemini name")
	}
}

func TestPrepareCustomOpenCode(t *testing.T) {
	b := NewCustomOpenCodeBackend(nil, nil)
	if _, _, err := b.prepareCustomOpenCode(&Agent{ID: "ag_x", CustomBaseURL: "https://evil.example/v1", Model: "m"}); err == nil {
		t.Error("non-preset URL must fail")
	}
	if _, _, err := b.prepareCustomOpenCode(&Agent{ID: "ag_x", CustomBaseURL: "https://api.groq.com/openai/v1"}); err == nil {
		t.Error("empty model must fail")
	}
	if _, _, err := b.prepareCustomOpenCode(&Agent{ID: "ag_x", CustomBaseURL: "https://api.groq.com/openai/v1", Model: "m"}); err == nil {
		t.Error("missing key for a keyed preset must fail")
	}
	c, key, err := b.prepareCustomOpenCode(&Agent{ID: "ag_x", CustomBaseURL: "http://127.0.0.1:11434/v1", Model: "qwen3:8b"})
	if err != nil || key != "" || c.hasKey || c.ref() != "kojo-ollama/qwen3:8b" {
		t.Errorf("ollama: %+v %q %v", c, key, err)
	}
}

func TestManagerCustomOpenCodeValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	mgr, err := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	if _, err := mgr.Create(AgentConfig{Name: "bad", Tool: ToolCustomOpenCode, CustomBaseURL: "https://evil.example/v1", Model: "m"}); err == nil {
		t.Fatal("create with a non-preset URL must fail")
	}
	a, err := mgr.Create(AgentConfig{
		Name: "oc", Tool: ToolCustomOpenCode, CustomBaseURL: "https://openrouter.ai/api/v1",
		Model: "openai/gpt-4o-mini", CustomAPIKey: "sk-or-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if key, _ := LoadCustomAPIKey(mgr.Credentials(), a.ID, a.CustomBaseURL); key != "sk-or-1" {
		t.Fatalf("key = %q", key)
	}
	evil := "https://evil.example/v1"
	if _, err := mgr.Update(a.ID, AgentUpdateConfig{CustomBaseURL: &evil}); err == nil {
		t.Fatal("PATCH to a non-preset URL must fail")
	}
	badModel := "a b"
	if _, err := mgr.Update(a.ID, AgentUpdateConfig{Model: &badModel}); err == nil {
		t.Fatal("PATCH to a bad model must fail")
	}
	// A custom-codex agent on an arbitrary URL cannot switch tools without
	// also picking a preset.
	cc, err := mgr.Create(AgentConfig{Name: "cc", Tool: ToolCustomCodex, CustomBaseURL: "http://localhost:8888"})
	if err != nil {
		t.Fatal(err)
	}
	tool := ToolCustomOpenCode
	if _, err := mgr.Update(cc.ID, AgentUpdateConfig{Tool: &tool}); err == nil {
		t.Fatal("tool switch keeping a non-preset URL must fail")
	}
	groq := "https://api.groq.com/openai/v1"
	if _, err := mgr.Update(cc.ID, AgentUpdateConfig{Tool: &tool, CustomBaseURL: &groq}); err != nil {
		t.Fatalf("tool switch with a preset: %v", err)
	}
}

func TestCustomOpenCodePresetLocalPort(t *testing.T) {
	ok := []string{"http://127.0.0.1:8081/v1", "http://127.0.0.1:65535/v1", " http://127.0.0.1:9000/v1 "}
	for _, u := range ok {
		p, found := CustomOpenCodePresetByBaseURL(u)
		if !found || !p.PortConfigurable || p.BaseURL != strings.TrimSpace(u) {
			t.Errorf("%q: got %+v %v", u, p, found)
		}
	}
	bad := []string{
		"http://127.0.0.1/v1", "http://127.0.0.1:0/v1", "http://127.0.0.1:70000/v1", "http://127.0.0.1:080/v1",
		"http://localhost:9000/v1", "https://127.0.0.1:9000/v1", "http://127.0.0.1:9000/v2",
		"http://127.0.0.1:9000/v1?x=1", "http://127.0.0.1:9000/v1?", "http://127.0.0.1:9000/v1#", "http://u@127.0.0.1:9000/v1", "http://127.0.0.1:9000/v1/",
		"http://evil.example:9000/v1", "https://api.groq.com:8443/openai/v1",
	}
	for _, u := range bad {
		if _, found := CustomOpenCodePresetByBaseURL(u); found {
			t.Errorf("%q accepted", u)
		}
	}
}
