package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grokSessionFixture writes a two-turn session directory shaped like the
// real thing: 34 chat lines, turn 0 starting at chat line 3 and turn 1 at
// chat line 21 (the counts observed in a live grok session).
func grokSessionFixture(t *testing.T, dir string, turn0, turn1 time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var chat strings.Builder
	chat.WriteString(`{"type":"system","content":"sys"}` + "\n")
	chat.WriteString(`{"type":"user","content":"user_info"}` + "\n")
	chat.WriteString(`{"type":"user","content":"reminder","synthetic_reason":"x"}` + "\n")
	for i := 3; i < 34; i++ {
		chat.WriteString(`{"type":"assistant","content":"line` + strconv.Itoa(i) + `"}` + "\n")
	}
	write(t, filepath.Join(dir, "chat_history.jsonl"), chat.String())

	var events strings.Builder
	events.WriteString(`{"type":"turn_started","ts":"` + turn0.Format(time.RFC3339Nano) +
		`","turn_number":0,"conversation_message_count":3}` + "\n")
	events.WriteString(`{"type":"phase_changed","ts":"` + turn0.Format(time.RFC3339Nano) + `","phase":"x"}` + "\n")
	events.WriteString(`{"type":"turn_ended","ts":"` + turn0.Add(time.Minute).Format(time.RFC3339Nano) +
		`","outcome":"completed"}` + "\n")
	events.WriteString(`{"type":"turn_started","ts":"` + turn1.Format(time.RFC3339Nano) +
		`","turn_number":1,"conversation_message_count":21}` + "\n")
	events.WriteString(`{"type":"phase_changed","ts":"` + turn1.Format(time.RFC3339Nano) + `","phase":"y"}` + "\n")
	events.WriteString(`{"type":"turn_ended","ts":"` + turn1.Add(time.Minute).Format(time.RFC3339Nano) +
		`","outcome":"completed"}` + "\n")
	write(t, filepath.Join(dir, "events.jsonl"), events.String())

	var updates strings.Builder
	updates.WriteString(`{"timestamp":` + strconv.Itoa(int(turn0.Unix())) + `,"method":"session/update"}` + "\n")
	updates.WriteString(`{"timestamp":` + strconv.Itoa(int(turn1.Unix())) + `,"method":"session/update"}` + "\n")
	write(t, filepath.Join(dir, "updates.jsonl"), updates.String())

	var rewinds strings.Builder
	rewinds.WriteString(`{"prompt_index":0,"created_at":"` + turn0.Format(time.RFC3339Nano) + `"}` + "\n")
	rewinds.WriteString(`{"prompt_index":1,"created_at":"` + turn1.Format(time.RFC3339Nano) + `"}` + "\n")
	write(t, filepath.Join(dir, "rewind_points.jsonl"), rewinds.String())

	write(t, filepath.Join(dir, "summary.json"),
		`{"info":{"id":"x"},"num_messages":2,"num_chat_messages":34,"updated_at":"`+
			turn1.Format(time.RFC3339Nano)+`","agent_name":"keep-me"}`+"\n")
	write(t, filepath.Join(dir, "system_prompt.txt"), "prompt\n")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		return 0
	}
	return strings.Count(string(body), "\n")
}

// The whole point of the feature: cutting inside turn 1 keeps turn 0's
// conversation on disk so the agent's context survives the rewind.
func TestTruncateGrokSessionDir_PreservesPrefix(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)

	// Threshold sits between the two turns.
	since := time.Date(2026, 8, 27, 1, 30, 0, 0, time.UTC)
	removed, deleted, _, err := truncateGrokSessionDir(dir, since)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if deleted {
		t.Fatal("session was deleted; the prefix should have survived")
	}
	if removed != 13 { // 34 - 21
		t.Fatalf("entriesRemoved = %d, want 13", removed)
	}
	if got := countLines(t, filepath.Join(dir, "chat_history.jsonl")); got != 21 {
		t.Fatalf("chat_history lines = %d, want 21", got)
	}
	if got := countLines(t, filepath.Join(dir, "events.jsonl")); got != 3 {
		t.Fatalf("events lines = %d, want 3 (cut at the turn_started we dropped)", got)
	}
	if got := countLines(t, filepath.Join(dir, "updates.jsonl")); got != 1 {
		t.Fatalf("updates lines = %d, want 1", got)
	}
	if got := countLines(t, filepath.Join(dir, "rewind_points.jsonl")); got != 1 {
		t.Fatalf("rewind_points lines = %d, want 1", got)
	}
	// The surviving chat prefix must still be the real turn-0 content.
	body, _ := os.ReadFile(filepath.Join(dir, "chat_history.jsonl"))
	if !strings.Contains(string(body), `"line20"`) || strings.Contains(string(body), `"line21"`) {
		t.Fatalf("chat_history cut at the wrong line:\n%s", body)
	}

	var summary map[string]any
	sb, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err := json.Unmarshal(sb, &summary); err != nil {
		t.Fatalf("summary.json unreadable after patch: %v", err)
	}
	if got := summary["num_chat_messages"]; got != float64(21) {
		t.Fatalf("num_chat_messages = %v, want 21", got)
	}
	if got := summary["num_messages"]; got != float64(1) {
		t.Fatalf("num_messages = %v, want 1", got)
	}
	if got := summary["agent_name"]; got != "keep-me" {
		t.Fatalf("unknown summary fields must survive; agent_name = %v", got)
	}
}

// A cut landing on the session's very first turn leaves nothing
// conversational, so the session is removed rather than left as an
// unusable --resume target.
func TestTruncateGrokSessionDir_DropsWhenFirstTurnCut(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)

	_, deleted, files, err := truncateGrokSessionDir(dir, turn0.Add(-time.Minute))
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if !deleted {
		t.Fatal("expected the session to be dropped")
	}
	if files != 6 {
		t.Fatalf("filesRemoved = %d, want 6", files)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir still present: %v", err)
	}
}

// A session entirely older than the threshold must come out byte-identical:
// its mtime feeds resume/idle decisions elsewhere.
func TestTruncateGrokSessionDir_NoOpWhenAllOlder(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)

	before, err := os.ReadFile(filepath.Join(dir, "chat_history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(filepath.Join(dir, "chat_history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	removed, deleted, _, err := truncateGrokSessionDir(dir, turn1.Add(time.Hour))
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if removed != 0 || deleted {
		t.Fatalf("removed=%d deleted=%v, want 0/false", removed, deleted)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "chat_history.jsonl"))
	if string(before) != string(after) {
		t.Fatal("chat_history rewritten on a no-op truncate")
	}
	stat2, _ := os.Stat(filepath.Join(dir, "chat_history.jsonl"))
	if !stat.ModTime().Equal(stat2.ModTime()) {
		t.Fatal("mtime changed on a no-op truncate")
	}
}

func TestFindGrokSessionCut_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	write(t, filepath.Join(dir, "events.jsonl"),
		"not json\n"+
			`{"type":"turn_started","turn_number":7}`+"\n"+ // no ts / count
			`{"type":"turn_started","ts":"nonsense","turn_number":7,"conversation_message_count":9}`+"\n"+
			`{"type":"turn_started","ts":"`+ts.Format(time.RFC3339Nano)+`","turn_number":4,"conversation_message_count":9}`+"\n")

	cut, err := findGrokSessionCut(filepath.Join(dir, "events.jsonl"), ts.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !cut.found || cut.chatLines != 9 || !cut.firstTurn || cut.eventLines != 3 {
		t.Fatalf("cut = %+v", cut)
	}
}

// A missing events.jsonl means we have no turn boundary to cut on, which
// must be a no-op rather than a wholesale delete.
func TestFindGrokSessionCut_MissingFile(t *testing.T) {
	cut, err := findGrokSessionCut(filepath.Join(t.TempDir(), "events.jsonl"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cut.found {
		t.Fatalf("cut = %+v, want not found", cut)
	}
}

// Rewinding to an assistant reply puts the boundary INSIDE that reply's
// turn (the message timestamp is later than the turn's turn_started).
// The whole turn must still be dropped — otherwise the reply the user
// rewound past stays in grok's replayed context.
func TestFindGrokSessionCut_BoundaryInsideTurn(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)

	// 30s after turn 1 started, i.e. while it was still running.
	cut, err := findGrokSessionCut(filepath.Join(dir, "events.jsonl"), turn1.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !cut.found || cut.chatLines != 21 || cut.firstTurn {
		t.Fatalf("cut = %+v, want the whole turn 1 dropped", cut)
	}
}

// A session whose events advertise more chat lines than the file holds is
// torn; resuming it would replay a mismatched prefix, so it is dropped.
func TestTruncateGrokSessionDir_DropsOnCountMismatch(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)
	write(t, filepath.Join(dir, "chat_history.jsonl"), `{"type":"system"}`+"\n")

	_, deleted, _, err := truncateGrokSessionDir(dir, time.Date(2026, 8, 27, 1, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("torn session should be dropped, not trimmed")
	}
}

// A non-monotonic conversation_message_count means events.jsonl has been
// rewritten or interleaved; the offsets it advertises can't be trusted.
func TestFindGrokSessionCut_RejectsNonMonotonicCounts(t *testing.T) {
	ts := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "events.jsonl"),
		`{"type":"turn_started","ts":"`+ts.Format(time.RFC3339Nano)+`","conversation_message_count":21}`+"\n"+
			`{"type":"turn_ended","ts":"`+ts.Add(time.Minute).Format(time.RFC3339Nano)+`"}`+"\n"+
			`{"type":"turn_started","ts":"`+ts.Add(time.Hour).Format(time.RFC3339Nano)+`","conversation_message_count":9}`+"\n")

	cut, err := findGrokSessionCut(filepath.Join(dir, "events.jsonl"), ts.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !cut.inconsistent {
		t.Fatalf("cut = %+v, want inconsistent", cut)
	}
}

// A turn_ended with an unreadable ts must not be mistaken for "the turn
// never finished" — that would cut a healthy prefix off an old session.
func TestFindGrokSessionCut_MalformedTurnEndedIsSkipped(t *testing.T) {
	ts := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "events.jsonl"),
		`{"type":"turn_started","ts":"`+ts.Format(time.RFC3339Nano)+`","conversation_message_count":3}`+"\n"+
			`{"type":"turn_ended","ts":"nonsense"}`+"\n"+
			`{"type":"turn_ended","ts":"`+ts.Add(time.Minute).Format(time.RFC3339Nano)+`"}`+"\n")

	cut, err := findGrokSessionCut(filepath.Join(dir, "events.jsonl"), ts.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if cut.found {
		t.Fatalf("cut = %+v, want no-op (the turn ended before the boundary)", cut)
	}
}

// A summary.json we cannot parse leaves the session inconsistent with the
// JSONL files we just trimmed, so the whole session is dropped.
func TestTruncateGrokSessionDir_DropsOnBrokenSummary(t *testing.T) {
	turn0 := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	turn1 := time.Date(2026, 8, 27, 2, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "01a03f02-bed3-76b1-9861-66ba0f22857f")
	grokSessionFixture(t, dir, turn0, turn1)
	write(t, filepath.Join(dir, "summary.json"), "{not json")

	_, deleted, _, err := truncateGrokSessionDir(dir, time.Date(2026, 8, 27, 1, 30, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("want an error for the unparseable summary")
	}
	if !deleted {
		t.Fatal("session should have been dropped after the partial rewrite")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("session dir still present: %v", serr)
	}
}
