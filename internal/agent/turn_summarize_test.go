package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// TurnSummarize
// ---------------------------------------------------------------------

// setupTurnAgent is setupIncrementalAgent but returns the agent's MAIN
// session file path (deterministic UUID name) — TurnSummarize targets
// that file explicitly instead of mtime-based discovery.
func setupTurnAgent(t *testing.T, agentID string) string {
	t.Helper()
	discovered := setupIncrementalAgent(t, agentID)
	return filepath.Join(filepath.Dir(discovered), agentIDToUUID(agentID)+".jsonl")
}

func TestTurnSummarize_NonClaudeToolNoop(t *testing.T) {
	agentID := "ag_turn_nonclaude"
	setupIncrementalAgent(t, agentID)

	fake := stubGenerateSummary(t)

	if err := TurnSummarize(agentID, "codex", testLogger()); err != nil {
		t.Fatalf("TurnSummarize: %v", err)
	}
	if fake.Calls != 0 {
		t.Errorf("expected 0 generateSummary calls for non-claude tool, got %d", fake.Calls)
	}
}

func TestTurnSummarize_BelowThresholdSkips(t *testing.T) {
	agentID := "ag_turn_below"
	sessionFile := setupTurnAgent(t, agentID)

	fake := stubGenerateSummary(t)

	writeSessionLine(t, sessionFile, userLine("hello there"))
	writeSessionLine(t, sessionFile, assistantLine("hi, how can I help?"))
	writeSessionLine(t, sessionFile, userLine("just checking in"))

	if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
		t.Fatalf("TurnSummarize: %v", err)
	}
	if fake.Calls != 0 {
		t.Errorf("expected 0 generateSummary calls below threshold, got %d", fake.Calls)
	}

	recentPath := filepath.Join(agentDir(agentID), "memory", recentSummaryFile)
	if _, err := os.Stat(recentPath); !os.IsNotExist(err) {
		t.Errorf("expected recent.md NOT to be created below threshold, stat err: %v", err)
	}
}

func TestTurnSummarize_AboveThresholdSummarizes(t *testing.T) {
	agentID := "ag_turn_above"
	sessionFile := setupTurnAgent(t, agentID)

	fake := stubGenerateSummary(t)

	// 20 messages of ~2000 chars each (~40,130 bytes total including the
	// "MSG-%d-" prefixes), comfortably above turnSummaryMinBacklogBytes
	// (32*1024 = 32,768) but each individual message stays at/under
	// summaryMsgMaxRunes (2000) so splitMessagesForSummary keeps this in
	// a single chunk (well under preCompactMaxPromptBytes).
	chunk := strings.Repeat("z", 2000)
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			writeSessionLine(t, sessionFile, userLine(fmt.Sprintf("MSG-%d-%s", i, chunk)))
		} else {
			writeSessionLine(t, sessionFile, assistantLine(fmt.Sprintf("MSG-%d-%s", i, chunk)))
		}
	}

	if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
		t.Fatalf("TurnSummarize (call 1): %v", err)
	}
	if fake.Calls < 1 {
		t.Fatalf("expected at least 1 generateSummary call above threshold, got %d", fake.Calls)
	}

	recent := recentMdContent(t, agentID)
	if !strings.Contains(recent, "SUMMARY") {
		t.Errorf("expected recent.md to contain the summary, got: %q", recent)
	}

	callsAfterFirst := fake.Calls

	// A second call immediately after should find the cursor already
	// advanced past all written content, so no additional LLM calls.
	if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
		t.Fatalf("TurnSummarize (call 2): %v", err)
	}
	if fake.Calls != callsAfterFirst {
		t.Errorf("expected no additional generateSummary calls on second call, got %d (was %d)", fake.Calls, callsAfterFirst)
	}
}

// TestTurnSummarize_ThresholdBoundary pins the exact byte boundary of the
// gate: turnSummaryMinBacklogBytes bytes of backlog must trigger
// summarization, one byte less must not. This guards against off-by-one
// regressions (e.g. flipping "<" to "<=") that the coarser above/below
// tests wouldn't catch since they use backlogs far from the boundary.
func TestTurnSummarize_ThresholdBoundary(t *testing.T) {
	t.Run("one byte below threshold: no call", func(t *testing.T) {
		agentID := "ag_turn_boundary_below"
		sessionFile := setupTurnAgent(t, agentID)
		fake := stubGenerateSummary(t)

		content := strings.Repeat("a", turnSummaryMinBacklogBytes-1)
		writeSessionLine(t, sessionFile, userLine(content))

		if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
			t.Fatalf("TurnSummarize: %v", err)
		}
		if fake.Calls != 0 {
			t.Errorf("expected 0 generateSummary calls at threshold-1 bytes, got %d", fake.Calls)
		}
	})

	t.Run("exactly at threshold: call made", func(t *testing.T) {
		agentID := "ag_turn_boundary_at"
		sessionFile := setupTurnAgent(t, agentID)
		fake := stubGenerateSummary(t)

		content := strings.Repeat("a", turnSummaryMinBacklogBytes)
		writeSessionLine(t, sessionFile, userLine(content))

		if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
			t.Fatalf("TurnSummarize: %v", err)
		}
		if fake.Calls < 1 {
			t.Errorf("expected at least 1 generateSummary call at exactly threshold bytes, got %d", fake.Calls)
		}
	})
}

func TestTurnSummarize_NoSessionFileNoop(t *testing.T) {
	agentID := "ag_turn_nofile"
	setupIncrementalAgent(t, agentID) // session file path returned but never created

	fake := stubGenerateSummary(t)

	if err := TurnSummarize(agentID, "claude", testLogger()); err != nil {
		t.Fatalf("TurnSummarize: %v", err)
	}
	if fake.Calls != 0 {
		t.Errorf("expected 0 generateSummary calls with no session file, got %d", fake.Calls)
	}
}

// ---------------------------------------------------------------------
// orderBackends
// ---------------------------------------------------------------------

func TestOrderBackends(t *testing.T) {
	newBackends := func() []cliBackend {
		return []cliBackend{
			{name: "claude"},
			{name: "codex"},
			{name: "grok"},
		}
	}

	tests := []struct {
		name          string
		preferredTool string
		want          []string
	}{
		{
			name:          "preferred already first stays first",
			preferredTool: "claude",
			want:          []string{"claude", "codex", "grok"},
		},
		{
			name:          "preferred in middle moves to front, rest keep relative order",
			preferredTool: "codex",
			want:          []string{"codex", "claude", "grok"},
		},
		{
			name:          "preferred last moves to front, rest keep relative order",
			preferredTool: "grok",
			want:          []string{"grok", "claude", "codex"},
		},
		{
			name:          "unknown preferred leaves original order",
			preferredTool: "nonexistent",
			want:          []string{"claude", "codex", "grok"},
		},
		{
			name:          "empty preferred leaves original order",
			preferredTool: "",
			want:          []string{"claude", "codex", "grok"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backends := newBackends()
			got := orderBackends(backends, tt.preferredTool)
			gotNames := make([]string, len(got))
			for i, b := range got {
				gotNames[i] = b.name
			}
			if !reflect.DeepEqual(gotNames, tt.want) {
				t.Errorf("orderBackends(%q) = %v, want %v", tt.preferredTool, gotNames, tt.want)
			}
		})
	}
}
