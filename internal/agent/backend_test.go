package agent

import "testing"

// TestBackendLoadsClaudeSkills locks the loader contract for
// .claude/skills. Adding a new backend that does
// NOT read `.claude/skills/` MUST keep returning false; conversely
// a new Claude-Code-compatible backend must be added here so the
// device-switch skill-delivery invariant below remains accurate.
func TestBackendLoadsClaudeSkills(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tool string
		want bool
	}{
		// Claude Code itself: native loader.
		{"claude", true},
		// "custom-claude" delegates to ClaudeBackend with a relocated
		// config dir; skill discovery still walks up from cwd. The legacy
		// spelling must resolve the same way for un-migrated rows.
		{"custom-claude", true},
		{"custom", true},
		// Grok Build: `grok inspect` from an agentDir lists kojo-*
		// skills as `project` scope, confirming the .claude/skills/
		// compatibility path is honored.
		{"grok", true},
		// codex has its own .codex/skills loader, not .claude/skills.
		{"codex", false},
		{"custom-codex", false},
		{"custom-bare", false},
		{"llama.cpp", false},
		// Unknown / empty values must fail closed.
		{"", false},
		{"unknown-future-cli", false},
	}
	for _, tc := range cases {
		if got := backendLoadsClaudeSkills(tc.tool); got != tc.want {
			t.Errorf("backendLoadsClaudeSkills(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestBackendSupportsDeviceSwitch locks the gating contract for the
// kojo-switch-device SKILL.md install sites. A backend qualifies
// only when the handoff orchestrator knows how to migrate its
// session state to the target peer: claude / custom-claude transfer the
// ~/.claude/projects/<...>/<uuid>.jsonl files; grok transfers
// `<agentDir>/.grok/session_id` plus the
// $GROK_HOME/sessions/<encoded(absAgentDir)>/<uuid>/ subtree (see
// grok_session_transfer.go); codex transfers .codex thread refs,
// rollout JSONLs, and Codex state rows. custom-bare has no session
// state at all and must stay false.
func TestBackendSupportsDeviceSwitch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tool string
		want bool
	}{
		{"claude", true},
		{"custom-claude", true},
		{"custom", true},
		{"grok", true},
		{"codex", true},
		{"custom-codex", true},
		{"custom-bare", false},
		{"llama.cpp", false},
		{"", false},
		{"unknown-future-cli", false},
	}
	for _, tc := range cases {
		if got := backendSupportsDeviceSwitch(tc.tool); got != tc.want {
			t.Errorf("backendSupportsDeviceSwitch(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestDeviceSwitchHasSkillLoader enforces the invariant that every
// device-switch-capable backend has a skill delivery path.
func TestDeviceSwitchHasSkillLoader(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"claude", "custom-claude", "grok", "codex", "custom-codex", "custom-bare", ""} {
		hasSkillLoader := backendLoadsClaudeSkills(tool) ||
			NormalizeToolName(tool) == ToolCodex || NormalizeToolName(tool) == ToolCustomCodex
		if backendSupportsDeviceSwitch(tool) && !hasSkillLoader {
			t.Errorf("backendSupportsDeviceSwitch(%q) is true but no skill loader is wired", tool)
		}
	}
}
