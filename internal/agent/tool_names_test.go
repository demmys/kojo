package agent

import "testing"

func TestNormalizeToolName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"custom":             ToolCustomClaude,
		"llama.cpp":          ToolCustomBare,
		ToolCustomClaude:     ToolCustomClaude,
		ToolCustomCodex:      ToolCustomCodex,
		ToolCustomBare:       ToolCustomBare,
		ToolClaude:           ToolClaude,
		ToolCodex:            ToolCodex,
		ToolGrok:             ToolGrok,
		"":                   "",
		"unknown-future-cli": "unknown-future-cli",
	}
	for in, want := range cases {
		if got := NormalizeToolName(in); got != want {
			t.Errorf("NormalizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolRequiresCustomBaseURL(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		ToolCustomClaude: true,
		ToolCustomCodex:  true,
		ToolCustomBare:   true,
		// Legacy spellings must gate identically: a PATCH from an old
		// client that sets tool=llama.cpp without a base URL has to fail
		// validation, not slip through and blow up at chat time.
		"custom":    true,
		"llama.cpp": true,
		ToolClaude:  false,
		ToolCodex:   false,
		ToolGrok:    false,
		"":          false,
	}
	for tool, want := range cases {
		if got := ToolRequiresCustomBaseURL(tool); got != want {
			t.Errorf("ToolRequiresCustomBaseURL(%q) = %v, want %v", tool, got, want)
		}
	}
}

func TestToolHasAgenticTools(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{ToolClaude, ToolCodex, ToolGrok, ToolCustomClaude, ToolCustomCodex, "custom", ""} {
		if !toolHasAgenticTools(tool) {
			t.Errorf("toolHasAgenticTools(%q) = false, want true", tool)
		}
	}
	for _, tool := range []string{ToolCustomBare, "llama.cpp"} {
		if toolHasAgenticTools(tool) {
			t.Errorf("toolHasAgenticTools(%q) = true, want false", tool)
		}
	}
}

func TestCustomCodexBaseURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// kojo stores the server root; codex's chat wire API wants the
		// OpenAI API root it appends /chat/completions to.
		"http://127.0.0.1:8080":     "http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080/":    "http://127.0.0.1:8080/v1",
		"  http://127.0.0.1:8080  ": "http://127.0.0.1:8080/v1",
		// Already an API root: don't double up.
		"http://127.0.0.1:8080/v1":  "http://127.0.0.1:8080/v1",
		"http://127.0.0.1:8080/v1/": "http://127.0.0.1:8080/v1",
		"":                          "",
		"   ":                       "",
	}
	for in, want := range cases {
		if got := customCodexBaseURL(in); got != want {
			t.Errorf("customCodexBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBackendSupportsSteer covers the delegating backends: custom-claude
// and custom-codex run the same CLI processes as claude and codex, so they
// register the same steer handle and must not be rejected up front.
func TestBackendSupportsSteer(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		ToolClaude:       true,
		ToolCodex:        true,
		ToolCustomClaude: true,
		ToolCustomCodex:  true,
		"custom":         true,
		ToolGrok:         false,
		ToolCustomBare:   false,
		"llama.cpp":      false,
		"":               false,
	}
	for tool, want := range cases {
		if got := backendSupportsSteer(tool); got != want {
			t.Errorf("backendSupportsSteer(%q) = %v, want %v", tool, got, want)
		}
	}
}

// TestCustomCodexOverrides pins the flags kojo hands the codex CLI. The
// wire_api value is the load-bearing one: codex removed `wire_api = "chat"`
// in rust-v0.95.0 and hard-errors on it, so anything but "responses" makes
// every custom-codex turn fail at process start.
func TestCustomCodexOverrides(t *testing.T) {
	t.Parallel()

	got := customCodexOverrides("http://127.0.0.1:8080/v1")
	want := []string{
		`model_providers.kojo_custom.name="kojo custom endpoint"`,
		`model_providers.kojo_custom.base_url="http://127.0.0.1:8080/v1"`,
		`model_providers.kojo_custom.wire_api="responses"`,
		`model_provider="kojo_custom"`,
		// view_image returns an input_image part inside function_call_output,
		// which llama-server's /v1/responses parser rejects with a 400 that
		// kills the whole turn. See customCodexOverrides.
		`features.view_image=false`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d overrides, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("override %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestNormalizeAgent_ToolRename verifies the read-path migration of the
// pre-rename backend identifiers. Rows written by an older kojo still carry
// "custom" / "llama.cpp"; the manager's backend registry only knows the new
// names, so hydration has to rewrite them or those agents lose their backend.
func TestNormalizeAgent_ToolRename(t *testing.T) {
	st := newAgentStore(t)

	cases := map[string]string{
		"custom":    ToolCustomClaude,
		"llama.cpp": ToolCustomBare,
		// Already-current and unrelated values pass through untouched.
		ToolCustomClaude: ToolCustomClaude,
		ToolCustomCodex:  ToolCustomCodex,
		ToolCustomBare:   ToolCustomBare,
		ToolClaude:       ToolClaude,
		"":               "",
	}
	for stored, want := range cases {
		a := &Agent{ID: "a", Tool: stored}
		st.normalizeAgent(a)
		if a.Tool != want {
			t.Errorf("stored tool %q normalized to %q, want %q", stored, a.Tool, want)
		}
	}
}
