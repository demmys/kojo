package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func useTempSystemPromptDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "system-prompts")
	prev := systemPromptDir
	systemPromptDir = func() string { return dir }
	t.Cleanup(func() { systemPromptDir = prev })
	return dir
}

func TestMaterializeClaudeSystemPromptMovesPromptOutOfArgv(t *testing.T) {
	dir := useTempSystemPromptDir(t)
	prompt := "persona\nMEMORY: Docker `ci:local` shard OOMs\n--not-a-flag"
	args := []string{"-p", "--model", "opus", "--system-prompt", prompt, "--effort", "high"}

	out, cleanup, err := materializeClaudeSystemPrompt(args)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(out, claudeSystemPromptFlag) {
		t.Fatalf("--system-prompt still in argv: %q", out)
	}
	for _, a := range out {
		if strings.Contains(a, "ci:local") {
			t.Fatalf("prompt text leaked into argv: %q", out)
		}
	}
	i := slices.Index(out, claudeSystemPromptFileFlag)
	if i < 0 || i+1 >= len(out) {
		t.Fatalf("missing %s: %q", claudeSystemPromptFileFlag, out)
	}
	want := []string{"-p", "--model", "opus", claudeSystemPromptFileFlag, out[i+1], "--effort", "high"}
	if !slices.Equal(out, want) {
		t.Fatalf("args = %q, want %q", out, want)
	}
	// The input slice must stay intact: callers fingerprint it.
	if args[3] != claudeSystemPromptFlag || args[4] != prompt {
		t.Fatalf("input args mutated: %q", args)
	}

	path := out[i+1]
	if filepath.Dir(path) != dir {
		t.Fatalf("file %q not under %q", path, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prompt {
		t.Fatalf("file content = %q, want %q", got, prompt)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
		}
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v, want 0700", st.Mode().Perm())
		}
	}

	cleanup()
	cleanup() // idempotent
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file not removed by cleanup: %v", err)
	}
}

func TestMaterializeClaudeSystemPromptNoPrompt(t *testing.T) {
	dir := useTempSystemPromptDir(t)
	args := []string{"-p", "--model", "opus"}
	out, cleanup, err := materializeClaudeSystemPrompt(args)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !slices.Equal(out, args) {
		t.Fatalf("args changed: %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir created without a prompt: %v", err)
	}
}

func TestSweepSystemPromptFiles(t *testing.T) {
	dir := useTempSystemPromptDir(t)
	if err := SweepSystemPromptFiles(); err != nil {
		t.Fatalf("sweep on missing dir: %v", err)
	}
	oldArgs, cleanOld, err := materializeClaudeSystemPrompt([]string{"--system-prompt", "old"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanOld()
	newArgs, cleanNew, err := materializeClaudeSystemPrompt([]string{"--system-prompt", "new"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanNew()
	past := time.Now().Add(-2 * systemPromptSweepAge)
	if err := os.Chtimes(oldArgs[1], past, past); err != nil {
		t.Fatal(err)
	}
	if err := SweepSystemPromptFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldArgs[1]); !os.IsNotExist(err) {
		t.Fatalf("stale file not swept: %v", err)
	}
	if _, err := os.Stat(newArgs[1]); err != nil {
		t.Fatalf("recent file swept: %v", err)
	}
	_ = dir
}

// TestSpawnSessionPassesSystemPromptViaFile runs spawnSession against a fake
// `claude` that records its argv and the prompt file content, then exits.
// The prompt must reach the CLI only through the file, and the file must be
// gone once the process has been reaped.
func TestSpawnSessionPassesSystemPromptViaFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI")
	}
	promptDir := useTempSystemPromptDir(t)
	bin := t.TempDir()
	out := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + out + `/argv"
while [ $# -gt 0 ]; do
  if [ "$1" = "--system-prompt-file" ]; then cat "$2" > "` + out + `/prompt"; fi
  shift
done
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewClaudeBackend(slog.New(slog.NewTextHandler(io.Discard, nil)))
	prompt := "MEMORY mentions ci:local and vitest"
	args := []string{"-p", "--system-prompt", prompt}
	s, err := b.spawnSession("ag_test", t.TempDir(), fingerprintArgs(args), args, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.awaitDead(5 * time.Second)

	argv, err := os.ReadFile(filepath.Join(out, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "ci:local") || strings.Contains(string(argv), "--system-prompt\n") {
		t.Fatalf("prompt leaked into argv:\n%s", argv)
	}
	got, err := os.ReadFile(filepath.Join(out, "prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prompt {
		t.Fatalf("CLI read prompt %q, want %q", got, prompt)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(promptDir)
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("prompt file not removed after exit: %d left", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
