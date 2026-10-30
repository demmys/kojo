package session

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

const ocTestSID = "ses_eb96caae1ffeNQtLNqB50oU8p4"

func TestToolBinary(t *testing.T) {
	for in, want := range map[string]string{toolGemini: "opencode", "claude": "claude", "codex": "codex", "grok": "grok"} {
		if got := toolBinary(in); got != want {
			t.Errorf("toolBinary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssignGeminiSessionID(t *testing.T) {
	for _, c := range []struct {
		in     []string
		wantID string
	}{
		{[]string{"--model", "x"}, ""}, // OpenCode assigns ids itself
		{[]string{"-s", ocTestSID}, ocTestSID},
		{[]string{"--session", ocTestSID, "--auto"}, ocTestSID},
		{[]string{"--session=" + ocTestSID}, ocTestSID},
		{[]string{"-s", "--auto"}, ""},
		{[]string{"--continue"}, ""},
		{[]string{"--session-id", "abc"}, ""}, // gemini-cli flag, not OpenCode's
	} {
		id, args := assignGeminiSessionID(c.in)
		if id != c.wantID || !reflect.DeepEqual(args, c.in) {
			t.Errorf("assignGeminiSessionID(%q) = %q, %q; want %q, unchanged args", c.in, id, args, c.wantID)
		}
	}
}

func TestBuildGeminiRestartArgs(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		tsid string
		want []string
	}{
		{"pinned id", []string{"--model", "m", "-s", ocTestSID}, ocTestSID, []string{"--model", "m", "-s", ocTestSID}},
		{"joined session replaced", []string{"--session=" + ocTestSID, "-m", "x"}, ocTestSID, []string{"-m", "x", "-s", ocTestSID}},
		{"fork dropped", []string{"-s", ocTestSID, "--fork"}, ocTestSID, []string{"-s", ocTestSID}},
		{"continue kept once", []string{"-c", "--auto", "--continue"}, "", []string{"--auto", "--continue"}},
		{"nothing", nil, "", []string{"--continue"}},
		{"legacy uuid id", []string{"-m", "x"}, "6b0f37f5-5d76-47ea-9d50-3a3add9c3936", []string{"-m", "x", "--continue"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := buildRestartArgs(toolGemini, c.in, c.tsid); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAppendYoloFlagGemini(t *testing.T) {
	if got := appendYoloFlag(toolGemini, []string{"-m", "x"}, true); !reflect.DeepEqual(got, []string{"-m", "x", "--auto"}) {
		t.Fatalf("got %q", got)
	}
	for _, in := range [][]string{{"--auto"}, {"--yolo"}, {"--dangerously-skip-permissions"}} {
		if got := appendYoloFlag(toolGemini, in, true); !reflect.DeepEqual(got, in) {
			t.Errorf("appendYoloFlag(%q) = %q; must not add --auto", in, got)
		}
	}
}

func TestPrepareLaunch(t *testing.T) {
	m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// gemini fails closed without a launcher.
	if _, _, _, err := m.prepareLaunch(toolGemini, "/bin/opencode", "/tmp", nil); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("gemini without launcher: err=%v", err)
	}
	// Other tools pass through untouched.
	p, a, c, err := m.prepareLaunch("claude", "/bin/claude", "/tmp", []string{"x"})
	if err != nil || p != "/bin/claude" || !reflect.DeepEqual(a, []string{"x"}) || c != nil {
		t.Fatalf("claude passthrough: %q %q %v", p, a, err)
	}

	var gotReq ToolLaunchRequest
	cleaned := false
	m.SetToolLauncher(toolGemini, func(req ToolLaunchRequest) (ToolLaunch, error) {
		gotReq = req
		return ToolLaunch{Path: "/w.sh", Args: []string{"-m", "google/gemini-4-argon"}, Cleanup: func() { cleaned = true }}, nil
	})
	p, a, c, err = m.prepareLaunch(toolGemini, "/bin/opencode", "/work", []string{"--effort", "high"})
	if err != nil || p != "/w.sh" || !reflect.DeepEqual(a, []string{"-m", "google/gemini-4-argon"}) {
		t.Fatalf("launcher result: %q %q %v", p, a, err)
	}
	if gotReq.ToolPath != "/bin/opencode" || gotReq.WorkDir != "/work" || !reflect.DeepEqual(gotReq.Args, []string{"--effort", "high"}) {
		t.Fatalf("launcher request: %+v", gotReq)
	}
	c()
	if !cleaned {
		t.Fatal("cleanup not propagated")
	}

	m.SetToolLauncher(toolGemini, func(ToolLaunchRequest) (ToolLaunch, error) { return ToolLaunch{}, errors.New("no key") })
	if _, _, _, err := m.prepareLaunch(toolGemini, "/bin/opencode", "/work", nil); err == nil {
		t.Fatal("launcher error swallowed")
	}
}

func TestLaunchedGeminiSessionID(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"-m", "google/x", "-s", ocTestSID}, ocTestSID},
		{[]string{"-m", "google/x", "--auto"}, ""},
		{[]string{"-s", "not-an-id"}, ""},
		{[]string{"-s"}, ""},
		{[]string{"-s", "ses_eb96caae1ffeAAAAAAAAAAAAAA", "-s", ocTestSID}, ocTestSID},
	} {
		if got := launchedGeminiSessionID(c.in); got != c.want {
			t.Fatalf("launchedGeminiSessionID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReapTool(t *testing.T) {
	m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Session{Tool: "gemini", WorkDir: "/work", ToolSessionID: "ses_eb8ffdbd0ffe067Bl9pwxaAw03"}
	m.reapTool(s) // no reaper: no-op

	var calls [][2]string
	m.SetToolReaper(toolGemini, func(workDir, toolSessionID string) {
		calls = append(calls, [2]string{workDir, toolSessionID})
	})
	m.reapTool(s)
	m.reapTool(&Session{Tool: "claude", WorkDir: "/w", ToolSessionID: "x"})
	m.reapTool(&Session{Tool: toolGemini, WorkDir: "/w"}) // no tool session
	if !reflect.DeepEqual(calls, [][2]string{{"/work", "ses_eb8ffdbd0ffe067Bl9pwxaAw03"}}) {
		t.Fatalf("reaper calls = %v", calls)
	}
	m.SetToolReaper(toolGemini, nil)
	m.reapTool(s)
	if len(calls) != 1 {
		t.Fatal("removed reaper still called")
	}
}

func TestNormalizeLegacyGeminiTool(t *testing.T) {
	if got := NormalizeToolName("gemini"); got != toolGemini {
		t.Fatalf("NormalizeToolName(gemini) = %q, want %q", got, toolGemini)
	}
	if got := toolBinary(toolGeminiAgy); got != "agy" {
		t.Fatalf("toolBinary(gemini-agy) = %q", got)
	}
	if got := newRestoredSession(SessionInfo{ID: "s_1", Tool: "gemini"}); got.Tool != toolGemini {
		t.Fatalf("restored legacy session tool = %q", got.Tool)
	}
}

func TestBuildAgyRestartArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		tsid string
		want []string
	}{
		{"nothing", nil, "", []string{"--continue"}},
		{"continue kept once", []string{"-c", "--model", "m", "--continue"}, "", []string{"--model", "m", "--continue"}},
		{"pinned split", []string{"--conversation", "abc", "--effort", "high"}, "", []string{"--effort", "high", "--conversation", "abc"}},
		{"pinned joined", []string{"--conversation=abc"}, "", []string{"--conversation", "abc"}},
		{"tool session wins", []string{"--conversation", "abc"}, "def", []string{"--conversation", "def"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildRestartArgs(toolGeminiAgy, c.in, c.tsid); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := appendYoloFlag(toolGeminiAgy, []string{"-m", "x"}, true); !reflect.DeepEqual(got, []string{"-m", "x", "--dangerously-skip-permissions"}) {
		t.Fatalf("yolo: got %q", got)
	}
}
