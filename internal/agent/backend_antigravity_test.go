package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChunkAntigravitySystemPrompt(t *testing.T) {
	line := strings.Repeat("あ", 30) + "\n" // 91 bytes
	prompt := strings.Repeat(line, 1000) + strings.Repeat("い", 5000) + "\nend"
	chunks := chunkAntigravitySystemPrompt(prompt, 1000)
	if strings.Join(chunks, "") != prompt {
		t.Fatal("chunks do not reassemble to the original prompt")
	}
	for i, c := range chunks {
		if len(c) > 1000 {
			t.Fatalf("chunk %d is %d bytes, over the limit", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d splits a UTF-8 sequence", i)
		}
	}
	if got := chunkAntigravitySystemPrompt("", 1000); got != nil {
		t.Fatalf("empty prompt: got %d chunks", len(got))
	}
}

func TestRenderAntigravityRulesRespectsCap(t *testing.T) {
	prompt := strings.Repeat("日本語のシステムプロンプト。\n", 6000)
	files := renderAntigravityRules(prompt)
	if len(files) < 2 {
		t.Fatalf("expected several rule files, got %d", len(files))
	}
	var body strings.Builder
	for i := 1; i <= len(files); i++ {
		name := fmt.Sprintf("%s%02d.md", antigravityRulesFilePrefix, i)
		c, ok := files[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if len(c) > antigravityRuleChunkBytes {
			t.Fatalf("%s is %d bytes", name, len(c))
		}
		if !strings.HasPrefix(c, antigravityRulesFrontmatter) {
			t.Fatalf("%s lacks the always_on frontmatter", name)
		}
		rest := strings.TrimPrefix(c, antigravityRulesFrontmatter)
		nl := strings.IndexByte(rest, '\n')
		body.WriteString(rest[nl+1:])
	}
	if body.String() != prompt {
		t.Fatal("rule bodies do not reassemble to the prompt")
	}
}

func TestWriteAntigravityRulesRemovesStaleChunks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "operator.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravityRules(dir, renderAntigravityRules(strings.Repeat("x\n", 30000))); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravityRules(dir, renderAntigravityRules("short")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "kojo-system-01.md,operator.md" {
		t.Fatalf("unexpected rule files: %v", names)
	}
	if err := writeAntigravityRules(dir, renderAntigravityRules("")); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "operator.md" {
		t.Fatalf("empty prompt should leave only operator rules, got %v", entries)
	}
}

func TestHoldAntigravityRulesWaitsForDifferentPrompt(t *testing.T) {
	dir := t.TempDir()
	rel1, err := holdAntigravityRules(context.Background(), dir, "A")
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := holdAntigravityRules(context.Background(), dir, "A")
	if err != nil {
		t.Fatal("same prompt must share the hold:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := holdAntigravityRules(ctx, dir, "B"); err == nil {
		t.Fatal("different prompt must wait while A is held")
	}
	rel1()
	rel2()
	rel3, err := holdAntigravityRules(context.Background(), dir, "B")
	if err != nil {
		t.Fatal(err)
	}
	defer rel3()
	b, _ := os.ReadFile(filepath.Join(antigravityRulesDir(dir), "kojo-system-01.md"))
	if !strings.HasSuffix(string(b), "B") {
		t.Fatalf("rules not rewritten: %q", b)
	}
}

func TestAntigravityModelArg(t *testing.T) {
	cases := []struct{ model, effort, want string }{
		{"", "", ""},
		{"gemini-4-argon", "", "gemini-4-argon"},
		{"gemini-4-argon", "high", "gemini-4-argon-high"},
		{"", "low", "gemini-4-argon-low"},
		{"gpt-oss-120b", "", "gpt-oss-120b-medium"},
		{"claude-opus-5-5", "medium", "claude-opus-5-5-medium"},
		{"gemini-3.8-flash-low", "", "gemini-3.8-flash-low"},
	}
	for _, c := range cases {
		if got := antigravityModelArg(c.model, c.effort); got != c.want {
			t.Errorf("antigravityModelArg(%q,%q)=%q want %q", c.model, c.effort, got, c.want)
		}
	}
}

func TestValidToolModelEffortAntigravity(t *testing.T) {
	ok := [][2]string{{"", ""}, {"", "high"}, {"claude-opus-5-5", "low"}, {"gpt-oss-120b", "medium"}, {"gemini-4-argon-high", ""}}
	bad := [][2]string{{"gpt-oss-120b", "high"}, {"claude-opus-5-5", "xhigh"}, {"claude-opus-5-5", "max"}, {"gemini-4-argon-high", "low"}, {"unknown", "low"}}
	for _, c := range ok {
		if !ValidToolModelEffort(ToolAntigravity, c[0], c[1]) {
			t.Errorf("expected valid: %v", c)
		}
	}
	for _, c := range bad {
		if ValidToolModelEffort(ToolAntigravity, c[0], c[1]) {
			t.Errorf("expected invalid: %v", c)
		}
	}
}

func TestBuildAntigravityArgsPrintLast(t *testing.T) {
	args := buildAntigravityArgs("12927f4d-240b-4ec7-9639-c75a39e1b1f0", &Agent{Model: "gemini-3.8-flash", Effort: "low"})
	if args[len(args)-1] != "--print=" {
		t.Fatalf("--print= must be last: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--conversation 12927f4d-240b-4ec7-9639-c75a39e1b1f0", "--model gemini-3.8-flash-low", "--dangerously-skip-permissions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

const agySampleStream = `{"event":"init","conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","init":{"model":"gemini-3.8-flash","tools":["run_command"]}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Checking"}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"pwd"}}}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":2,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"pwd"},"output":"Command exited with code 0.\n/tmp\n"}}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":3,"state":"ERROR","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"false"},"error":{"type":"TOOL_ERROR","message":"boom"}}}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":4,"state":"ACTIVE","step_type":"agent_response","text_delta":"See [main.go](file:///x/main.go)"}}
{"event":"step_update","step_update":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","step_index":4,"state":"DONE","step_type":"agent_response","text_delta":" done.\n"}}
{"event":"result","result":{"conversation_id":"43b7f4a5-be76-4103-a869-590c9f38ad1f","status":"SUCCESS","response":"See main.go done.\n","num_turns":2,"usage":{"input_tokens":100,"output_tokens":10,"thinking_tokens":5,"cache_read_tokens":40,"total_tokens":115}}}
`

func TestParseAntigravityStream(t *testing.T) {
	var events []ChatEvent
	res := parseAntigravityStream(strings.NewReader(agySampleStream), slog.New(slog.NewTextHandler(io.Discard, nil)), func(e ChatEvent) bool {
		events = append(events, e)
		return true
	})
	if res.conversationID != "43b7f4a5-be76-4103-a869-590c9f38ad1f" {
		t.Fatalf("conversation id = %q", res.conversationID)
	}
	if res.text != "Checking\n\nSee [main.go](file:///x/main.go) done.\n" {
		t.Fatalf("text = %q", res.text)
	}
	if got := cleanAntigravityContent(res.text); got != "Checking\n\nSee main.go done.\n" {
		t.Fatalf("clean = %q", got)
	}
	if len(res.toolUses) != 2 || res.toolUses[0].Input != `{"CommandLine":"pwd"}` || !strings.Contains(res.toolUses[0].Output, "/tmp") {
		t.Fatalf("tool uses = %+v", res.toolUses)
	}
	if res.toolUses[1].Output != "Error: boom" {
		t.Fatalf("error tool output = %q", res.toolUses[1].Output)
	}
	if res.usage == nil || res.usage.InputTokens != 100 || res.usage.OutputTokens != 15 || res.usage.CacheReadInputTokens != 40 {
		t.Fatalf("usage = %+v", res.usage)
	}
	if msg := classifyAntigravityProcessError(nil, "", res); msg != "" {
		t.Fatalf("unexpected error %q", msg)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Type)
	}
	if strings.Join(kinds, ",") != "text,tool_use,tool_result,tool_use,tool_result,text,text" {
		t.Fatalf("events = %v", kinds)
	}
}

func TestParseAntigravityStreamErrors(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	noop := func(ChatEvent) bool { return true }

	bare := `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out"}` + "\n"
	res := parseAntigravityStream(strings.NewReader(bare), discard, noop)
	if got := classifyAntigravityProcessError(nil, "", res); got != "authentication failed or timed out" {
		t.Fatalf("bare result error = %q", got)
	}

	fatal := "error: model not available\n"
	res = parseAntigravityStream(strings.NewReader(fatal), discard, noop)
	if got := classifyAntigravityProcessError(nil, "", res); got != "error: model not available" {
		t.Fatalf("fatal line error = %q", got)
	}

	res = parseAntigravityStream(strings.NewReader(""), discard, noop)
	got := classifyAntigravityProcessError(nil, "warning: conversation \"x\" not found\nreal problem\n", res)
	if got != "antigravity exited without a result: real problem" {
		t.Fatalf("empty stream error = %q", got)
	}
}

func TestParseAntigravityStreamStopsOnCancel(t *testing.T) {
	res := parseAntigravityStream(strings.NewReader(agySampleStream), slog.New(slog.NewTextHandler(io.Discard, nil)), func(e ChatEvent) bool {
		return e.Type != "tool_use"
	})
	if !res.cancelled || res.text != "Checking" || len(res.toolUses) != 1 {
		t.Fatalf("cancel result = %+v", res)
	}
}

func TestAntigravitySessionRefsRejectInjectedIDs(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeAntigravitySessionIDFor(dir, "", "--model=x", logger)
	if _, err := os.Stat(antigravitySessionIDFile(dir)); !os.IsNotExist(err) {
		t.Fatal("non-UUID id was persisted")
	}
	if err := os.MkdirAll(antigravityStateDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(antigravitySessionIDFile(dir), []byte("../../etc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readAntigravitySessionIDFor(dir, ""); got != "" {
		t.Fatalf("poisoned ref read back as %q", got)
	}
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	writeAntigravitySessionIDFor(dir, "slack:C1:1.2", id, logger)
	if readAntigravitySessionIDFor(dir, "slack:C1:1.2") != id || !antigravitySessionIDPersisted(dir, id) {
		t.Fatal("keyed ref not round-tripped")
	}
	if readAntigravitySessionIDFor(dir, "") != "" {
		t.Fatal("keyed ref leaked into the main session")
	}
}

func TestRemoveAntigravityConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	other := "15414599-1403-4310-b19b-67e8da1e88b5"
	for _, p := range []string{
		"conversations/" + id + ".db", "conversations/" + id + ".db-wal", "annotations/" + id + ".pbtxt",
		"presence/" + id + ".lock", "brain/" + id + "/notes.md", "conversations/" + other + ".db",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !antigravityConversationExists(id) {
		t.Fatal("conversation should exist")
	}
	n, err := removeAntigravityConversation(id)
	if err != nil || n != 5 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if antigravityConversationExists(id) || !antigravityConversationExists(other) {
		t.Fatal("removed the wrong conversation")
	}
	if n, _ := removeAntigravityConversation("../conversations"); n != 0 {
		t.Fatal("non-UUID id must be ignored")
	}
}

func TestAntigravityRefusesSymlinkedWorkspaceDirs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "kojo-system-01.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlinked rules dir: no write, no stale-file removal through it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, antigravityRulesDir(dir)); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravityRules(antigravityRulesDir(dir), renderAntigravityRules("hi")); err == nil {
		t.Fatal("symlinked rules dir accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "kojo-system-01.md")); string(b) != "keep" {
		t.Fatal("write went through the symlink")
	}

	// Symlinked state dir: refs and ledger are not written through it.
	dir2 := t.TempDir()
	if err := os.Symlink(victim, antigravityStateDir(dir2)); err != nil {
		t.Fatal(err)
	}
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	writeAntigravitySessionIDFor(dir2, "", id, logger)
	recordAntigravityConversation(dir2, id, logger)
	for _, name := range []string{"session_id", "conversations"} {
		if _, err := os.Stat(filepath.Join(victim, name)); !os.IsNotExist(err) {
			t.Fatalf("%s written through the symlink", name)
		}
	}
}

func TestAntigravityRefusesSymlinkedRefFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(antigravityStateDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{antigravitySessionIDFile(dir), antigravityKnownFile(dir)} {
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
	}
	id := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	writeAntigravitySessionIDFor(dir, "", id, logger)
	recordAntigravityConversation(dir, id, logger)
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlinked ref file was written through: %q", b)
	}
}
