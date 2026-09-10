package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupCodexTransferTest(t *testing.T) (agentID, codexRoot string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	codexRoot = filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexRoot)
	agentID = "ag_codex_transfer"
	return agentID, codexRoot
}

func TestReadCodexSessionFiles_ReadsRefAndRollout(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	threadID := "019e7cc9-dd5e-7971-b654-7840c683879e"
	rel := filepath.Join("sessions", "2026", "05", "31",
		"rollout-2026-05-31T00-00-00-"+threadID+".jsonl")
	rolloutPath := filepath.Join(codexRoot, rel)
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatalf("mkdir rollout parent: %v", err)
	}
	if err := os.WriteFile(rolloutPath, []byte(`{"type":"session_meta"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeCodexThreadRef(agentID, "", codexThreadRef{ThreadID: threadID, RolloutPath: rolloutPath}, logger)

	got, skipped, err := ReadCodexSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadCodexSessionFiles: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if got == nil || len(got.Threads) != 1 {
		t.Fatalf("threads len = %v, want 1", got)
	}
	th := got.Threads[0]
	if th.ThreadID != threadID || th.RefName != "main.json" {
		t.Fatalf("thread metadata = %#v", th)
	}
	if th.RolloutRelPath != filepath.ToSlash(rel) {
		t.Fatalf("relpath = %q, want %q", th.RolloutRelPath, filepath.ToSlash(rel))
	}
	if string(th.RolloutContent) == "" {
		t.Fatalf("rollout content empty")
	}
}

func TestReadCodexSessionFiles_NewestRefFirst(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	type fixture struct {
		key, threadID string
	}
	fixtures := []fixture{
		{"", "019e7cc9-dd5e-7971-b654-7840c683879e"},
		{"slack:test", "019e7cc9-dd5e-7971-b654-7840c683879f"},
	}
	for i, f := range fixtures {
		rel := filepath.Join("sessions", "2026", "05", "31", "rollout-"+f.threadID+".jsonl")
		path := filepath.Join(codexRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(f.threadID), 0o644); err != nil {
			t.Fatal(err)
		}
		writeCodexThreadRef(agentID, f.key, codexThreadRef{ThreadID: f.threadID, RolloutPath: path}, logger)
		refPath := codexThreadRefPath(agentID, f.key)
		mtime := time.Now().Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(refPath, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	got, skipped, err := ReadCodexSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || got == nil || len(got.Threads) != 2 {
		t.Fatalf("got=%+v skipped=%+v", got, skipped)
	}
	if got.Threads[0].ThreadID != fixtures[1].threadID || got.Threads[1].ThreadID != fixtures[0].threadID {
		t.Fatalf("thread order = %s, %s; want newest ref first", got.Threads[0].ThreadID, got.Threads[1].ThreadID)
	}
}

func TestReadCodexSessionFiles_RefCountKeepsNewestAndSkipsOlder(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	baseTime := time.Now().Add(-time.Hour)
	threadIDs := make([]string, codexSessionTransferMaxThreads+1)
	for i := range threadIDs {
		threadID := fmt.Sprintf("019e7cc9-dd5e-7971-b654-%012x", i+1)
		threadIDs[i] = threadID
		rel := filepath.Join("sessions", "2026", "05", "31", "rollout-"+threadID+".jsonl")
		rolloutPath := filepath.Join(codexRoot, rel)
		if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rolloutPath, []byte(threadID), 0o644); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("slack:test:%03d", i)
		writeCodexThreadRef(agentID, key, codexThreadRef{ThreadID: threadID, RolloutPath: rolloutPath}, logger)
		refPath := codexThreadRefPath(agentID, key)
		mtime := baseTime.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(refPath, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	got, skipped, err := ReadCodexSessionFiles(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Threads) != codexSessionTransferMaxThreads {
		t.Fatalf("threads=%v, want newest %d", got, codexSessionTransferMaxThreads)
	}
	if got.Threads[0].ThreadID != threadIDs[len(threadIDs)-1] ||
		got.Threads[len(got.Threads)-1].ThreadID != threadIDs[1] {
		t.Fatalf("kept range = %s ... %s; want newest through second-oldest",
			got.Threads[0].ThreadID, got.Threads[len(got.Threads)-1].ThreadID)
	}
	if len(skipped) != 1 || skipped[0].Reason != "capacity" {
		t.Fatalf("skipped=%+v, want one capacity skip", skipped)
	}
}

func TestStageCodexSession_RollbackAndCommit(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	threadID := "019e7cc9-dd5e-7971-b654-7840c683879e"
	rel := filepath.ToSlash(filepath.Join("sessions", "2026", "05", "31",
		"rollout-2026-05-31T00-00-00-"+threadID+".jsonl"))
	transfer := &CodexSessionTransfer{Threads: []CodexThreadTransfer{{
		RefName:        "main.json",
		ThreadID:       threadID,
		RolloutRelPath: rel,
		RolloutContent: []byte(`{"type":"session_meta"}` + "\n"),
	}}}

	commit, rollback, err := StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageCodexSession: %v", err)
	}
	rolloutPath := filepath.Join(codexRoot, filepath.FromSlash(rel))
	refPath := codexThreadRefPath(agentID, "")
	if _, err := os.Stat(rolloutPath); err != nil {
		t.Fatalf("rollout not staged: %v", err)
	}
	if _, err := os.Stat(refPath); err != nil {
		t.Fatalf("ref not staged: %v", err)
	}
	rollback()
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("rollout survived rollback: %v", err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Fatalf("ref survived rollback: %v", err)
	}
	_ = commit

	commit, rollback, err = StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageCodexSession second: %v", err)
	}
	commit()
	_ = rollback
	if _, err := os.Stat(rolloutPath); err != nil {
		t.Fatalf("rollout missing after commit: %v", err)
	}
	ref, err := readCodexThreadRef(agentID, "")
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	if ref.ThreadID != threadID || ref.RolloutPath != rolloutPath {
		t.Fatalf("ref = %#v, want thread %s path %s", ref, threadID, rolloutPath)
	}

	files, threads, err := clearCodexSessionCounted(agentID)
	if err != nil {
		t.Fatalf("clearCodexSessionCounted: %v", err)
	}
	if files != 1 || threads != 1 {
		t.Fatalf("clear counts = files %d threads %d, want 1/1", files, threads)
	}
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("rollout survived clear: %v", err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Fatalf("ref survived clear: %v", err)
	}
}

func TestStageCodexSession_RemovesOmittedRefsAndRollsBack(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	staleThread := "019e7cc9-dd5e-7971-b654-7840c683879d"
	staleRollout := filepath.Join(codexRoot, "sessions", "stale-"+staleThread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(staleRollout), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleRollout, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCodexThreadRef(agentID, "slack:stale", codexThreadRef{
		ThreadID: staleThread, RolloutPath: staleRollout,
	}, logger)
	staleRef := codexThreadRefPath(agentID, "slack:stale")

	newThread := "019e7cc9-dd5e-7971-b654-7840c683879e"
	newRel := filepath.ToSlash(filepath.Join("sessions", "new-"+newThread+".jsonl"))
	transfer := &CodexSessionTransfer{Threads: []CodexThreadTransfer{{
		RefName: "main.json", ThreadID: newThread, RolloutRelPath: newRel,
		RolloutContent: []byte("new"),
	}}}
	commit, rollback, err := StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleRef); !os.IsNotExist(err) {
		t.Fatalf("omitted ref remains staged: %v", err)
	}
	rollback()
	if _, err := os.Stat(staleRef); err != nil {
		t.Fatalf("omitted ref not restored by rollback: %v", err)
	}
	_ = commit

	commit, rollback, err = StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	_ = rollback
	if _, err := os.Stat(staleRef); !os.IsNotExist(err) {
		t.Fatalf("omitted ref survived commit: %v", err)
	}
}
