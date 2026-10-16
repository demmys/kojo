package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	gemTestMainSID   = "6b0f37f5-5d76-47ea-9d50-3a3add9c3936"
	gemTestThreadSID = "0c1d2e3f-4a5b-4c6d-8e7f-901234567890"
	gemTestStaleSID  = "deadbeef-0000-4000-8000-000000000000"
	gemTestSubID     = "sub-agent-1"
)

func withGeminiTransferEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
}

func gemSessionBody(sid, text string) string {
	return `{"sessionId":"` + sid + `","projectHash":"x","kind":"main"}` + "\n" +
		`{"type":"user","content":[{"text":"` + text + `"}]}` + "\n"
}

func gemThreadRefName(key string) string { return "key-" + agentIDToUUID(key) }

// plantGeminiAgentSession writes a session JSONL under the agent's
// CLI home (slug = slugify(basename)) and points the given ref at it.
func plantGeminiAgentSession(t *testing.T, agentID, refName, sid, body string) string {
	t.Helper()
	dir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		t.Fatal(err)
	}
	chats := filepath.Join(geminiCLIHome(dir), ".gemini", "tmp", geminiSlugify(filepath.Base(dir)), "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(chats, "session-2026-10-15T09-13-"+sid[:8]+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGeminiRef(t, dir, refName, sid)
	return p
}

func writeGeminiRef(t *testing.T, dir, refName, sid string) {
	t.Helper()
	ref := geminiSessionIDFile(dir)
	if refName != "" {
		ref = filepath.Join(geminiThreadRefDir(dir), refName)
	}
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, []byte(sid), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gemRelPaths(files []GeminiSessionFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	sort.Strings(out)
	return out
}

func TestReadGeminiSessionFiles_MainThreadSubagentAndStale(t *testing.T) {
	withGeminiTransferEnv(t)
	const agentID = "ag_gem_read"
	thread := gemThreadRefName("slack:C1:123")
	mainPath := plantGeminiAgentSession(t, agentID, "", gemTestMainSID, gemSessionBody(gemTestMainSID, "hello"))
	plantGeminiAgentSession(t, agentID, thread, gemTestThreadSID, gemSessionBody(gemTestThreadSID, "thread"))
	// Subagent log of the main session.
	sub := filepath.Join(filepath.Dir(mainPath), gemTestMainSID)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, gemTestSubID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale thread ref: no session file behind it.
	dir, _ := filepath.Abs(AgentDir(agentID))
	writeGeminiRef(t, dir, gemThreadRefName("stale"), gemTestStaleSID)
	// A second thread pointing at the main session shares its files.
	shared := gemThreadRefName("shared")
	writeGeminiRef(t, dir, shared, gemTestMainSID)

	tr, skipped, err := ReadGeminiSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	if tr == nil {
		t.Fatal("nil transfer")
	}
	if tr.Refs[0].Name != "" || tr.Refs[0].SessionID != gemTestMainSID {
		t.Fatalf("main ref not first: %+v", tr.Refs)
	}
	gotRefs := map[string]string{}
	for _, r := range tr.Refs {
		gotRefs[r.Name] = r.SessionID
	}
	want := map[string]string{"": gemTestMainSID, thread: gemTestThreadSID, shared: gemTestMainSID}
	if len(gotRefs) != len(want) {
		t.Fatalf("refs = %+v, want %+v", gotRefs, want)
	}
	for k, v := range want {
		if gotRefs[k] != v {
			t.Fatalf("ref %q = %q, want %q", k, gotRefs[k], v)
		}
	}
	rels := gemRelPaths(tr.Files)
	wantRels := []string{
		gemTestMainSID + "/" + gemTestSubID + ".jsonl",
		"session-2026-10-15T09-13-" + gemTestThreadSID[:8] + ".jsonl",
		"session-2026-10-15T09-13-" + gemTestMainSID[:8] + ".jsonl",
	}
	sort.Strings(wantRels)
	if strings.Join(rels, ",") != strings.Join(wantRels, ",") {
		t.Fatalf("files = %v, want %v", rels, wantRels)
	}
	// Source state is untouched (no self-healing delete of the stale ref).
	if _, err := os.Stat(filepath.Join(geminiThreadRefDir(dir), gemThreadRefName("stale"))); err != nil {
		t.Fatalf("stale ref removed on read: %v", err)
	}
}

func TestReadGeminiSessionFiles_NoRefs(t *testing.T) {
	withGeminiTransferEnv(t)
	tr, skipped, err := ReadGeminiSessionFiles("ag_gem_none")
	if err != nil || tr != nil || len(skipped) != 0 {
		t.Fatalf("got %+v %+v %v", tr, skipped, err)
	}
}

func TestReadGeminiSessionFiles_OversizedSessionSkipped(t *testing.T) {
	withGeminiTransferEnv(t)
	prev := geminiSessionFileMaxBytes
	geminiSessionFileMaxBytes = 64
	t.Cleanup(func() { geminiSessionFileMaxBytes = prev })

	const agentID = "ag_gem_big"
	plantGeminiAgentSession(t, agentID, "", gemTestMainSID, gemSessionBody(gemTestMainSID, strings.Repeat("x", 200)))
	thread := gemThreadRefName("t")
	plantGeminiAgentSession(t, agentID, thread, gemTestThreadSID, `{"sessionId":"`+gemTestThreadSID+`"}`+"\n")

	tr, skipped, err := ReadGeminiSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Path != "gemini-session/"+gemTestMainSID || skipped[0].Reason != "oversized" {
		t.Fatalf("skipped = %+v", skipped)
	}
	if tr == nil || len(tr.Refs) != 1 || tr.Refs[0].Name != thread || len(tr.Files) != 1 {
		t.Fatalf("transfer = %+v", tr)
	}
}

func TestStageGeminiSession_RoundTripAndRollback(t *testing.T) {
	withGeminiTransferEnv(t)
	const srcID = "ag_gem_src"
	thread := gemThreadRefName("k")
	plantGeminiAgentSession(t, srcID, "", gemTestMainSID, gemSessionBody(gemTestMainSID, "main"))
	plantGeminiAgentSession(t, srcID, thread, gemTestThreadSID, gemSessionBody(gemTestThreadSID, "thread"))
	tr, _, err := ReadGeminiSessionFiles(srcID)
	if err != nil || tr == nil {
		t.Fatalf("read: %+v %v", tr, err)
	}

	// Target agent with pre-existing divergent state: an old session, an
	// old thread ref that is not in the transfer, and a marker owned by
	// another path in the predicted slug's tmp dir (must be swapped out).
	const dstID = "ag_gem_dst"
	oldPath := plantGeminiAgentSession(t, dstID, "", gemTestStaleSID, gemSessionBody(gemTestStaleSID, "old"))
	dstDir, _ := filepath.Abs(AgentDir(dstID))
	oldThread := gemThreadRefName("old")
	writeGeminiRef(t, dstDir, oldThread, gemTestStaleSID)
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(oldPath)), ".project_root"), []byte("/elsewhere/ag_gem_dst"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit, rollback, err := StageGeminiSession(dstID, tr)
	if err != nil {
		t.Fatal(err)
	}
	if got := readGeminiRefRaw(geminiSessionIDFile(dstDir)); got != gemTestMainSID {
		t.Fatalf("main ref = %q", got)
	}
	if got := readGeminiRefRaw(filepath.Join(geminiThreadRefDir(dstDir), thread)); got != gemTestThreadSID {
		t.Fatalf("thread ref = %q", got)
	}
	if _, err := os.Stat(filepath.Join(geminiThreadRefDir(dstDir), oldThread)); !os.IsNotExist(err) {
		t.Fatalf("old thread ref survived: %v", err)
	}
	slugDir := filepath.Join(geminiCLIHome(dstDir), ".gemini", "tmp", geminiSlugify(dstID))
	if _, err := os.Stat(filepath.Join(slugDir, ".project_root")); !os.IsNotExist(err) {
		t.Fatalf("marker shipped/kept: %v", err)
	}
	if got := geminiSessionFiles(dstDir, gemTestMainSID); len(got) != 1 || !strings.HasPrefix(got[0], slugDir) {
		t.Fatalf("main session not resolvable on target: %v", got)
	}
	if got := geminiSessionFiles(dstDir, gemTestStaleSID); len(got) != 0 {
		t.Fatalf("old session survived: %v", got)
	}

	rollback()
	commit() // no-op after rollback
	if got := readGeminiRefRaw(geminiSessionIDFile(dstDir)); got != gemTestStaleSID {
		t.Fatalf("rollback main ref = %q", got)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("rollback lost old session: %v", err)
	}
	if got := readGeminiRefRaw(filepath.Join(geminiThreadRefDir(dstDir), oldThread)); got != gemTestStaleSID {
		t.Fatalf("rollback old thread ref = %q", got)
	}
	if _, err := os.Stat(filepath.Join(geminiThreadRefDir(dstDir), thread)); !os.IsNotExist(err) {
		t.Fatalf("rollback left new thread ref: %v", err)
	}
	assertNoGeminiStagingLeftovers(t, dstDir)

	// Stage again and commit: backups are dropped, lock released.
	commit, _, err = StageGeminiSession(dstID, tr)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	assertNoGeminiStagingLeftovers(t, dstDir)
	if got := geminiSessionFiles(dstDir, gemTestThreadSID); len(got) != 1 {
		t.Fatalf("thread session missing after commit: %v", got)
	}
	// Lock was released: a further stage must not deadlock.
	c2, _, err := StageGeminiSessionCleanup(dstID)
	if err != nil {
		t.Fatal(err)
	}
	c2()
	if _, err := os.Stat(geminiKojoDir(dstDir)); err != nil {
		t.Fatalf("kojo dir removed: %v", err)
	}
	if refs := listGeminiRefs(dstDir); len(refs) != 0 {
		t.Fatalf("cleanup left refs: %+v", refs)
	}
	assertNoGeminiStagingLeftovers(t, dstDir)
}

func assertNoGeminiStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	for _, root := range []string{filepath.Join(geminiCLIHome(dir), ".gemini"), geminiKojoDir(dir)} {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if strings.Contains(e.Name(), "kojo-bk") || strings.Contains(e.Name(), "staging") || strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("leftover %s in %s", e.Name(), root)
			}
		}
	}
}

func TestStageGeminiSession_Rejects(t *testing.T) {
	withGeminiTransferEnv(t)
	good := GeminiSessionFile{
		RelPath: "session-2026-10-15T09-13-" + gemTestMainSID[:8] + ".jsonl",
		Content: []byte(gemSessionBody(gemTestMainSID, "x")),
	}
	cases := map[string]*GeminiSessionTransfer{
		"traversal relpath": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good, {RelPath: "../x.jsonl", Content: []byte("{}")}},
		},
		"nested relpath": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good, {RelPath: gemTestMainSID + "/a/b.jsonl", Content: []byte("{}")}},
		},
		"marker relpath": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good, {RelPath: ".project_root", Content: []byte("/x")}},
		},
		"bad session id": {
			Refs:  []GeminiSessionRef{{SessionID: "../../etc"}},
			Files: []GeminiSessionFile{good},
		},
		"bad ref name": {
			Refs:  []GeminiSessionRef{{Name: "../session_id", SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good},
		},
		"ref without session file": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestThreadSID}},
			Files: []GeminiSessionFile{good},
		},
		"duplicate relpath (case-insensitive)": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good, {RelPath: strings.ToUpper(good.RelPath[:7]) + good.RelPath[7:], Content: good.Content}},
		},
		"duplicate ref": {
			Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}, {SessionID: gemTestMainSID}},
			Files: []GeminiSessionFile{good},
		},
	}
	const agentID = "ag_gem_reject"
	plantGeminiAgentSession(t, agentID, "", gemTestStaleSID, gemSessionBody(gemTestStaleSID, "keep"))
	dir, _ := filepath.Abs(AgentDir(agentID))
	for name, tr := range cases {
		if _, _, err := StageGeminiSession(agentID, tr); err == nil {
			t.Errorf("%s: expected error", name)
		}
		// Rejections happen before any swap: prior state intact.
		if got := readGeminiRefRaw(geminiSessionIDFile(dir)); got != gemTestStaleSID {
			t.Fatalf("%s: prior ref clobbered: %q", name, got)
		}
		assertNoGeminiStagingLeftovers(t, dir)
	}
	// Oversized file.
	prev := geminiSessionFileMaxBytes
	geminiSessionFileMaxBytes = 8
	defer func() { geminiSessionFileMaxBytes = prev }()
	if _, _, err := StageGeminiSession(agentID, &GeminiSessionTransfer{
		Refs: []GeminiSessionRef{{SessionID: gemTestMainSID}}, Files: []GeminiSessionFile{good},
	}); err == nil {
		t.Error("oversized: expected error")
	}
}

func TestStageGeminiSession_EmptyIsNoOp(t *testing.T) {
	withGeminiTransferEnv(t)
	c, r, err := StageGeminiSession("ag_gem_empty", &GeminiSessionTransfer{})
	if c != nil || r != nil || err != nil {
		t.Fatalf("got %v %v %v", c != nil, r != nil, err)
	}
	c, r, err = StageGeminiSessionCleanup("ag_gem_empty")
	if c != nil || r != nil || err != nil {
		t.Fatalf("cleanup on empty: %v %v %v", c != nil, r != nil, err)
	}
}

func TestStageGeminiSessionCleanup_RollbackRestores(t *testing.T) {
	withGeminiTransferEnv(t)
	const agentID = "ag_gem_cleanup"
	p := plantGeminiAgentSession(t, agentID, "", gemTestMainSID, gemSessionBody(gemTestMainSID, "x"))
	dir, _ := filepath.Abs(AgentDir(agentID))
	thread := gemThreadRefName("t")
	writeGeminiRef(t, dir, thread, gemTestMainSID)

	_, rollback, err := StageGeminiSessionCleanup(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("session survived cleanup stage: %v", err)
	}
	if refs := listGeminiRefs(dir); len(refs) != 0 {
		t.Fatalf("refs survived cleanup stage: %+v", refs)
	}
	rollback()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("rollback lost session: %v", err)
	}
	if refs := listGeminiRefs(dir); len(refs) != 2 {
		t.Fatalf("rollback refs = %+v", refs)
	}
	assertNoGeminiStagingLeftovers(t, dir)
}

func TestGeminiProjectSlug(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "ag_Foo.Bar")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gdir := filepath.Join(home, ".gemini")
	if got := geminiProjectSlug(home, agentDir, false); got != "ag-foo-bar" {
		t.Fatalf("fresh slug = %q", got)
	}
	// Another path owns the base slug via a history marker → next free.
	if err := os.MkdirAll(filepath.Join(gdir, "history", "ag-foo-bar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, "history", "ag-foo-bar", ".project_root"), []byte("/other/ag_foo.bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := geminiProjectSlug(home, agentDir, false); got != "ag-foo-bar-1" {
		t.Fatalf("collision slug = %q", got)
	}
	// A projects.json mapping for this path wins.
	reg := `{"projects":{"` + strings.ReplaceAll(agentDir, `\`, `\\`) + `":"custom-slug"}}`
	if err := os.WriteFile(filepath.Join(gdir, "projects.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := geminiProjectSlug(home, agentDir, false); got != "custom-slug" {
		t.Fatalf("mapped slug = %q", got)
	}
	// A tmp marker owned by another path invalidates the mapping unless
	// tmp is excluded (it is about to be swapped out).
	if err := os.MkdirAll(filepath.Join(gdir, "tmp", "custom-slug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, "tmp", "custom-slug", ".project_root"), []byte("/someone/else"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := geminiProjectSlug(home, agentDir, false); got != "ag-foo-bar-1" {
		t.Fatalf("invalidated mapping slug = %q", got)
	}
	if got := geminiProjectSlug(home, agentDir, true); got != "custom-slug" {
		t.Fatalf("excludeTmp slug = %q", got)
	}
}

func TestGeminiSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"ag_9e90fb61fc4c9f5c": "ag-9e90fb61fc4c9f5c",
		"--A  b__C--":         "a-b-c",
		"日本語":                 "project",
		"":                    "project",
	} {
		if got := geminiSlugify(in); got != want {
			t.Errorf("geminiSlugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGeminiSessionTransfer_Live runs real gemini turns on a "source"
// config dir, ships the sessions through Read → Stage → commit into a
// "target" config dir (different absolute agent dir, same agent id), and
// checks that both the main and a SessionKey thread resume natively.
func TestGeminiSessionTransfer_Live(t *testing.T) {
	if os.Getenv("KOJO_GEMINI_LIVE") != "1" {
		t.Skip("set KOJO_GEMINI_LIVE=1 to run")
	}
	model := os.Getenv("KOJO_GEMINI_LIVE_MODEL")
	if model == "" {
		model = GeminiDefaultModel
	}
	src := t.TempDir()
	setHome := func(h string) {
		t.Setenv("HOME", h)
		t.Setenv("USERPROFILE", h)
		t.Setenv("XDG_CONFIG_HOME", h+"/.config")
	}
	setHome(src)
	b := NewGeminiBackend(silentLogger(), nil)
	a := &Agent{ID: "ag_gemini_xfer_live", Model: model, Effort: "low"}
	thread := ChatOptions{SessionKey: "slack:C9:42"}
	for _, turn := range []struct {
		msg  string
		opts ChatOptions
	}{
		{"Remember the word OTTER. Reply with just OK.", ChatOptions{}},
		{"Remember the word FALCON. Reply with just OK.", thread},
	} {
		evs := runGeminiTurn(t, b, a, turn.msg, turn.opts)
		if last := evs[len(evs)-1]; last.Type != "done" || last.ErrorMessage != "" {
			t.Fatalf("source turn = %+v", last)
		}
	}
	srcDir := agentDir(a.ID)
	mainSID := readGeminiSessionIDFor(srcDir, "")
	threadSID := readGeminiSessionIDFor(srcDir, thread.SessionKey)
	if mainSID == "" || threadSID == "" || mainSID == threadSID {
		t.Fatalf("source refs main=%q thread=%q", mainSID, threadSID)
	}
	tr, skipped, err := ReadGeminiSessionFiles(a.ID)
	if err != nil || tr == nil || len(skipped) != 0 {
		t.Fatalf("read: %+v %+v %v", tr, skipped, err)
	}

	dst := t.TempDir()
	setHome(dst)
	dstDir := agentDir(a.ID)
	if dstDir == srcDir {
		t.Fatal("target agent dir must differ from source")
	}
	commit, _, err := StageGeminiSession(a.ID, tr)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	for _, c := range []struct {
		opts ChatOptions
		sid  string
		word string
	}{{ChatOptions{}, mainSID, "OTTER"}, {thread, threadSID, "FALCON"}} {
		evs := runGeminiTurn(t, b, a, "What word did I ask you to remember? Reply with just the word.", c.opts)
		last := evs[len(evs)-1]
		if last.Message == nil || !strings.Contains(strings.ToUpper(last.Message.Content), c.word) {
			t.Fatalf("target resume (%q) = %+v", c.opts.SessionKey, last)
		}
		if got := readGeminiSessionIDFor(dstDir, c.opts.SessionKey); got != c.sid {
			t.Fatalf("target session id %q, want resumed %q", got, c.sid)
		}
		t.Logf("target resumed %s → %q", c.sid, last.Message.Content)
	}
}

// A symlinked .gemini (planted by the agent in its own dir) must not let
// a transfer read, stage into, or swap/remove anything outside the home.
func TestGeminiSessionTransfer_RefusesSymlinkedDirs(t *testing.T) {
	withGeminiTransferEnv(t)
	const agentID = "ag_gem_symlink"
	dir, _ := filepath.Abs(AgentDir(agentID))
	writeGeminiRef(t, dir, "", gemTestMainSID)

	outside := t.TempDir()
	victim := filepath.Join(outside, "tmp", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(geminiCLIHome(dir), ".gemini")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ReadGeminiSessionFiles(agentID); err == nil {
		t.Error("read: expected refusal")
	}
	if _, _, err := StageGeminiSessionCleanup(agentID); err == nil {
		t.Error("cleanup: expected refusal")
	}
	if _, _, err := StageGeminiSession(agentID, &GeminiSessionTransfer{
		Refs: []GeminiSessionRef{{SessionID: gemTestMainSID}},
		Files: []GeminiSessionFile{{
			RelPath: "session-2026-10-15T09-13-" + gemTestMainSID[:8] + ".jsonl",
			Content: []byte(gemSessionBody(gemTestMainSID, "x")),
		}},
	}); err == nil {
		t.Error("stage: expected refusal")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("outside file touched: %v", err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatalf("outside dir gained entries: %v", entries)
	}
}

// A symlinked chats dir is skipped on read (nothing from outside ships),
// and a symlinked .gemini never receives kojo's settings.json.
func TestGemini_SymlinkedChatsAndSettings(t *testing.T) {
	withGeminiTransferEnv(t)
	const agentID = "ag_gem_symchats"
	dir, _ := filepath.Abs(AgentDir(agentID))
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "session-2026-10-15T09-13-"+gemTestMainSID[:8]+".jsonl"),
		[]byte(gemSessionBody(gemTestMainSID, "secret")), 0o644); err != nil {
		t.Fatal(err)
	}
	slugDir := filepath.Join(geminiCLIHome(dir), ".gemini", "tmp", geminiSlugify(filepath.Base(dir)))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(slugDir, "chats")); err != nil {
		t.Fatal(err)
	}
	writeGeminiRef(t, dir, "", gemTestMainSID)
	tr, _, err := ReadGeminiSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil && len(tr.Files) != 0 {
		t.Fatalf("shipped files through a symlinked chats dir: %v", gemRelPaths(tr.Files))
	}

	home := filepath.Join(t.TempDir(), "h")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(home, ".gemini")); err != nil {
		t.Fatal(err)
	}
	if err := writeGeminiSettingsFile(filepath.Join(home, ".gemini", "settings.json"), "gemini-3.8-flash", "low", 0, nil); err == nil {
		t.Fatal("settings written through a symlinked .gemini")
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("victim dir gained %v", entries)
	}
}
