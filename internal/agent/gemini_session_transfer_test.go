package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	gemTestMainSID   = "ses_eb96caae1ffeNQtLNqB50oU8p4"
	gemTestThreadSID = "ses_eb9681278ffeTz386G0jH8fwgf"
	gemTestStaleSID  = "ses_eb96f397fffeYN24NtXaaOmKIU"
)

func withGeminiTransferEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
}

func gemExportBody(sid, text string) string {
	return `{"info":{"id":"` + sid + `","title":"t","directory":"/src/agent"},"messages":[{"info":{"role":"user"},"parts":[{"type":"text","text":"` + text + `"}]}]}`
}

func gemThreadRefName(key string) string { return "key-" + agentIDToUUID(key) }

// stubOpenCodeExport replaces `opencode export` with bodies; a missing id
// fails like a broken export. It counts calls and peak concurrency.
func stubOpenCodeExport(t *testing.T, bodies map[string]string) (calls, peak *int32) {
	t.Helper()
	var mu sync.Mutex
	var n, cur, max int32
	orig := exportOpenCodeSessionFn
	exportOpenCodeSessionFn = func(_ context.Context, _ string, sid string) ([]byte, error) {
		atomic.AddInt32(&n, 1)
		mu.Lock()
		cur++
		if cur > max {
			max = cur
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		b, ok := bodies[sid]
		if !ok {
			return nil, errors.New("export failed")
		}
		return []byte(b), nil
	}
	t.Cleanup(func() { exportOpenCodeSessionFn = orig })
	return &n, &max
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
	var out []string
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	sort.Strings(out)
	return out
}

func absAgentDirFor(t *testing.T, id string) string {
	t.Helper()
	dir, err := filepath.Abs(AgentDir(id))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadGeminiSessionFiles_RefsStaleAndShared(t *testing.T) {
	withGeminiTransferEnv(t)
	const id = "ag_gem_read"
	dir := absAgentDirFor(t, id)
	threadA, threadB, threadStale := gemThreadRefName("a"), gemThreadRefName("b"), gemThreadRefName("s")
	writeGeminiRef(t, dir, "", gemTestMainSID)
	writeGeminiRef(t, dir, threadA, gemTestThreadSID)
	writeGeminiRef(t, dir, threadB, gemTestMainSID) // shares the main session
	writeGeminiRef(t, dir, threadStale, gemTestStaleSID)
	writeGeminiRef(t, dir, "not-a-ref", gemTestMainSID)
	// The DB knows main and thread; the stale ref's session is gone.
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{gemTestMainSID, dir, "x"}, [3]string{gemTestThreadSID, dir, "y"})
	calls, _ := stubOpenCodeExport(t, map[string]string{
		gemTestMainSID:   gemExportBody(gemTestMainSID, "main"),
		gemTestThreadSID: gemExportBody(gemTestThreadSID, "thread"),
	})

	tr, skipped, err := ReadGeminiSessionFiles(id)
	if err != nil || tr == nil {
		t.Fatalf("read: %+v %v", tr, err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if got := gemRelPaths(tr.Files); strings.Join(got, ",") != strings.Join([]string{gemTestThreadSID + ".json", gemTestMainSID + ".json"}, ",") &&
		strings.Join(got, ",") != strings.Join([]string{gemTestMainSID + ".json", gemTestThreadSID + ".json"}, ",") {
		t.Fatalf("files = %v", got)
	}
	if len(tr.Refs) != 3 {
		t.Fatalf("refs = %+v (stale and malformed names must be dropped)", tr.Refs)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Fatalf("export called %d times (shared session must be exported once)", *calls)
	}
}

func TestReadGeminiSessionFiles_NoRefs(t *testing.T) {
	withGeminiTransferEnv(t)
	tr, skipped, err := ReadGeminiSessionFiles("ag_gem_none")
	if tr != nil || skipped != nil || err != nil {
		t.Fatalf("got %+v %+v %v", tr, skipped, err)
	}
}

func TestReadGeminiSessionFiles_SkipsOversizedUnreadableAndOverBudget(t *testing.T) {
	withGeminiTransferEnv(t)
	const id = "ag_gem_caps"
	dir := absAgentDirFor(t, id)
	ids := []string{
		"ses_eb9600000001AAAAAAAAAAAAA1", "ses_eb9600000002AAAAAAAAAAAAA2",
		"ses_eb9600000003AAAAAAAAAAAAA3", "ses_eb9600000004AAAAAAAAAAAAA4",
		"ses_eb9600000005AAAAAAAAAAAAA5", "ses_eb9600000006AAAAAAAAAAAAA6",
	}
	bodies := map[string]string{}
	for i, sid := range ids {
		writeGeminiRef(t, dir, gemThreadRefName(string(rune('a'+i))), sid)
		bodies[sid] = gemExportBody(sid, strings.Repeat("x", 100))
	}
	big := ids[0]
	bodies[big] = gemExportBody(big, strings.Repeat("y", 4000))
	delete(bodies, ids[1]) // unreadable
	origFile, origTotal := geminiSessionFileMaxBytes, geminiSessionTransferMaxTotalBytes
	geminiSessionFileMaxBytes, geminiSessionTransferMaxTotalBytes = 1000, 500
	t.Cleanup(func() { geminiSessionFileMaxBytes, geminiSessionTransferMaxTotalBytes = origFile, origTotal })
	var rows [][3]string
	for _, sid := range ids {
		rows = append(rows, [3]string{sid, dir, "x"})
	}
	plantOpenCodeDB(t, openCodeHome(dir), rows...)
	_, peak := stubOpenCodeExport(t, bodies)

	tr, skipped, err := ReadGeminiSessionFiles(id)
	if err != nil {
		t.Fatal(err)
	}
	// Ref order follows the hashed thread names, so which sessions fall
	// past the budget varies; the oversized and the unreadable one are
	// always skipped (the latter as "capacity" when its batch was never
	// exported).
	reasons := map[string]int{}
	byID := map[string]string{}
	for _, s := range skipped {
		reasons[s.Reason]++
		byID[strings.TrimPrefix(s.Path, "gemini-session/")] = s.Reason
	}
	if byID[big] != "oversized" || (byID[ids[1]] != "unreadable" && byID[ids[1]] != "capacity") || reasons["capacity"] == 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	var total int
	for _, f := range tr.Files {
		total += len(f.Content)
	}
	if total > 500 || len(tr.Files) == 0 {
		t.Fatalf("shipped %d files / %d bytes", len(tr.Files), total)
	}
	if p := atomic.LoadInt32(peak); p > geminiExportParallelism {
		t.Fatalf("peak concurrent exports = %d", p)
	}
}

func TestReadGeminiSessionFiles_ShipsPendingImport(t *testing.T) {
	withGeminiTransferEnv(t)
	const id = "ag_gem_pending"
	dir := absAgentDirFor(t, id)
	writeGeminiRef(t, dir, "", gemTestMainSID)
	pending := geminiPendingImportDir(dir)
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	body := gemExportBody(gemTestMainSID, "from elsewhere")
	if err := os.WriteFile(filepath.Join(pending, gemTestMainSID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	calls, _ := stubOpenCodeExport(t, nil)
	tr, _, err := ReadGeminiSessionFiles(id)
	if err != nil || tr == nil || len(tr.Files) != 1 || string(tr.Files[0].Content) != body {
		t.Fatalf("read: %+v %v", tr, err)
	}
	if *calls != 0 {
		t.Fatal("exported a session still pending import")
	}
}

func TestStageGeminiSession_RoundTripAndRollback(t *testing.T) {
	withGeminiTransferEnv(t)
	const srcID = "ag_gem_src"
	srcDir := absAgentDirFor(t, srcID)
	thread := gemThreadRefName("k")
	writeGeminiRef(t, srcDir, "", gemTestMainSID)
	writeGeminiRef(t, srcDir, thread, gemTestThreadSID)
	plantOpenCodeDB(t, openCodeHome(srcDir), [3]string{gemTestMainSID, srcDir, "x"}, [3]string{gemTestThreadSID, srcDir, "y"})
	stubOpenCodeExport(t, map[string]string{
		gemTestMainSID:   gemExportBody(gemTestMainSID, "main"),
		gemTestThreadSID: gemExportBody(gemTestThreadSID, "thread"),
	})
	tr, _, err := ReadGeminiSessionFiles(srcID)
	if err != nil || tr == nil {
		t.Fatalf("read: %+v %v", tr, err)
	}

	// Target with divergent state: an old main ref, an old thread ref not
	// in the transfer, and an older pending import.
	const dstID = "ag_gem_dst"
	dstDir := absAgentDirFor(t, dstID)
	oldThread := gemThreadRefName("old")
	writeGeminiRef(t, dstDir, "", gemTestStaleSID)
	writeGeminiRef(t, dstDir, oldThread, gemTestStaleSID)
	oldPending := filepath.Join(geminiPendingImportDir(dstDir), gemTestStaleSID+".json")
	if err := os.MkdirAll(filepath.Dir(oldPending), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPending, []byte(gemExportBody(gemTestStaleSID, "old")), 0o600); err != nil {
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
	for _, sid := range []string{gemTestMainSID, gemTestThreadSID} {
		if !hasPendingOpenCodeImport(dstDir, sid) {
			t.Fatalf("%s not staged for import", sid)
		}
	}
	if hasPendingOpenCodeImport(dstDir, gemTestStaleSID) {
		t.Fatal("old pending import survived")
	}
	// A staged session counts as resumable before it is imported.
	if !geminiCanResumeSession(dstID, "k") {
		t.Fatal("staged thread not resumable")
	}

	rollback()
	commit() // no-op after rollback
	if got := readGeminiRefRaw(geminiSessionIDFile(dstDir)); got != gemTestStaleSID {
		t.Fatalf("rollback main ref = %q", got)
	}
	if _, err := os.Stat(oldPending); err != nil {
		t.Fatalf("rollback lost old pending import: %v", err)
	}
	if got := readGeminiRefRaw(filepath.Join(geminiThreadRefDir(dstDir), oldThread)); got != gemTestStaleSID {
		t.Fatalf("rollback old thread ref = %q", got)
	}
	if _, err := os.Stat(filepath.Join(geminiThreadRefDir(dstDir), thread)); !os.IsNotExist(err) {
		t.Fatalf("rollback left new thread ref: %v", err)
	}
	assertNoGeminiStagingLeftovers(t, dstDir)

	// Stage again and commit: backups dropped, lock released.
	commit, _, err = StageGeminiSession(dstID, tr)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	assertNoGeminiStagingLeftovers(t, dstDir)
	if !hasPendingOpenCodeImport(dstDir, gemTestThreadSID) {
		t.Fatal("thread export missing after commit")
	}
	c2, _, err := StageGeminiSessionCleanup(dstID)
	if err != nil {
		t.Fatal(err)
	}
	c2()
	if refs := listGeminiRefs(dstDir); len(refs) != 0 {
		t.Fatalf("cleanup left refs: %+v", refs)
	}
	if hasPendingOpenCodeImport(dstDir, gemTestMainSID) {
		t.Fatal("cleanup left pending imports")
	}
	assertNoGeminiStagingLeftovers(t, dstDir)
}

func assertNoGeminiStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(geminiKojoDir(dir))
	for _, e := range entries {
		if strings.Contains(e.Name(), "kojo-bk") || strings.Contains(e.Name(), "staging") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover %s in %s", e.Name(), geminiKojoDir(dir))
		}
	}
}

func TestStageGeminiSession_Rejects(t *testing.T) {
	withGeminiTransferEnv(t)
	good := GeminiSessionFile{RelPath: gemTestMainSID + ".json", Content: []byte(gemExportBody(gemTestMainSID, "x"))}
	ref := GeminiSessionRef{SessionID: gemTestMainSID}
	cases := map[string]*GeminiSessionTransfer{
		"traversal relpath":  {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: "../" + gemTestMainSID + ".json", Content: good.Content}}},
		"nested relpath":     {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: "a/" + gemTestMainSID + ".json", Content: good.Content}}},
		"legacy jsonl":       {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: "session-x.jsonl", Content: good.Content}}},
		"duplicate file":     {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{good, good}},
		"mismatched export":  {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: good.RelPath, Content: []byte(gemExportBody(gemTestThreadSID, "x"))}}},
		"not json":           {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: good.RelPath, Content: []byte("nope")}}},
		"no messages":        {Refs: []GeminiSessionRef{ref}, Files: []GeminiSessionFile{{RelPath: good.RelPath, Content: []byte(`{"info":{"id":"` + gemTestMainSID + `"}}`)}}},
		"bad ref name":       {Refs: []GeminiSessionRef{{Name: "../x", SessionID: gemTestMainSID}}, Files: []GeminiSessionFile{good}},
		"bad ref id":         {Refs: []GeminiSessionRef{{SessionID: "-s"}}, Files: []GeminiSessionFile{good}},
		"duplicate ref":      {Refs: []GeminiSessionRef{ref, ref}, Files: []GeminiSessionFile{good}},
		"ref without export": {Refs: []GeminiSessionRef{{SessionID: gemTestThreadSID}}, Files: []GeminiSessionFile{good}},
	}
	for name, tr := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := StageGeminiSession("ag_gem_rej", tr); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	dir := absAgentDirFor(t, "ag_gem_rej")
	if refs := listGeminiRefs(dir); len(refs) != 0 {
		t.Fatalf("a rejected stage left refs: %+v", refs)
	}
	assertNoGeminiStagingLeftovers(t, dir)
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
	dir := absAgentDirFor(t, agentID)
	writeGeminiRef(t, dir, "", gemTestMainSID)
	writeGeminiRef(t, dir, gemThreadRefName("t"), gemTestMainSID)
	_, rollback, err := StageGeminiSessionCleanup(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if refs := listGeminiRefs(dir); len(refs) != 0 {
		t.Fatalf("refs survived cleanup stage: %+v", refs)
	}
	rollback()
	if refs := listGeminiRefs(dir); len(refs) != 2 {
		t.Fatalf("rollback refs = %+v", refs)
	}
	assertNoGeminiStagingLeftovers(t, dir)
}

func TestGeminiSessionTransfer_RefusesSymlinkedDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	withGeminiTransferEnv(t)
	const agentID = "ag_gem_symlink"
	dir := absAgentDirFor(t, agentID)
	if err := os.MkdirAll(openCodeHome(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "session_id")
	if err := os.WriteFile(victim, []byte(gemTestStaleSID), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, geminiKojoDir(dir)); err != nil {
		t.Fatal(err)
	}
	stubOpenCodeExport(t, map[string]string{gemTestStaleSID: gemExportBody(gemTestStaleSID, "secret")})

	if _, _, err := ReadGeminiSessionFiles(agentID); err == nil {
		t.Error("read: expected refusal")
	}
	if _, _, err := StageGeminiSessionCleanup(agentID); err == nil {
		t.Error("cleanup: expected refusal")
	}
	if _, _, err := StageGeminiSession(agentID, &GeminiSessionTransfer{
		Refs:  []GeminiSessionRef{{SessionID: gemTestMainSID}},
		Files: []GeminiSessionFile{{RelPath: gemTestMainSID + ".json", Content: []byte(gemExportBody(gemTestMainSID, "x"))}},
	}); err == nil {
		t.Error("stage: expected refusal")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != gemTestStaleSID {
		t.Fatalf("outside file touched: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside dir gained entries: %v", entries)
	}
}

// importPendingOpenCodeSessions runs `opencode import` for each staged
// export, deleting the local copy first, and removes the imported files.
func TestImportPendingOpenCodeSessions(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("the fake's session delete needs the sqlite3 CLI")
	}
	const id = "ag_gem_import"
	dir := agentDir(id)
	pending := geminiPendingImportDir(dir)
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{gemTestMainSID, gemTestThreadSID} {
		if err := os.WriteFile(filepath.Join(pending, sid+".json"), []byte(gemExportBody(sid, "x")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pending, "junk.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{gemTestMainSID, dir, "old copy"})
	importPendingOpenCodeSessions(context.Background(), id, silentLogger())
	log, _ := os.ReadFile(logPath)
	s := string(log)
	if !strings.Contains(s, "AUX session delete "+gemTestMainSID) || strings.Contains(s, "AUX session delete "+gemTestThreadSID) {
		t.Fatalf("delete-before-import:\n%s", s)
	}
	for _, sid := range []string{gemTestMainSID, gemTestThreadSID} {
		if !strings.Contains(s, "AUX import "+filepath.Join(pending, sid+".json")) {
			t.Fatalf("no import of %s:\n%s", sid, s)
		}
		if hasPendingOpenCodeImport(dir, sid) {
			t.Fatalf("%s still pending", sid)
		}
	}
	if strings.Contains(s, "junk") {
		t.Fatal("imported a file that is not a session export")
	}
}

// When the local copy of a transferred session cannot be dropped, the
// import is postponed (export kept, attempt counted) instead of merging
// the export into the stale copy; other exports still import.
func TestImportPendingOpenCodeSessions_PostponesWhenDeleteFails(t *testing.T) {
	logPath := setupFakeOpenCode(t)
	t.Setenv("FAKE_OC_NODELETE", "1")
	const id = "ag_gem_import_nodel"
	dir := agentDir(id)
	pending := geminiPendingImportDir(dir)
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{gemTestMainSID, gemTestThreadSID} {
		if err := os.WriteFile(filepath.Join(pending, sid+".json"), []byte(gemExportBody(sid, "x")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plantOpenCodeDB(t, openCodeHome(dir), [3]string{gemTestMainSID, dir, "old copy"})
	importPendingOpenCodeSessions(context.Background(), id, silentLogger())
	log, _ := os.ReadFile(logPath)
	s := string(log)
	if strings.Contains(s, "AUX import "+filepath.Join(pending, gemTestMainSID+".json")) {
		t.Fatalf("imported over a copy that could not be dropped:\n%s", s)
	}
	if !hasPendingOpenCodeImport(dir, gemTestMainSID) {
		t.Fatal("postponed export was not kept")
	}
	if _, err := os.Stat(filepath.Join(pending, gemTestMainSID+".json"+openCodeImportAttemptsSuffix)); err != nil {
		t.Fatalf("attempt not counted: %v", err)
	}
	if hasPendingOpenCodeImport(dir, gemTestThreadSID) || !strings.Contains(s, "AUX import "+filepath.Join(pending, gemTestThreadSID+".json")) {
		t.Fatalf("independent export not imported:\n%s", s)
	}
}

// A pending export is the newest copy of its session: when it cannot be
// used, the session is not shipped from the older local DB instead.
func TestGeminiSessionExport_BadPendingDoesNotFallBack(t *testing.T) {
	withGeminiTransferEnv(t)
	calls, _ := stubOpenCodeExport(t, map[string]string{gemTestMainSID: gemExportBody(gemTestMainSID, "stale db copy")})
	dir := agentDir("ag_gem_badpending")
	pending := filepath.Join(geminiPendingImportDir(dir), gemTestMainSID+".json")
	if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"not json", gemExportBody(gemTestThreadSID, "other session")} {
		if err := os.WriteFile(pending, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := geminiSessionExport("ag_gem_badpending", dir, gemTestMainSID)
		if err == nil || got != nil {
			t.Fatalf("pending %q: got %q, err %v; want an error", body, got, err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(pending, 0); err != nil {
			t.Fatal(err)
		}
		if _, rerr := os.ReadFile(pending); rerr != nil { // not root
			if got, err := geminiSessionExport("ag_gem_badpending", dir, gemTestMainSID); err == nil {
				t.Fatalf("unreadable pending: got %q, want an error", got)
			}
		}
		_ = os.Chmod(pending, 0o600)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("fell back to the DB export %d times", n)
	}
	// A valid pending export is shipped as is.
	want := gemExportBody(gemTestMainSID, "newest")
	if err := os.WriteFile(pending, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := geminiSessionExport("ag_gem_badpending", dir, gemTestMainSID); err != nil || string(got) != want {
		t.Fatalf("valid pending: got %q, err %v", got, err)
	}
}
