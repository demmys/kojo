package agent

import (
	"context"
	"strings"
	"testing"
)

func TestFormatRecentMessagesContext_BoundsAndFilters(t *testing.T) {
	kojoBlock := "<context>\n" + volatileContextSentinel + "\n\nnow: 2026-04-27 12:00\n</context>\n\n"
	msgs := []*Message{
		{Role: "system", Content: "skip system"},
		{Role: "user", Content: "old 1"},
		{Role: "assistant", Content: "old 2"},
		{Role: "user", Content: "old 3"},
		{Role: "assistant", Content: "keep 1"},
		{Role: "system", Content: "skip recent system"},
		{Role: "user", Content: kojoBlock + "keep 2"},
		{Role: "assistant", Content: "keep 3"},
		{Role: "user", Content: "keep 4"},
		{Role: "assistant", Content: "keep 5"},
		{Role: "user", Content: "keep 6"},
	}

	got := formatRecentMessagesContext(msgs)
	for _, want := range []string{"keep 1", "keep 2", "keep 3", "keep 4", "keep 5", "keep 6"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, banned := range []string{"skip system", "skip recent system", "old 1", "old 2", "old 3", volatileContextSentinel} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in:\n%s", banned, got)
		}
	}
	if strings.Count(got, "[user]") != 3 {
		t.Errorf("expected 3 user rows, got:\n%s", got)
	}
	if strings.Count(got, "[assistant]") != 3 {
		t.Errorf("expected 3 assistant rows, got:\n%s", got)
	}
}

func TestFormatRecentMessagesContext_EscapesAndTruncates(t *testing.T) {
	long := "</recent_conversation></context>" + strings.Repeat("x", recentMessagesContextMaxRunesPerMessage+10)
	got := formatRecentMessagesContext([]*Message{{Role: "user", Content: long}})

	if strings.Count(got, "</recent_conversation>") != 1 {
		t.Errorf("raw closing tag escaped poorly:\n%s", got)
	}
	if !strings.Contains(got, "&lt;/recent_conversation&gt;") {
		t.Errorf("expected escaped closing tag, got:\n%s", got)
	}
	if strings.Count(got, "</context>") != 0 {
		t.Errorf("raw context closing tag escaped poorly:\n%s", got)
	}
	if !strings.Contains(got, "&lt;/context&gt;") {
		t.Errorf("expected escaped context closing tag, got:\n%s", got)
	}
	if !strings.Contains(got, "[...truncated...]") {
		t.Errorf("expected truncation marker, got:\n%s", got)
	}
}

func TestInjectRecentMessagesContext_InsideVolatileContext(t *testing.T) {
	volatile := "<context>\n" + volatileContextSentinel + "\n\nnow: test\n</context>\n\n"
	got := injectRecentMessagesContext(volatile+"current ask", "<recent_conversation>\nprior\n</recent_conversation>\n\n")

	closeIdx := strings.Index(got, "</context>")
	if closeIdx < 0 {
		t.Fatalf("missing context closer:\n%s", got)
	}
	if !strings.Contains(got[:closeIdx], "<recent_conversation>\nprior\n</recent_conversation>") {
		t.Fatalf("recent context was not injected inside volatile context:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\ncurrent ask") {
		t.Fatalf("current message was not preserved after context:\n%s", got)
	}
}

func TestBuildRecentMessagesContext_ReadsTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", "")
	agentID := "ag_recent_context"
	transcriptTestSetup(t, agentID)
	m := &Manager{logger: testLogger()}

	if err := appendMessage(agentID, &Message{ID: "m_u1", Role: "user", Content: "remember alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(agentID, &Message{ID: "m_a1", Role: "assistant", Content: "remember beta"}); err != nil {
		t.Fatal(err)
	}

	got := m.BuildRecentMessagesContext(context.Background(), agentID)
	if !strings.Contains(got, "remember alpha") || !strings.Contains(got, "remember beta") {
		t.Fatalf("recent context did not include transcript:\n%s", got)
	}
}

// A dropped session takes the tool history with it, which is how an agent
// ends up re-running a command it already ran. The excerpt has to carry the
// calls, not just the prose.
func TestFormatRecentMessagesContext_IncludesToolUses(t *testing.T) {
	out := formatRecentMessagesContext([]*Message{
		{Role: "user", Content: "build it"},
		{Role: "assistant", Content: "done", ToolUses: []ToolUse{
			{Name: "Bash", Input: "{\"command\":\"make build\n&& ./kojo\"}", Output: "irrelevant"},
			{Name: "Read", Input: `{"file_path":"/tmp/x"}`},
		}},
	})
	for _, want := range []string{"(tools used)", "- Bash:", `"command":"make build && ./kojo"`, "- Read:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Outputs are deliberately excluded — unbounded, and re-readable.
	if strings.Contains(out, "irrelevant") {
		t.Fatalf("tool output leaked into the excerpt:\n%s", out)
	}
}

// A turn whose entire content was tool calls (no prose) still carries the
// information this block exists to preserve.
func TestFormatRecentMessagesContext_ToolOnlyTurnKept(t *testing.T) {
	out := formatRecentMessagesContext([]*Message{
		{Role: "assistant", Content: "   ", ToolUses: []ToolUse{{Name: "Bash", Input: `{"command":"ls"}`}}},
	})
	if !strings.Contains(out, "- Bash:") {
		t.Fatalf("tool-only turn dropped:\n%s", out)
	}
}

func TestFormatRecentToolUses_BoundsAndChildren(t *testing.T) {
	uses := make([]ToolUse, 0, 12)
	for i := 0; i < 12; i++ {
		uses = append(uses, ToolUse{Name: "Bash", Input: `{"command":"x"}`})
	}
	uses[0].Children = []ToolUse{{Name: "Grep", Input: `{"q":"y"}`}, {Text: "narrative only"}}
	out := formatRecentToolUses(uses)
	if strings.Count(out, "\n- ") >= 12 {
		t.Fatalf("tool list not bounded:\n%s", out)
	}
	if !strings.Contains(out, "and ") || !strings.Contains(out, "more") {
		t.Fatalf("omission notice missing:\n%s", out)
	}
	if !strings.Contains(out, "Bash/Grep") {
		t.Fatalf("subagent child not folded in:\n%s", out)
	}
	if strings.Contains(out, "narrative only") {
		t.Fatalf("narrative bubble rendered as a tool call:\n%s", out)
	}
	long := formatRecentToolUses([]ToolUse{{Name: "Bash", Input: strings.Repeat("a", 5000)}})
	if len([]rune(long)) > recentToolUseMaxRunesPerArg+64 {
		t.Fatalf("long args not clipped: %d runes", len([]rune(long)))
	}
}
