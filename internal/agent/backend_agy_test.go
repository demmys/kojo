package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	agyIDA = "3f2c9a1e-8b7d-4c6e-9f10-2a3b4c5d6e7f"
	agyIDB = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

func TestAgyConversationRef_KeysAreIsolated(t *testing.T) {
	dir := t.TempDir()
	writeAgyConversationID(dir, "", agyIDA, silentLogger())
	writeAgyConversationID(dir, "ag_x:slack:C1:1", agyIDB, silentLogger())
	if got := readAgyConversationID(dir, ""); got != agyIDA {
		t.Errorf("main = %q, want %q", got, agyIDA)
	}
	if got := readAgyConversationID(dir, "ag_x:slack:C1:1"); got != agyIDB {
		t.Errorf("keyed = %q, want %q", got, agyIDB)
	}
	if got := readAgyConversationID(dir, "ag_x:slack:C1:2"); got != "" {
		t.Errorf("unseen key = %q, want empty", got)
	}
	if p := agyConversationRefPath(dir, "../../etc/passwd"); !strings.HasPrefix(p, filepath.Join(dir, ".agy", "threads")+string(filepath.Separator)) {
		t.Errorf("keyed path escaped the threads dir: %q", p)
	}
}

func TestAgyConversationRef_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	writeAgyConversationID(dir, "", "--model=evil", silentLogger())
	if _, err := os.Stat(agyConversationRefPath(dir, "")); !os.IsNotExist(err) {
		t.Fatalf("malformed id was persisted (err=%v)", err)
	}
	path := agyConversationRefPath(dir, "")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("-c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readAgyConversationID(dir, ""); got != "" {
		t.Errorf("read malformed = %q, want empty", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("malformed ref was not removed")
	}
}

func TestBuildAgyArgs(t *testing.T) {
	a := &Agent{Model: "gemini-3.8-flash", Effort: "high"}
	args := buildAgyArgs(agyIDA, a, true)
	want := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--print-timeout", "0s",
		"--dangerously-skip-permissions",
		"--disable-slash-commands",
		"--conversation", agyIDA,
		"--agent", agyKojoAgentName,
		"--model", "gemini-3.8-flash",
		"--effort", "high",
	}
	if !slices.Equal(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
	args = buildAgyArgs("", &Agent{}, false)
	for _, f := range []string{"--conversation", "--continue", "-c", "--agent", "--model", "--effort"} {
		if slices.Contains(args, f) {
			t.Errorf("fresh args carry %s: %q", f, args)
		}
	}
}

func TestAgyUserMessageLine(t *testing.T) {
	line, err := agyUserMessageLine("hi\n\"there\"")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(line), "\n") || strings.Count(string(line), "\n") != 1 {
		t.Fatalf("not one NDJSON line: %q", line)
	}
	var ev struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Event != "user" || ev.Message.Content != "hi\n\"there\"" {
		t.Errorf("decoded = %+v", ev)
	}
}

const agyTestStream = `{"event":"init","conversation_id":"` + agyIDA + `","init":{"cwd":"/x","permission_mode":"always-proceed"}}
{"event":"step_update","step_update":{"conversation_id":"` + agyIDA + `","step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"command":"ls"}}}}
{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"command":"ls"},"output":"a.txt\n"}}}
{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"path":"nope"},"error":{"type":"NOT_FOUND","message":"no such file"}}}}
not json
{"event":"step_update","step_update":{"step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"app"}}
{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"le"}}
{"event":"result","result":{"conversation_id":"` + agyIDA + `","status":"SUCCESS","response":"apple","usage":{"input_tokens":100,"output_tokens":5,"thinking_tokens":7,"cache_read_tokens":40,"total_tokens":152}}}
`

func TestParseAgyStream(t *testing.T) {
	var events []ChatEvent
	results := 0
	res := parseAgyStream(strings.NewReader(agyTestStream), silentLogger(), func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}, func() { results++ })

	if res.cancelled || !res.gotResult || res.status != "SUCCESS" || results != 1 {
		t.Fatalf("result = %+v, onResult calls = %d", res, results)
	}
	if res.text != "apple" {
		t.Errorf("text = %q, want apple", res.text)
	}
	if res.conversationID != agyIDA {
		t.Errorf("conversation = %q", res.conversationID)
	}
	if res.usage == nil || res.usage.InputTokens != 100 || res.usage.OutputTokens != 12 || res.usage.CacheReadInputTokens != 40 {
		t.Errorf("usage = %+v", res.usage)
	}
	if len(res.toolUses) != 2 {
		t.Fatalf("toolUses = %+v", res.toolUses)
	}
	if tu := res.toolUses[0]; tu.Name != "run_command" || tu.Input != `{"command":"ls"}` || tu.Output != "a.txt\n" {
		t.Errorf("tool 0 = %+v", tu)
	}
	if tu := res.toolUses[1]; tu.Name != "view_file" || tu.Output != "no such file" {
		t.Errorf("tool 1 = %+v", tu)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Type)
	}
	want := []string{"tool_use", "tool_result", "tool_use", "tool_result", "text", "text"}
	if !slices.Equal(kinds, want) {
		t.Errorf("event types = %v, want %v", kinds, want)
	}
}

func TestParseAgyStream_ResponseFallbackAndCancel(t *testing.T) {
	in := `{"event":"result","result":{"status":"SUCCESS","response":"only here"}}` + "\n"
	res := parseAgyStream(strings.NewReader(in), silentLogger(), func(ChatEvent) bool { return true }, nil)
	if res.text != "only here" {
		t.Errorf("text = %q, want the result response", res.text)
	}

	res = parseAgyStream(strings.NewReader(agyTestStream), silentLogger(), func(ChatEvent) bool { return false }, nil)
	if !res.cancelled {
		t.Errorf("a refused send must mark the run cancelled")
	}
}

func TestClassifyAgyError(t *testing.T) {
	ok := &agyStreamResult{gotResult: true, status: "SUCCESS"}
	if msg := classifyAgyError(ok, nil, ""); msg != "" {
		t.Errorf("success classified as %q", msg)
	}
	auth := &agyStreamResult{gotResult: true, status: "ERROR", errorMessage: "authentication failed or timed out"}
	if msg := classifyAgyError(auth, nil, "Error: authentication required. Run 'agy' to log in, then retry."); !strings.Contains(msg, "sign in by running `agy`") {
		t.Errorf("auth error lacks the sign-in hint: %q", msg)
	}
	none := &agyStreamResult{}
	if msg := classifyAgyError(none, nil, "boom"); !strings.Contains(msg, "without a result") || !strings.Contains(msg, "boom") {
		t.Errorf("missing result = %q", msg)
	}
	if !isAgyStaleConversationError("conversation 123 not found") || isAgyStaleConversationError("rate limited") {
		t.Errorf("stale conversation detection is off")
	}
}

func TestRenderAgyMCPConfig(t *testing.T) {
	out, err := renderAgyMCPConfig(nil)
	if err != nil || out != "" {
		t.Fatalf("empty = %q, %v", out, err)
	}
	out, err = renderAgyMCPConfig(map[string]mcpServerEntry{
		"kojo":  {URL: "http://127.0.0.1:8081/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
		"local": {Command: "/bin/srv", Env: map[string]string{"A": "1"}},
		"bad":   {},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("servers = %v", cfg.MCPServers)
	}
	k := cfg.MCPServers["kojo"]
	if k["serverUrl"] != "http://127.0.0.1:8081/mcp" || k["command"] != nil || k["timeoutSeconds"] != float64(agyMCPTimeoutSeconds) {
		t.Errorf("kojo entry = %v", k)
	}
	l := cfg.MCPServers["local"]
	if l["command"] != "/bin/srv" || l["serverUrl"] != nil {
		t.Errorf("local entry = %v", l)
	}
	if args, ok := l["args"].([]any); !ok || len(args) != 0 {
		t.Errorf("local args = %v, want []", l["args"])
	}
}

func TestHoldAgyWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := agyAgentDefPath(dir)
	rel1, err := holdAgyWorkspace(context.Background(), dir, map[string]string{path: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "one" {
		t.Fatalf("content = %q", b)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	// Same content: shares the hold and restores an edited file.
	if err := os.WriteFile(path, []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel2, err := holdAgyWorkspace(context.Background(), dir, map[string]string{path: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "one" {
		t.Errorf("same-content hold did not restore the file: %q", b)
	}

	// Different content waits for both holds.
	got := make(chan error, 1)
	var rel3 func()
	go func() {
		var err error
		rel3, err = holdAgyWorkspace(context.Background(), dir, map[string]string{path: ""})
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("conflicting hold did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	rel1()
	rel1() // idempotent
	select {
	case <-got:
		t.Fatal("conflicting hold did not wait for the second user")
	case <-time.After(50 * time.Millisecond):
	}
	rel2()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty content should remove the file")
	}
	rel3()

	// A cancelled context gives up waiting.
	relA, _ := holdAgyWorkspace(context.Background(), dir, map[string]string{path: "a"})
	defer relA()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := holdAgyWorkspace(ctx, dir, map[string]string{path: "b"}); err == nil {
		t.Errorf("cancelled wait returned no error")
	}
}

// TestAgyChat_FakeBinary runs Chat against a stand-in agy that records
// its argv and stdin and replays a canned stream.
func TestAgyChat_FakeBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	argvLog := filepath.Join(bin, "argv")
	stdinLog := filepath.Join(bin, "stdin")
	stream := filepath.Join(bin, "stream")
	if err := os.WriteFile(stream, []byte(agyTestStream), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvLog + "\ncat > " + stdinLog + "\ncat " + stream + "\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewAgyBackend(silentLogger())
	agent := &Agent{ID: "ag_agytest", Model: "gemini-3.8-flash-high"}
	run := func(opts ChatOptions) ChatEvent {
		t.Helper()
		ch, err := b.Chat(context.Background(), agent, "hello", "SYSTEM PROMPT", opts)
		if err != nil {
			t.Fatal(err)
		}
		var last ChatEvent
		for e := range ch {
			last = e
		}
		return last
	}

	done := run(ChatOptions{})
	if done.Type != "done" || done.Message == nil || done.Message.Content != "apple" {
		t.Fatalf("final event = %+v", done)
	}
	dir := agentDir(agent.ID)
	if got := readAgyConversationID(dir, ""); got != agyIDA {
		t.Errorf("conversation ref = %q, want %q", got, agyIDA)
	}
	if b, _ := os.ReadFile(agyAgentDefPath(dir)); !strings.Contains(string(b), "SYSTEM PROMPT") {
		t.Errorf("agent def = %q", b)
	}
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "--conversation") || !strings.Contains(string(argv), "--agent\nkojo\n") {
		t.Errorf("first turn argv = %q", argv)
	}
	in, _ := os.ReadFile(stdinLog)
	if !strings.Contains(string(in), `"event":"user"`) || !strings.Contains(string(in), "hello") {
		t.Errorf("stdin = %q", in)
	}

	run(ChatOptions{})
	argv, _ = os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "--conversation\n"+agyIDA+"\n") {
		t.Errorf("second turn did not resume: %q", argv)
	}

	// A keyed session starts its own conversation; OneShot never persists.
	run(ChatOptions{SessionKey: "ag_agytest:slack:C1:1"})
	argv, _ = os.ReadFile(argvLog)
	if strings.Contains(string(argv), "--conversation") {
		t.Errorf("new keyed session resumed the main conversation: %q", argv)
	}
	if !agyCanResumeSession(agent.ID, "ag_agytest:slack:C1:1") {
		t.Errorf("keyed ref not stored")
	}
	if err := deleteAgyThreadRefStrict(agent.ID, "ag_agytest:slack:C1:1"); err != nil || agyCanResumeSession(agent.ID, "ag_agytest:slack:C1:1") {
		t.Errorf("delete keyed ref: err=%v", err)
	}
	if n, err := clearAgySessionCounted(agent.ID); err != nil || n != 1 {
		t.Errorf("clear = %d, %v; want 1 ref removed", n, err)
	}
}

// agy rejects --effort for models without a ladder, so a stored effort
// must not reach argv for them.
func TestBuildAgyArgs_DropsUnsupportedEffort(t *testing.T) {
	for _, m := range []string{"claude-sonnet-4-6", "gemini-3.8-flash-high"} {
		if args := buildAgyArgs("", &Agent{Model: m, Effort: "high"}, false); slices.Contains(args, "--effort") {
			t.Errorf("%s: args carry --effort: %q", m, args)
		}
	}
	if args := buildAgyArgs("", &Agent{Effort: "max"}, false); slices.Contains(args, "--effort") {
		t.Errorf("max reached argv: %q", args)
	}
}

func TestAgyConversationExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if !agyConversationExists(agyIDA) {
		t.Errorf("without a conversation store the id must be trusted")
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if agyConversationExists(agyIDA) {
		t.Errorf("missing conversation reported as existing")
	}
	if err := os.WriteFile(filepath.Join(dir, agyIDA+".db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !agyConversationExists(agyIDA) {
		t.Errorf("existing conversation not found")
	}
}
