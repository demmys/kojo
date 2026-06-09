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
