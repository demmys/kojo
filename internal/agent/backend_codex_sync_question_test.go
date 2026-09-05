package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodexSyncQuestionFeatureCapability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command capture fixture requires a POSIX shell")
	}
	for _, interactive := range []bool{false, true} {
		name := "noninteractive"
		if interactive {
			name = "interactive"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			argsFile := filepath.Join(dir, "args")
			// Capture the actual spawned command, then exit before initialization.
			if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", argsFile)), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			opts := ChatOptions{OneShot: true}
			if interactive {
				opts.OnQuestionReady = func(AnswerFunc) {}
			}
			backend := NewCodexBackend(slog.Default())
			events, err := backend.Chat(ctx, &Agent{ID: "ag_flag_test", Tool: ToolCodex}, "test", "test", opts)
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(args), "features.default_mode_request_user_input=true"); got != interactive {
				t.Fatalf("feature enabled=%v, interactive=%v; args=%s", got, interactive, args)
			}
		})
	}
}
