package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func collectGeminiEvents(t *testing.T, lines ...string) ([]ChatEvent, *geminiStreamResult) {
	t.Helper()
	r := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var events []ChatEvent
	res := parseGeminiStream(r, silentLogger(), func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}, nil)
	return events, res
}

const geminiSampleSessionID = "fb573b1c-c020-4912-8ae5-08c14d3adc44"

func TestParseGeminiStream_Sample(t *testing.T) {
	events, res := collectGeminiEvents(t,
		`YOLO mode is enabled. All tool calls will be automatically approved.`,
		`{"type":"init","timestamp":"2026-10-15T00:00:00Z","session_id":"`+geminiSampleSessionID+`","model":"flash"}`,
		`{"type":"message","timestamp":"t","role":"user","content":"run echo hi"}`,
		`{"type":"tool_use","timestamp":"t","tool_name":"run_shell_command","tool_id":"run_shell_command__call_389850","parameters":{"command":"echo hi","description":"x"}}`,
		`{"type":"tool_result","timestamp":"t","tool_id":"run_shell_command__call_389850","status":"success","output":"hi"}`,
		`{"type":"message","timestamp":"t","role":"assistant","content":"The output is:\n\n","delta":true}`,
		`{"type":"message","timestamp":"t","role":"assistant","content":"hi","delta":true}`,
		`    at stack trace line`,
		`{"type":"result","timestamp":"t","status":"success","stats":{"total_tokens":20505,"input_tokens":20289,"output_tokens":42,"cached":100,"input":20189,"duration_ms":7921,"tool_calls":1,"models":{"gemini-3.8-flash":{}}}}`,
	)
	if res.sessionID != geminiSampleSessionID {
		t.Fatalf("sessionID = %q", res.sessionID)
	}
	if res.text != "The output is:\n\nhi" {
		t.Fatalf("text = %q", res.text)
	}
	if len(res.toolUses) != 1 || res.toolUses[0].Name != "run_shell_command" || res.toolUses[0].Output != "hi" ||
		!strings.Contains(res.toolUses[0].Input, `"echo hi"`) {
		t.Fatalf("toolUses = %+v", res.toolUses)
	}
	if res.usage == nil || res.usage.InputTokens != 20189 || res.usage.OutputTokens != 42 || res.usage.CacheReadInputTokens != 100 {
		t.Fatalf("usage = %+v", res.usage)
	}
	if res.resultStatus != "success" || res.streamError != "" || res.cancelled {
		t.Fatalf("status=%q err=%q cancelled=%v", res.resultStatus, res.streamError, res.cancelled)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "tool_use,tool_result,text,text" {
		t.Fatalf("event types = %s", got)
	}
	if events[1].ToolName != "run_shell_command" || events[1].ToolUseID != "run_shell_command__call_389850" {
		t.Fatalf("tool_result event = %+v", events[1])
	}
	if msg := classifyGeminiProcessError(nil, "", res); msg != "" {
		t.Fatalf("classify success = %q", msg)
	}
}

func TestParseGeminiStream_ErrorResult(t *testing.T) {
	_, res := collectGeminiEvents(t,
		`{"type":"init","timestamp":"t","session_id":"`+geminiSampleSessionID+`","model":"gemini-4-argon"}`,
		`{"type":"result","timestamp":"t","status":"error","error":{"type":"unknown","message":"[API Error: 429 quota exceeded]"},"stats":{"total_tokens":0,"input_tokens":0,"output_tokens":0,"cached":0,"input":0}}`,
	)
	if res.resultStatus != "error" || res.streamError != "[API Error: 429 quota exceeded]" {
		t.Fatalf("res = %+v", res)
	}
	if res.usage != nil {
		t.Fatalf("zero stats should not produce usage: %+v", res.usage)
	}
	if msg := classifyGeminiProcessError(nil, "", res); msg != "[API Error: 429 quota exceeded]" {
		t.Fatalf("classify = %q", msg)
	}
}

func TestParseGeminiStream_ErrorEventsAndToolError(t *testing.T) {
	_, res := collectGeminiEvents(t,
		`{"type":"tool_use","tool_name":"read_file","tool_id":"t1","parameters":{"path":"x"}}`,
		`{"type":"tool_result","tool_id":"t1","status":"error","output":"","error":{"type":"FILE_NOT_FOUND","message":"no such file"}}`,
		`{"type":"error","severity":"warning","message":"just a warning"}`,
		`{"type":"error","severity":"error","message":"Maximum session turns exceeded"}`,
		`{"type":"result","status":"error","stats":{}}`,
	)
	if res.toolUses[0].Output != "Error: no such file" {
		t.Fatalf("tool output = %q", res.toolUses[0].Output)
	}
	if got := classifyGeminiProcessError(nil, "", res); got != "Maximum session turns exceeded" {
		t.Fatalf("classify = %q", got)
	}
}

func TestParseGeminiStream_RejectsMalformedSessionID(t *testing.T) {
	_, res := collectGeminiEvents(t, `{"type":"init","session_id":"--cwd=/etc"}`)
	if res.sessionID != "" {
		t.Fatalf("sessionID = %q", res.sessionID)
	}
}

func TestParseGeminiStream_CancelStopsEarly(t *testing.T) {
	r := strings.NewReader(`{"type":"message","role":"assistant","content":"a"}` + "\n" +
		`{"type":"message","role":"assistant","content":"b"}` + "\n")
	calls := 0
	res := parseGeminiStream(r, silentLogger(), func(ChatEvent) bool { calls++; return false }, nil)
	if !res.cancelled || calls != 1 || res.text != "a" {
		t.Fatalf("cancelled=%v calls=%d text=%q", res.cancelled, calls, res.text)
	}
}

func TestClassifyGeminiProcessError_NoResult(t *testing.T) {
	res := &geminiStreamResult{}
	got := classifyGeminiProcessError(nil, "Error resuming session: Invalid session identifier \"x\".", res)
	if !strings.Contains(got, "Invalid session identifier") || !isGeminiStaleSessionError(got) {
		t.Fatalf("classify = %q", got)
	}
	if isGeminiStaleSessionError("[API Error: 429]") {
		t.Fatal("429 must not look stale")
	}
}

func TestBuildGeminiArgs(t *testing.T) {
	args := buildGeminiArgs("gemini-4-argon", "", []string{"/guide"})
	want := []string{"--skip-trust", "--yolo", "-o", "stream-json", "-m", "gemini-4-argon", "--include-directories", "/guide", "-p", ""}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q", args)
	}
	args = buildGeminiArgs("flash", geminiSampleSessionID, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume "+geminiSampleSessionID) || args[len(args)-2] != "-p" || args[len(args)-1] != "" {
		t.Fatalf("args = %q", args)
	}
}

func TestGeminiModelDefault(t *testing.T) {
	if got := geminiModel(&Agent{}); got != GeminiDefaultModel {
		t.Fatalf("default = %q", got)
	}
	if got := geminiModel(&Agent{Model: "gemini-3.8-flash"}); got != "gemini-3.8-flash" {
		t.Fatalf("model = %q", got)
	}
}

func TestGeminiCommandEnv_Sanitizes(t *testing.T) {
	for _, k := range []string{"GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_LOCATION", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_GEMINI_BASE_URL", "GEMINI_API_KEY", "GEMINI_CLI_HOME", "GEMINI_SYSTEM_MD", "GEMINI_MODEL"} {
		t.Setenv(k, "parent-"+k)
	}
	t.Setenv("GOOGLE_API_KEY_EXTRA_UNRELATED", "keep")
	env := geminiCommandEnv("ag_x", "/agents/ag_x", "kojo-key", "/agents/ag_x/.gemini-cli-home/kojo/system.md")
	for _, e := range env {
		if strings.Contains(e, "=parent-") {
			t.Fatalf("parent value leaked: %s", e)
		}
	}
	for _, want := range []string{
		"GEMINI_CLI_HOME=/agents/ag_x/.gemini-cli-home",
		"GEMINI_API_KEY=kojo-key",
		"GEMINI_SYSTEM_MD=/agents/ag_x/.gemini-cli-home/kojo/system.md",
		"KOJO_AGENT_ID=ag_x",
		"GOOGLE_API_KEY_EXTRA_UNRELATED=keep",
	} {
		if !stringSliceContains(env, want) {
			t.Fatalf("missing %q", want)
		}
	}
	env = geminiCommandEnv("ag_x", "/agents/ag_x", "k", "")
	for _, e := range env {
		if strings.HasPrefix(e, "GEMINI_SYSTEM_MD=") {
			t.Fatalf("unexpected %s", e)
		}
	}
}

func TestBuildGeminiSettings(t *testing.T) {
	s := buildGeminiSettings("gemini-4-argon", "low", map[string]mcpServerEntry{
		"kojo":  {URL: "http://127.0.0.1:1/mcp", Type: "http", Headers: map[string]string{"X-Kojo-Token": "tok"}},
		"local": {Command: "node", Args: []string{"srv.js"}, Env: map[string]string{"A": "1"}},
	})
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
		Tools struct {
			Exclude []string `json:"exclude"`
		} `json:"tools"`
		MCPServers map[string]struct {
			HTTPURL string            `json:"httpUrl"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Trust   bool              `json:"trust"`
		} `json:"mcpServers"`
		ModelConfigs struct {
			CustomOverrides []struct {
				Match       map[string]string `json:"match"`
				ModelConfig struct {
					GenerateContentConfig struct {
						ThinkingConfig struct {
							ThinkingLevel   string `json:"thinkingLevel"`
							IncludeThoughts bool   `json:"includeThoughts"`
						} `json:"thinkingConfig"`
					} `json:"generateContentConfig"`
				} `json:"modelConfig"`
			} `json:"customOverrides"`
		} `json:"modelConfigs"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Security.Auth.SelectedType != "gemini-api-key" {
		t.Fatalf("auth = %q", got.Security.Auth.SelectedType)
	}
	if !stringSliceContains(got.Tools.Exclude, "save_memory") {
		t.Fatalf("exclude = %v", got.Tools.Exclude)
	}
	k := got.MCPServers["kojo"]
	if k.HTTPURL != "http://127.0.0.1:1/mcp" || k.URL != "" || k.Headers["X-Kojo-Token"] != "tok" || !k.Trust {
		t.Fatalf("kojo mcp = %+v", k)
	}
	l := got.MCPServers["local"]
	if l.Command != "node" || len(l.Args) != 1 || l.Env["A"] != "1" || l.HTTPURL != "" {
		t.Fatalf("local mcp = %+v", l)
	}
	if len(got.ModelConfigs.CustomOverrides) != 1 ||
		got.ModelConfigs.CustomOverrides[0].Match["model"] != "gemini-4-argon" ||
		got.ModelConfigs.CustomOverrides[0].ModelConfig.GenerateContentConfig.ThinkingConfig.ThinkingLevel != "LOW" ||
		!got.ModelConfigs.CustomOverrides[0].ModelConfig.GenerateContentConfig.ThinkingConfig.IncludeThoughts {
		t.Fatalf("overrides = %+v", got.ModelConfigs.CustomOverrides)
	}

	// No effort / alias model → no override; no servers → no mcpServers key.
	// The flash alias keys the override on its resolved id.
	fl := buildGeminiSettings("flash", "high", nil)["modelConfigs"].(map[string]any)["customOverrides"].([]any)[0].(map[string]any)
	if fl["match"].(map[string]any)["model"] != "gemini-3.8-flash" {
		t.Fatalf("flash alias override = %+v", fl)
	}
	for _, tc := range [][2]string{{"gemini-4-argon", ""}, {"pro", "high"}, {"auto", "low"}} {
		s := buildGeminiSettings(tc[0], tc[1], nil)
		if _, ok := s["modelConfigs"]; ok {
			t.Fatalf("%v: unexpected modelConfigs", tc)
		}
		if _, ok := s["mcpServers"]; ok {
			t.Fatalf("%v: unexpected mcpServers", tc)
		}
	}
}

func TestValidToolModelEffort_Gemini(t *testing.T) {
	cases := []struct {
		model, effort string
		ok            bool
	}{
		{"", "", true},
		{"", "medium", true},
		{"gemini-4-argon", "low", true},
		{"gemini-4-argon", "high", true},
		{"gemini-3.8-flash", "medium", true},
		{"gemini-4-argon", "xhigh", false},
		{"gemini-4-argon", "max", false},
		{"gemini-4-argon", "minimal", false},
		{"flash", "low", true},
		{"pro", "low", false},
		{"auto", "medium", false},
		{"flash", "", true},
	}
	for _, c := range cases {
		if got := ValidToolModelEffort(ToolGemini, c.model, c.effort); got != c.ok {
			t.Errorf("ValidToolModelEffort(gemini, %q, %q) = %v, want %v", c.model, c.effort, got, c.ok)
		}
	}
}

func TestGeminiSessionRefs(t *testing.T) {
	dir := t.TempDir()
	writeGeminiSessionIDFor(dir, "", geminiSampleSessionID, silentLogger())
	if got := readGeminiSessionIDFor(dir, ""); got != geminiSampleSessionID {
		t.Fatalf("primary = %q", got)
	}
	if got := readGeminiSessionIDFor(dir, "slack:C1:1.2"); got != "" {
		t.Fatalf("keyed should be empty, got %q", got)
	}
	const keyed = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeGeminiSessionIDFor(dir, "slack:C1:1.2", keyed, silentLogger())
	if got := readGeminiSessionIDFor(dir, "slack:C1:1.2"); got != keyed {
		t.Fatalf("keyed = %q", got)
	}
	if !geminiSessionIDPersisted(dir, keyed) || !geminiSessionIDPersisted(dir, geminiSampleSessionID) {
		t.Fatal("persisted lookup failed")
	}
	writeGeminiSessionIDFor(dir, "x", "../../etc", silentLogger())
	if _, err := os.Stat(geminiSessionRefPath(dir, "x")); !os.IsNotExist(err) {
		t.Fatal("malformed id was written")
	}
	// A poisoned ref is dropped on read.
	if err := os.WriteFile(geminiSessionIDFile(dir), []byte("--resume=/etc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != "" {
		t.Fatalf("poisoned = %q", got)
	}
	if _, err := os.Stat(geminiSessionIDFile(dir)); !os.IsNotExist(err) {
		t.Fatal("poisoned ref not removed")
	}
}

func plantGeminiSession(t *testing.T, dir, sessionID string) string {
	t.Helper()
	chats := filepath.Join(geminiCLIHome(dir), ".gemini", "tmp", filepath.Base(dir), "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(chats, "session-2026-10-15T06-57-"+sessionID[:8]+".jsonl")
	if err := os.WriteFile(p, []byte(`{"sessionId":"`+sessionID+`","kind":"main"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGeminiSessionFiles(t *testing.T) {
	dir := t.TempDir()
	p := plantGeminiSession(t, dir, geminiSampleSessionID)
	if !geminiSessionExists(dir, geminiSampleSessionID) {
		t.Fatal("session should exist")
	}
	// Same 8-char prefix, different full id → not a match.
	if geminiSessionExists(dir, "fb573b1c-0000-0000-0000-000000000000") {
		t.Fatal("prefix collision matched")
	}
	removeGeminiSession(dir, geminiSampleSessionID)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("session file not removed")
	}
}

// fakeGeminiScript is a stand-in `gemini` that records its argv, env and
// stdin into $FAKE_GEMINI_LOG and emits a stream-json turn. FAKE env vars
// are not stripped by geminiCommandEnv (only GEMINI_* / GOOGLE_* are).
const fakeGeminiScript = `#!/bin/sh
log="$FAKE_GEMINI_LOG"
{
  echo "ARGS:$*"
  echo "HOME_ENV:$GEMINI_CLI_HOME"
  echo "KEY:$GEMINI_API_KEY"
  echo "GOOGLE:$GOOGLE_API_KEY"
  echo "SYSMD:$(cat "$GEMINI_SYSTEM_MD")"
  echo "STDIN:$(cat)"
  echo "SETTINGS:$(cat "$GEMINI_CLI_HOME/.gemini/settings.json" | tr -d '\n ')"
} >> "$log"
sid="$FAKE_GEMINI_SID"
for a in "$@"; do
  if [ "$prev" = "--resume" ]; then sid="$a"; fi
  prev="$a"
done
chats="$GEMINI_CLI_HOME/.gemini/tmp/$(basename "$PWD")/chats"
mkdir -p "$chats"
short=$(echo "$sid" | cut -c1-8)
f="$chats/session-2026-10-15T00-00-$short.jsonl"
[ -f "$f" ] || echo "{\"sessionId\":\"$sid\"}" > "$f"
now=$(date -u +%Y-%m-%dT%H:%M:%S.999Z)
echo "{\"id\":\"rec-$$\",\"timestamp\":\"$now\",\"type\":\"gemini\",\"content\":\"hello\",\"thoughts\":[{\"subject\":\"Plan\",\"description\":\"Say hello.\"}]}" >> "$f"
echo "Ripgrep is not available."
echo "{\"type\":\"init\",\"session_id\":\"$sid\",\"model\":\"m\"}"
echo '{"type":"message","role":"assistant","content":"hello","delta":true}'
echo '{"type":"result","status":"success","stats":{"input_tokens":10,"output_tokens":2,"cached":0,"input":10}}'
`

func setupFakeGemini(t *testing.T) (logPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gemini"), []byte(fakeGeminiScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath = filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_GEMINI_LOG", logPath)
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("GOOGLE_API_KEY", "should-be-stripped")
	return logPath
}

func runGeminiTurn(t *testing.T, b *GeminiBackend, a *Agent, msg string, opts ChatOptions) []ChatEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ch, err := b.Chat(ctx, a, msg, "SYSTEM PROMPT", opts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var evs []ChatEvent
	for e := range ch {
		evs = append(evs, e)
	}
	if len(evs) == 0 {
		t.Fatalf("no events (ctx err: %v)", ctx.Err())
	}
	return evs
}

func TestGeminiChat_PersistsAndResumes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	logPath := setupFakeGemini(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_gemini_chat", Effort: "high"}
	dir := agentDir(a.ID)

	t.Setenv("FAKE_GEMINI_SID", geminiSampleSessionID)
	evs := runGeminiTurn(t, b, a, "-first message", ChatOptions{})
	last := evs[len(evs)-1]
	if last.Type != "done" || last.Message == nil || last.Message.Content != "hello" || last.ErrorMessage != "" {
		t.Fatalf("last event = %+v", last)
	}
	if last.Message.Thinking != "**Plan**\nSay hello.\n\n" {
		t.Fatalf("thinking = %q", last.Message.Thinking)
	}
	var thinkingEvents int
	for _, e := range evs {
		if e.Type == "thinking" {
			thinkingEvents++
		}
	}
	if thinkingEvents != 1 {
		t.Fatalf("thinking events = %d", thinkingEvents)
	}
	if last.Usage == nil || last.Usage.InputTokens != 10 {
		t.Fatalf("usage = %+v", last.Usage)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != geminiSampleSessionID {
		t.Fatalf("stored session = %q", got)
	}
	logBody, _ := os.ReadFile(logPath)
	log := string(logBody)
	for _, want := range []string{
		"ARGS:--skip-trust --yolo -o stream-json -m gemini-4-argon -p",
		"HOME_ENV:" + geminiCLIHome(dir),
		"KEY:test-key",
		"GOOGLE:\n",
		"SYSMD:SYSTEM PROMPT",
		"STDIN:-first message",
		`"selectedType":"gemini-api-key"`,
		`"thinkingLevel":"HIGH"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "--resume") {
		t.Fatalf("first turn must not resume:\n%s", log)
	}
	// Per-turn system prompt file is cleaned up.
	if m, _ := filepath.Glob(filepath.Join(geminiKojoDir(dir), "system-*.md")); len(m) != 0 {
		t.Fatalf("system md leaked: %v", m)
	}

	// Second turn resumes the stored id.
	os.Remove(logPath)
	t.Setenv("FAKE_GEMINI_SID", "11111111-1111-1111-1111-111111111111")
	evs2 := runGeminiTurn(t, b, a, "second", ChatOptions{})
	logBody, _ = os.ReadFile(logPath)
	if !strings.Contains(string(logBody), "--resume "+geminiSampleSessionID) {
		t.Fatalf("second turn did not resume:\n%s", logBody)
	}
	if n := countThinking(evs2); n != 1 {
		t.Fatalf("resumed turn thinking events = %d (history re-emitted?)", n)
	}

	// A keyed turn gets its own session and does not touch the primary.
	os.Remove(logPath)
	const keyedSID = "22222222-2222-2222-2222-222222222222"
	t.Setenv("FAKE_GEMINI_SID", keyedSID)
	runGeminiTurn(t, b, a, "thread", ChatOptions{SessionKey: "slack:C:1"})
	logBody, _ = os.ReadFile(logPath)
	if strings.Contains(string(logBody), "--resume") {
		t.Fatalf("keyed first turn must not resume:\n%s", logBody)
	}
	if got := readGeminiSessionIDFor(dir, "slack:C:1"); got != keyedSID {
		t.Fatalf("keyed ref = %q", got)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != geminiSampleSessionID {
		t.Fatalf("primary ref changed to %q", got)
	}
	if !geminiCanResumeSession(a.ID, "slack:C:1") {
		t.Fatal("keyed session should be resumable")
	}

	// OneShot: fresh session, never persisted, session file removed.
	const oneShotSID = "33333333-3333-3333-3333-333333333333"
	t.Setenv("FAKE_GEMINI_SID", oneShotSID)
	runGeminiTurn(t, b, a, "oneshot", ChatOptions{OneShot: true})
	if geminiSessionExists(dir, oneShotSID) {
		t.Fatal("OneShot session file not cleaned up")
	}
	if geminiSessionIDPersisted(dir, oneShotSID) {
		t.Fatal("OneShot session persisted")
	}

	// Reset clears sessions and refs.
	if _, err := clearGeminiSessionCounted(a.ID); err != nil {
		t.Fatal(err)
	}
	if readGeminiSessionIDFor(dir, "") != "" || readGeminiSessionIDFor(dir, "slack:C:1") != "" {
		t.Fatal("refs not cleared")
	}
	if geminiSessionExists(dir, geminiSampleSessionID) {
		t.Fatal("session files not cleared")
	}
}

func TestGeminiChat_DropsRefWithoutSessionFile(t *testing.T) {
	logPath := setupFakeGemini(t)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_gemini_stale"}
	dir := agentDir(a.ID)
	writeGeminiSessionIDFor(dir, "", geminiSampleSessionID, silentLogger())
	t.Setenv("FAKE_GEMINI_SID", "44444444-4444-4444-4444-444444444444")
	runGeminiTurn(t, b, a, "hi", ChatOptions{})
	logBody, _ := os.ReadFile(logPath)
	if strings.Contains(string(logBody), "--resume") {
		t.Fatalf("stale ref was resumed:\n%s", logBody)
	}
	if got := readGeminiSessionIDFor(dir, ""); got != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("ref = %q", got)
	}
}

func TestGeminiChat_NoAPIKey(t *testing.T) {
	setupFakeGemini(t)
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("KOJO_GEMINI_API_KEY", "")
	b := NewGeminiBackend(silentLogger(), nil)
	_, err := b.Chat(context.Background(), &Agent{ID: "ag_gemini_nokey"}, "hi", "", ChatOptions{})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v", err)
	}
}

// TestGeminiChat_Live drives the real gemini CLI. Opt-in only:
// KOJO_GEMINI_LIVE=1 with `gemini` on PATH and a usable API key
// (KOJO_GEMINI_LIVE_MODEL overrides the model; effort is high).
func TestGeminiChat_Live(t *testing.T) {
	if os.Getenv("KOJO_GEMINI_LIVE") != "1" {
		t.Skip("set KOJO_GEMINI_LIVE=1 to run")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Setenv("GEMINI_API_KEY", "placeholder-wrapper-supplies-key")
	}
	model := os.Getenv("KOJO_GEMINI_LIVE_MODEL")
	if model == "" {
		model = GeminiDefaultModel
	}
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_gemini_live", Model: model, Effort: "high"}
	evs := runGeminiTurn(t, b, a, "Remember the word PELICAN. Also: how many positive integers below 500 are divisible by 3 or 7 but not by 21? Work it out carefully, then reply with just the number.", ChatOptions{})
	last := evs[len(evs)-1]
	if last.Type != "done" || last.ErrorMessage != "" || last.Message == nil || last.Message.Content == "" {
		t.Fatalf("turn1 = %+v", last)
	}
	// Every thought the CLI recorded for this turn must have been emitted.
	fileThoughts := 0
	if sid := readGeminiSessionIDFor(agentDir(a.ID), ""); sid != "" {
		for _, f := range geminiSessionFiles(agentDir(a.ID), sid) {
			body, _ := os.ReadFile(f)
			seen := map[string]int{}
			for _, line := range strings.Split(string(body), "\n") {
				var rec geminiSessionRecord
				if json.Unmarshal([]byte(line), &rec) == nil && rec.Type == "gemini" && len(rec.Thoughts) > seen[rec.ID] {
					seen[rec.ID] = len(rec.Thoughts)
				}
			}
			for _, n := range seen {
				fileThoughts += n
			}
		}
	}
	if countThinking(evs) != fileThoughts {
		t.Fatalf("thinking events = %d, session file thoughts = %d", countThinking(evs), fileThoughts)
	}
	if countThinking(evs) == 0 || last.Message.Thinking == "" {
		var dump string
		if sid := readGeminiSessionIDFor(agentDir(a.ID), ""); sid != "" {
			for _, f := range geminiSessionFiles(agentDir(a.ID), sid) {
				b, _ := os.ReadFile(f)
				dump += string(b)
			}
		}
		t.Fatalf("no thinking events (effort high): %+v\nsession file:\n%s", last.Message, dump)
	}
	if strings.Contains(last.Message.Content, last.Message.Thinking) {
		t.Fatalf("thinking leaked into content: %q", last.Message.Content)
	}
	t.Logf("turn1 thinking: %q / content: %q", last.Message.Thinking, last.Message.Content)
	sid := readGeminiSessionIDFor(agentDir(a.ID), "")
	if sid == "" || !geminiSessionExists(agentDir(a.ID), sid) {
		t.Fatalf("session not persisted: %q", sid)
	}
	evs = runGeminiTurn(t, b, a, "What word did I ask you to remember? Reply with just the word.", ChatOptions{})
	last = evs[len(evs)-1]
	if last.Message == nil || !strings.Contains(strings.ToUpper(last.Message.Content), "PELICAN") {
		t.Fatalf("turn2 = %+v", last)
	}
	if readGeminiSessionIDFor(agentDir(a.ID), "") != sid {
		t.Fatal("session id changed across resume")
	}
}

func TestGeminiThoughtTail(t *testing.T) {
	dir := t.TempDir()
	path := plantGeminiSession(t, dir, geminiSampleSessionID)
	appendLine := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Date(2026, 10, 15, 7, 40, 0, 0, time.UTC)
	// A record from an earlier turn that is already on disk.
	appendLine(`{"id":"old","timestamp":"2026-10-15T07:39:00.000Z","type":"gemini","content":"x","thoughts":[{"subject":"Old","description":"stale"}]}` + "\n")

	var got []string
	send := func(e ChatEvent) bool {
		if e.Type != "thinking" {
			t.Fatalf("unexpected event %+v", e)
		}
		got = append(got, e.Delta)
		return true
	}

	tail := newGeminiThoughtTail(dir, "", since)
	tail.poll(send) // no session yet → nothing
	tail.setSession(geminiSampleSessionID)
	tail.poll(send)
	if len(got) != 0 {
		t.Fatalf("old-turn thought emitted: %q", got)
	}

	appendLine(`{"$set":{"lastUpdated":"2026-10-15T07:40:01.000Z"}}` + "\n")
	appendLine(`{"id":"user1","timestamp":"2026-10-15T07:40:01.000Z","type":"user","content":[{"text":"hi"}]}` + "\n")
	rec := `{"id":"r1","timestamp":"2026-10-15T07:40:06.717Z","type":"gemini","content":"9.9","thoughts":[{"subject":"Assessing","description":"Compare 9.11 and 9.9.","timestamp":"2026-10-15T07:40:06.717Z"}]}`
	// Partial line: nothing until the newline arrives.
	appendLine(rec[:40])
	tail.poll(send)
	if len(got) != 0 {
		t.Fatalf("partial line emitted: %q", got)
	}
	appendLine(rec[40:] + "\n")
	tail.poll(send)
	if len(got) != 1 || got[0] != "**Assessing**\nCompare 9.11 and 9.9.\n\n" {
		t.Fatalf("got %q", got)
	}
	// Same id re-appended with an extra thought (record update): only the new one.
	appendLine(`{"id":"r1","timestamp":"2026-10-15T07:40:06.717Z","type":"gemini","content":"9.9","thoughts":[{"subject":"Assessing","description":"Compare 9.11 and 9.9."},{"subject":"","description":"Done."}],"toolCalls":[]}` + "\n")
	tail.poll(send)
	tail.poll(send)
	if len(got) != 2 || got[1] != "Done.\n\n" {
		t.Fatalf("got %q", got)
	}
	if tail.thinking() != got[0]+got[1] {
		t.Fatalf("thinking() = %q", tail.thinking())
	}

	// Resumed session: existing content (even with fresh timestamps) is
	// skipped by offset, but an unterminated trailing fragment is re-read
	// so a line completed later still parses.
	appendLine(`{"id":"r9","timestamp":"2026-10-15T07:40:30.000Z","type":"gemini","thoughts":[{"subject":"Torn"`)
	got = nil
	resumed := newGeminiThoughtTail(dir, geminiSampleSessionID, since.Add(20*time.Second))
	resumed.poll(send)
	if len(got) != 0 {
		t.Fatalf("resume re-emitted history: %q", got)
	}
	appendLine(`,"description":"x"}]}` + "\n")
	appendLine(`{"id":"r2","timestamp":"2026-10-15T07:41:00.000Z","type":"gemini","content":"","thoughts":[{"subject":"Next","description":""}]}` + "\n")
	resumed.poll(send)
	if len(got) != 2 || got[0] != "**Torn**\nx\n\n" || got[1] != "**Next**\n\n" {
		t.Fatalf("got %q", got)
	}

	// File replaced (same or larger size): re-read from the start; dedup
	// keeps r2 from repeating, the new r3 is emitted.
	body, _ := os.ReadFile(path)
	body = append(body, []byte(`{"id":"r3","timestamp":"2026-10-15T07:42:00.000Z","type":"gemini","thoughts":[{"subject":"Replaced","description":""}]}`+"\n")...)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	got = nil
	resumed.poll(send)
	if len(got) != 1 || got[0] != "**Replaced**\n\n" {
		t.Fatalf("after replace got %q", got)
	}
}

func countThinking(evs []ChatEvent) int {
	n := 0
	for _, e := range evs {
		if e.Type == "thinking" {
			n++
		}
	}
	return n
}
