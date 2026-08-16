package agent

// Backend tool identifiers stored in Agent.Tool and accepted by the API.
//
// The three "custom-*" backends all talk to a user-supplied endpoint
// (Agent.CustomBaseURL) but differ in what drives the conversation:
//
//   - ToolCustomClaude: the claude CLI with ANTHROPIC_BASE_URL pointed at an
//     Anthropic Messages API compatible server. Full tools / sessions / skills.
//   - ToolCustomCodex:  the codex CLI with a `model_providers.*` override
//     pointed at an OpenAI-compatible server. Full tools / sessions.
//   - ToolCustomBare:   kojo speaks OpenAI-compatible HTTP to the endpoint
//     directly, with no CLI in between. No tools, no session, no skills —
//     every turn is a stateless [system, user] request.
const (
	ToolClaude       = "claude"
	ToolCodex        = "codex"
	ToolGrok         = "grok"
	ToolCustomClaude = "custom-claude"
	ToolCustomCodex  = "custom-codex"
	ToolCustomBare   = "custom-bare"
)

// legacyToolNames maps the pre-rename identifiers onto their current
// equivalents. "custom" and "llama.cpp" were renamed because neither name
// said which CLI (if any) actually drives the turn — the whole point of the
// custom-{claude,codex,bare} split.
//
// Old values are still accepted on the API and migrated on the store's read
// path (normalizeAgent), so agents created before the rename keep working
// without a data migration step.
var legacyToolNames = map[string]string{
	"custom":    ToolCustomClaude,
	"llama.cpp": ToolCustomBare,
}

// NormalizeToolName rewrites a legacy tool identifier to its current name.
// Unknown / already-current values pass through untouched — validation of
// the value itself happens elsewhere (backend lookup).
func NormalizeToolName(tool string) string {
	if cur, ok := legacyToolNames[tool]; ok {
		return cur
	}
	return tool
}

// ToolRequiresCustomBaseURL reports whether the backend cannot run without
// Agent.CustomBaseURL. All three custom-* backends target a user-supplied
// endpoint, so an empty base URL is a configuration error rather than a
// runtime fallback.
func ToolRequiresCustomBaseURL(tool string) bool {
	switch NormalizeToolName(tool) {
	case ToolCustomClaude, ToolCustomCodex, ToolCustomBare:
		return true
	default:
		return false
	}
}

// toolHasAgenticTools reports whether the backend gives the agent a
// tool-calling harness (file read/write, shell, MCP) during a turn.
//
// Only ToolCustomBare is false: kojo posts a single stateless chat
// completion to the endpoint, so the model can neither read its memory
// files nor curl kojo's API. buildSystemPrompt uses this to drop every
// instruction that presupposes a tool call — see buildSystemPrompt's
// slim path.
func toolHasAgenticTools(tool string) bool {
	return NormalizeToolName(tool) != ToolCustomBare
}
