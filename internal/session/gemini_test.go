package session

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func TestAssignGeminiSessionID(t *testing.T) {
	id, args := assignGeminiSessionID([]string{"--model", "x"})
	if len(id) != 36 || !reflect.DeepEqual(args, []string{"--model", "x", "--session-id", id}) {
		t.Fatalf("generated: id=%q args=%q", id, args)
	}
	for _, c := range []struct {
		in     []string
		wantID string
	}{
		{[]string{"--session-id", "abc"}, "abc"},
		{[]string{"--session-id=abc"}, "abc"},
		{[]string{"--resume", "latest"}, ""},
		{[]string{"-r", "3"}, ""},
		{[]string{"--resume=latest"}, ""},
		{[]string{"--session-file", "f.json"}, ""},
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
		{"pinned id", []string{"--model", "m", "--session-id", "S1"}, "S1", []string{"--model", "m", "--resume", "S1"}},
		{"user resume preserved", []string{"--resume", "U1", "--model", "m"}, "", []string{"--model", "m", "--resume", "U1"}},
		{"joined resume preserved", []string{"-r=5"}, "", []string{"--resume", "5"}},
		{"bare resume → latest", []string{"--resume", "--yolo"}, "", []string{"--yolo", "--resume", "latest"}},
		{"session-file dropped", []string{"--session-file", "f.json", "-m", "x"}, "", []string{"-m", "x", "--resume", "latest"}},
		{"nothing", nil, "", []string{"--resume", "latest"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := buildRestartArgs("gemini", c.in, c.tsid); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAppendYoloFlagGemini(t *testing.T) {
	if got := appendYoloFlag("gemini", []string{"-m", "x"}, true); !reflect.DeepEqual(got, []string{"-m", "x", "--yolo"}) {
		t.Fatalf("got %q", got)
	}
	for _, in := range [][]string{{"--approval-mode", "plan"}, {"--approval-mode=auto_edit"}, {"-y"}, {"--yolo"}} {
		if got := appendYoloFlag("gemini", in, true); !reflect.DeepEqual(got, in) {
			t.Errorf("appendYoloFlag(%q) = %q; must not add --yolo", in, got)
		}
	}
}

func TestPrepareLaunch(t *testing.T) {
	m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// gemini fails closed without a launcher.
	if _, _, _, err := m.prepareLaunch("gemini", "/bin/gemini", "/tmp", nil); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("gemini without launcher: err=%v", err)
	}
	// Other tools pass through untouched.
	p, a, c, err := m.prepareLaunch("claude", "/bin/claude", "/tmp", []string{"x"})
	if err != nil || p != "/bin/claude" || !reflect.DeepEqual(a, []string{"x"}) || c != nil {
		t.Fatalf("claude passthrough: %q %q %v", p, a, err)
	}

	var gotReq ToolLaunchRequest
	cleaned := false
	m.SetToolLauncher("gemini", func(req ToolLaunchRequest) (ToolLaunch, error) {
		gotReq = req
		return ToolLaunch{Path: "/w.sh", Args: []string{"--skip-trust"}, Cleanup: func() { cleaned = true }}, nil
	})
	p, a, c, err = m.prepareLaunch("gemini", "/bin/gemini", "/work", []string{"--effort", "high"})
	if err != nil || p != "/w.sh" || !reflect.DeepEqual(a, []string{"--skip-trust"}) {
		t.Fatalf("launcher result: %q %q %v", p, a, err)
	}
	if gotReq.ToolPath != "/bin/gemini" || gotReq.WorkDir != "/work" || !reflect.DeepEqual(gotReq.Args, []string{"--effort", "high"}) {
		t.Fatalf("launcher request: %+v", gotReq)
	}
	c()
	if !cleaned {
		t.Fatal("cleanup not propagated")
	}

	m.SetToolLauncher("gemini", func(ToolLaunchRequest) (ToolLaunch, error) { return ToolLaunch{}, errors.New("no key") })
	if _, _, _, err := m.prepareLaunch("gemini", "/bin/gemini", "/work", nil); err == nil {
		t.Fatal("launcher error swallowed")
	}
}
