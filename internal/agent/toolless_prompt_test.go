package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedToollessAgentFiles writes the on-disk state buildSystemPrompt reads
// (MEMORY.md, user.md, status.json) so both the tool-backed and the
// tool-less prompt have every optional section available to emit.
func seedToollessAgentFiles(t *testing.T, agentID string) {
	t.Helper()
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"MEMORY.md":   "# Memory\n\n- the user prefers short answers\n",
		"user.md":     "- the user is called Hana\n",
		"status.json": `{"mood":"flat"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBuildSystemPrompt_ToollessOmitsToolInstructions locks the custom-bare
// contract: kojo posts one stateless chat completion per turn, so the model
// has no Read/Edit/Grep/Bash and no way to reach kojo's HTTP API. Every
// instruction that presupposes one of those is a directive it can only
// hallucinate compliance with, so none of them may appear.
func TestBuildSystemPrompt_ToollessOmitsToolInstructions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_toolless", Tool: ToolCustomBare, Persona: "You are terse."}
	seedToollessAgentFiles(t, a.ID)

	prompt := buildSystemPrompt(a, newQuietLogger(), "http://127.0.0.1:8080", nil, true)

	banned := []string{
		"## Sending file attachments to the user", // needs a file write
		"## Calling the user",                     // needs curl
		"## kojo Guides",                          // every guide is a Read
		"## Memory Recall",                        // Read / Grep procedure
		"Memory Write — MANDATORY",                // needs the Edit tool
		"using the Edit tool",
		"Use Grep to search",
		"curl",
		"Your file storage directory is",
		"your current working directory",
	}
	for _, s := range banned {
		if strings.Contains(prompt, s) {
			t.Errorf("tool-less prompt must not contain %q", s)
		}
	}
}

// TestBuildSystemPrompt_ToollessKeepsInjectedContent is the other half of
// the contract: dropping the tool instructions must not drop the content
// kojo injects on the model's behalf, which is the only context a
// stateless backend ever gets.
func TestBuildSystemPrompt_ToollessKeepsInjectedContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_toolless_content", Tool: ToolCustomBare, Persona: "You are terse."}
	seedToollessAgentFiles(t, a.ID)

	prompt := buildSystemPrompt(a, newQuietLogger(), "http://127.0.0.1:8080", nil, true)

	required := []string{
		"the user prefers short answers", // MEMORY.md body
		"the user is called Hana",        // user.md body
		`{"mood":"flat"}`,                // status.json body
		"You are terse.",                 // persona
		"# Your Status",
	}
	for _, s := range required {
		if !strings.Contains(prompt, s) {
			t.Errorf("tool-less prompt must still contain %q", s)
		}
	}
}

// TestBuildSystemPrompt_ToolBackedKeepsToolInstructions guards against the
// gate leaking into the normal backends: a claude agent with identical
// on-disk state must still get the full contract.
func TestBuildSystemPrompt_ToolBackedKeepsToolInstructions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_toolful", Tool: ToolClaude, Persona: "You are terse."}
	seedToollessAgentFiles(t, a.ID)

	prompt := buildSystemPrompt(a, newQuietLogger(), "http://127.0.0.1:8080", nil, true)

	required := []string{
		"## Sending file attachments to the user",
		"## Calling the user",
		"## Memory Recall",
		"Memory Write — MANDATORY",
		"Your file storage directory is",
		"update the file with the Edit tool",
	}
	for _, s := range required {
		if !strings.Contains(prompt, s) {
			t.Errorf("tool-backed prompt must contain %q", s)
		}
	}
}

// TestBuildSystemPrompt_LegacyToolNameIsToolless covers an agent row that
// never went through normalizeAgent (a direct struct build, or a peer
// payload applied before hydration): the legacy "llama.cpp" spelling must
// still select the slim prompt rather than silently handing a tool-less
// model the full tool contract.
func TestBuildSystemPrompt_LegacyToolNameIsToolless(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_legacy_toolless", Tool: "llama.cpp"}
	seedToollessAgentFiles(t, a.ID)

	prompt := buildSystemPrompt(a, newQuietLogger(), "http://127.0.0.1:8080", nil, true)
	if strings.Contains(prompt, "## Calling the user") {
		t.Error("legacy llama.cpp tool name must still get the tool-less prompt")
	}
}
