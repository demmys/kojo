package agent

import "testing"

// TestFilterClaudeStderrNoise pins the behaviour that made a custom-claude
// failure unreadable: the CLI's connectors advisory was the only line on
// stderr, so kojo reported it as the turn's error.
func TestFilterClaudeStderrNoise(t *testing.T) {
	t.Parallel()

	const connectors = "⚠ claude.ai connectors are disabled because ANTHROPIC_API_KEY or another auth source is set and takes precedence over your claude.ai login · Unset it to load your organization's connectors"

	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{"noise only", connectors + "\n", ""},
		{"empty", "   \n\n", ""},
		{"real error kept", "API Error: 400 context exceeded", "API Error: 400 context exceeded"},
		{"noise stripped around real error", connectors + "\nAPI Error: 400 context exceeded\n", "API Error: 400 context exceeded"},
		{"multiple real lines", "line one\nline two", "line one\nline two"},
		// Prefix-anchored: a diagnostic that only mentions the advisory
		// (echoed argv, wrapped log record) is a real error and must survive.
		{"advisory mentioned inside a real error", "spawn failed: claude.ai connectors are disabled because ANTHROPIC_API_KEY", "spawn failed: claude.ai connectors are disabled because ANTHROPIC_API_KEY"},
		// Indentation carries structure in stack traces and JSON dumps.
		{"indentation preserved", "Error: boom\n    at foo()\n        at bar()", "Error: boom\n    at foo()\n        at bar()"},
		{"crlf trimmed", "API Error: 400\r\n", "API Error: 400"},
		{"leading indent on first line preserved", "    Error: boom\n", "    Error: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterClaudeStderrNoise(tc.stderr); got != tc.want {
				t.Errorf("filterClaudeStderrNoise(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}
