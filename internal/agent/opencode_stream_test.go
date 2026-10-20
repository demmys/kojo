package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func ocLine(typ, sid, rest string) string {
	return `{"type":"` + typ + `","sessionID":"` + sid + `",` + rest + `}`
}

func TestParseGeminiStream_LiveDeltas(t *testing.T) {
	delta := func(sid, id, kind, d string) string {
		return ocLine("kojo_delta", sid, `"part":{"id":"`+id+`","type":"`+kind+`"},"delta":"`+d+`"`)
	}
	evs, res := collectGeminiEvents(t,
		// Plugin lines can arrive before run's own step_start.
		delta(ocSID1, "r1", "reasoning", "**Pl"),
		ocLine("step_start", ocSID1, `"part":{"id":"s1","type":"step-start"}`),
		delta(ocSID1, "r1", "reasoning", "an**"),
		delta(ocSID3, "x1", "text", "subagent text"), // another session: dropped
		ocLine("reasoning", ocSID1, `"part":{"id":"r1","type":"reasoning","text":"**Plan**"}`),
		ocLine("kojo_tool_start", ocSID1, `"part":{"id":"t1","type":"tool","tool":"bash","callID":"c1","state":{"status":"running","input":{"command":"sleep 1"}}}`),
		ocLine("kojo_child_step", ocSID3, `"part":{"id":"cs1","type":"step-finish","reason":"stop","tokens":{"input":5,"output":6,"reasoning":7,"cache":{"read":8,"write":9}}}`),
		ocLine("kojo_child_step", ocSID3, `"part":{"id":"cs1","type":"step-finish","reason":"stop","tokens":{"input":5,"output":6,"reasoning":7,"cache":{"read":8,"write":9}}}`),
		ocLine("tool_use", ocSID1, `"part":{"id":"t1","type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"sleep 1"},"output":"ok"}}`),
		ocLine("step_finish", ocSID1, `"part":{"id":"f1","reason":"tool-calls","tokens":{"input":100,"output":10,"reasoning":1,"cache":{"read":0,"write":0}}}`),
		delta(ocSID1, "w1", "text", "  "), // whitespace-only start is held back
		delta(ocSID1, "w1", "text", "Hel"),
		delta(ocSID1, "w1", "text", "lo"),
		ocLine("text", ocSID1, `"part":{"id":"w1","type":"text","text":"  Hello world"}`),
		ocLine("text", ocSID1, `"part":{"id":"w2","type":"text","text":"Second"}`),
		delta(ocSID1, "w2", "text", "late"), // after completion: ignored
		delta(ocSID1, "w3", "text", "cut "),
		delta(ocSID1, "w3", "text", "off"), // never completed
		ocLine("step_finish", ocSID1, `"part":{"id":"f2","reason":"stop","tokens":{"input":200,"output":20,"reasoning":2,"cache":{"read":50,"write":3}}}`),
	)
	var got [][2]string
	for _, e := range evs {
		v := e.Delta
		if e.Type == "tool_use" {
			v = e.ToolName + " " + e.ToolInput
		}
		if e.Type == "tool_result" {
			v = e.ToolOutput
		}
		got = append(got, [2]string{e.Type, v})
	}
	want := [][2]string{
		{"thinking", "**Pl"},
		{"thinking", "an**"},
		{"tool_use", `bash {"command":"sleep 1"}`},
		{"tool_result", "ok"},
		{"text", "  Hel"},
		{"text", "lo"},
		{"text", " world"},
		{"text", "\n\nSecond"},
		{"text", "\n\ncut "},
		{"text", "off"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%q\nwant\n%q", got, want)
	}
	if res.sessionID != ocSID1 || !res.finished || !res.anyEvent {
		t.Fatalf("res = %+v", res)
	}
	if res.thinking != "**Plan**" {
		t.Fatalf("thinking = %q", res.thinking)
	}
	if res.text != "  Hello world\n\nSecond\n\ncut off" {
		t.Fatalf("text = %q", res.text)
	}
	if len(res.toolUses) != 1 || res.toolUses[0].Output != "ok" {
		t.Fatalf("toolUses = %+v", res.toolUses)
	}
	// Root steps plus the (deduplicated) subagent step.
	wantU := Usage{InputTokens: 100 + 200 + 3 + 5 + 9, OutputTokens: 10 + 1 + 20 + 2 + 6 + 7, CacheReadInputTokens: 50 + 8}
	if res.usage == nil || res.usage.InputTokens != wantU.InputTokens || res.usage.OutputTokens != wantU.OutputTokens || res.usage.CacheReadInputTokens != wantU.CacheReadInputTokens {
		t.Fatalf("usage = %+v, want %+v", res.usage, wantU)
	}
}

func TestParseGeminiStream_PluginLinesOnlyAreNotEvents(t *testing.T) {
	// A stale `-s` run prints no run events; plugin lines alone must not
	// mask that (anyEvent drives the stale-session retry).
	_, res := collectGeminiEvents(t, ocLine("kojo_delta", ocSID1, `"part":{"id":"a","type":"text"},"delta":"x"`))
	if res.anyEvent || res.sessionID != "" {
		t.Fatalf("res = %+v", res)
	}
}

func TestParseGeminiStream_DivergedCompletedPart(t *testing.T) {
	evs, res := collectGeminiEvents(t,
		ocLine("kojo_delta", ocSID1, `"part":{"id":"a","type":"text"},"delta":"abc"`),
		ocLine("text", ocSID1, `"part":{"id":"a","type":"text","text":"xyz"}`),
	)
	if len(evs) != 1 || evs[0].Delta != "abc" || res.text != "xyz" {
		t.Fatalf("evs = %+v, text = %q", evs, res.text)
	}
}

// TestOpenCodeEnvPlugin_Events runs the real plugin source under node with
// synthetic bus events and feeds its stdout to the parser.
func TestOpenCodeEnvPlugin_Events(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kojo-env.mjs"), []byte(openCodeEnvPlugin), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `import { KojoEnv } from "./kojo-env.mjs"
const h = await KojoEnv()
const ev = (type, properties) => h.event({ event: { id: "e", type, properties } })
const root = "` + ocSID1 + `", child = "` + ocSID3 + `"
await ev("session.created", { sessionID: child, info: { id: child, parentID: root } })
await ev("message.part.updated", { sessionID: root, part: { id: "r", sessionID: root, type: "reasoning", text: "" } })
await ev("message.part.delta", { sessionID: root, messageID: "m", partID: "r", field: "text", delta: "think" })
await ev("message.part.updated", { sessionID: root, part: { id: "t", sessionID: root, type: "text", text: "" } })
await ev("message.part.delta", { sessionID: root, messageID: "m", partID: "t", field: "text", delta: "he said \"hi\"\n" })
await ev("message.part.delta", { sessionID: root, messageID: "m", partID: "unknown", field: "text", delta: "dropped" })
await ev("message.part.updated", { sessionID: child, part: { id: "ct", sessionID: child, type: "text", text: "" } })
await ev("message.part.delta", { sessionID: child, messageID: "m2", partID: "ct", field: "text", delta: "child" })
await ev("message.part.updated", { sessionID: root, part: { id: "k", sessionID: root, type: "tool", tool: "bash", callID: "c", state: { status: "pending", input: {} } } })
await ev("message.part.updated", { sessionID: root, part: { id: "k", sessionID: root, type: "tool", tool: "bash", callID: "c", state: { status: "running", input: { command: "ls" } } } })
await ev("message.part.updated", { sessionID: root, part: { id: "k", sessionID: root, type: "tool", tool: "bash", callID: "c", state: { status: "running", input: { command: "ls" }, metadata: {} } } })
await ev("message.part.updated", { sessionID: child, part: { id: "cs", sessionID: child, type: "step-finish", reason: "stop", tokens: { input: 1, output: 2, reasoning: 3, cache: { read: 4, write: 5 } } } })
await ev("message.part.updated", { sessionID: root, part: { id: "rs", sessionID: root, type: "step-finish", reason: "stop", tokens: { input: 9, output: 9, reasoning: 9, cache: { read: 9, write: 9 } } } })
const out = {}
await h["shell.env"]({}, { env: out })
console.error(JSON.stringify(Object.fromEntries(Object.entries(out).map(([k, v]) => [k, v === undefined ? "<unset>" : v]))))
`
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stream string) (string, string) {
		cmd := exec.Command(node, "driver.mjs")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "KOJO_OC_STREAM=" + stream, "KOJO_OC_ORIG_XDG_CONFIG_HOME=/op"}
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("node: %v\n%s", err, stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	out, envOut := run("1")
	if !strings.Contains(envOut, `"KOJO_OC_STREAM":"<unset>"`) || !strings.Contains(envOut, `"XDG_CONFIG_HOME":"/op"`) {
		t.Fatalf("shell.env = %s", envOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("plugin lines = %d:\n%s", len(lines), out)
	}
	lines = append(lines, ocLine("step_start", ocSID1, `"part":{"id":"s","type":"step-start"}`))
	evs, res := collectGeminiEvents(t, lines...)
	var types []string
	for _, e := range evs {
		types = append(types, e.Type+":"+e.Delta+e.ToolInput)
	}
	wantTypes := []string{"thinking:think", "text:he said \"hi\"\n", `tool_use:{"command":"ls"}`}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Fatalf("events = %q, want %q", types, wantTypes)
	}
	if res.usage == nil || res.usage.InputTokens != 6 || res.usage.OutputTokens != 5 || res.usage.CacheReadInputTokens != 4 {
		t.Fatalf("child usage = %+v", res.usage)
	}
	if out, _ := run(""); out != "" {
		t.Fatalf("plugin wrote without %s=1:\n%s", openCodeStreamEnv, out)
	}
}
