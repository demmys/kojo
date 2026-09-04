package agent

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	grokKeyA = "ag_x:slack:C123:1700000000.000100"
	grokKeyB = "ag_x:slack:C123:1700000000.000200"
	grokIDA  = "019e588f-419b-7202-9cff-1647e57116d5"
	grokIDB  = "019e58a0-2222-7202-9cff-1647e57116d5"
)

// Two SessionKeys must never share a resume ID: that is the whole point
// of keying, and it is what makes explicit --resume safe where
// --continue was not.
func TestGrokSessionRef_KeysAreIsolated(t *testing.T) {
	dir := t.TempDir()

	writeGrokSessionIDFor(dir, grokKeyA, grokIDA, silentLogger())
	writeGrokSessionIDFor(dir, grokKeyB, grokIDB, silentLogger())
	writeGrokSessionID(dir, "019e58b1-3333-7202-9cff-1647e57116d5", silentLogger())

	if got := readGrokSessionIDFor(dir, grokKeyA); got != grokIDA {
		t.Errorf("key A = %q, want %q", got, grokIDA)
	}
	if got := readGrokSessionIDFor(dir, grokKeyB); got != grokIDB {
		t.Errorf("key B = %q, want %q", got, grokIDB)
	}
	if got := readGrokSessionID(dir); got != "019e58b1-3333-7202-9cff-1647e57116d5" {
		t.Errorf("main = %q, want the main id", got)
	}
	if got := readGrokSessionIDFor(dir, "ag_x:slack:C123:unseen"); got != "" {
		t.Errorf("unseen key = %q, want \"\"", got)
	}
}

// The empty key is the agent's own session and must keep using the
// pre-existing path, so an agent that predates keyed threads resumes
// its session instead of silently starting over.
func TestGrokSessionRef_EmptyKeyIsLegacyPath(t *testing.T) {
	dir := t.TempDir()
	if got := grokSessionRefPath(dir, ""); got != grokSessionIDFile(dir) {
		t.Errorf("empty-key path = %q, want %q", got, grokSessionIDFile(dir))
	}
	if err := os.MkdirAll(filepath.Dir(grokSessionIDFile(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grokSessionIDFile(dir), []byte(grokIDA), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGrokSessionIDFor(dir, ""); got != grokIDA {
		t.Errorf("legacy read = %q, want %q", got, grokIDA)
	}
}

// A SessionKey is caller supplied (Slack channel + thread ts today), so
// it must not be able to steer the ref file out of the threads dir.
func TestGrokSessionRef_HostileKeyStaysInside(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"../../etc/passwd", "a/b", "..", "  "} {
		got := grokSessionRefPath(dir, key)
		rel, err := filepath.Rel(grokThreadRefDir(dir), got)
		if err != nil || filepath.Dir(rel) != "." {
			t.Errorf("key %q → %q, escapes %q", key, got, grokThreadRefDir(dir))
		}
	}
}

// A poisoned keyed ref is rejected AND deleted, same as the primary one.
func TestGrokSessionRef_PoisonedKeyedValueRemoved(t *testing.T) {
	dir := t.TempDir()
	path := grokSessionRefPath(dir, grokKeyA)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("--cwd=/etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGrokSessionIDFor(dir, grokKeyA); got != "" {
		t.Errorf("poisoned read = %q, want \"\"", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("poisoned ref still on disk: err=%v", err)
	}
}

// The OneShot cleanup path deletes a session directory only when no key
// still resumes it, so a thread's session survives a concurrent
// disposable turn.
func TestGrokSessionIDPersisted(t *testing.T) {
	dir := t.TempDir()
	if grokSessionIDPersisted(dir, grokIDA) {
		t.Error("empty store reported the id as persisted")
	}
	if grokSessionIDPersisted(dir, "") {
		t.Error("empty id reported as persisted")
	}
	writeGrokSessionIDFor(dir, grokKeyA, grokIDA, silentLogger())
	if !grokSessionIDPersisted(dir, grokIDA) {
		t.Error("keyed id not reported as persisted")
	}
	if grokSessionIDPersisted(dir, grokIDB) {
		t.Error("unrelated id reported as persisted")
	}
	writeGrokSessionID(dir, grokIDB, silentLogger())
	if !grokSessionIDPersisted(dir, grokIDB) {
		t.Error("primary id not reported as persisted")
	}
}

func TestGrokCanResumeSession(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_resume_probe"
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if grokCanResumeSession(agentID, "") {
		t.Error("empty key must never be resumable")
	}
	if grokCanResumeSession(agentID, grokKeyA) {
		t.Error("resumable with no stored ref")
	}

	writeGrokSessionIDFor(dir, grokKeyA, grokIDA, silentLogger())
	// A ref whose session grok has since GC'd is not resumable — the
	// caller has to re-inject the thread history instead.
	if grokCanResumeSession(agentID, grokKeyA) {
		t.Error("resumable with a ref but no session on disk")
	}

	if err := os.MkdirAll(filepath.Join(grokSessionDir(dir), grokIDA), 0o755); err != nil {
		t.Fatal(err)
	}
	if !grokCanResumeSession(agentID, grokKeyA) {
		t.Error("not resumable with ref + session on disk")
	}
	if grokCanResumeSession(agentID, grokKeyB) {
		t.Error("another key became resumable")
	}
}

// A reset wipes the sessions, so the keyed refs pointing at them must go
// too — otherwise every thread's next turn burns a failed resume.
func TestClearGrokSessionCounted_RemovesThreadRefs(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_clear_threads"
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGrokSessionIDFor(dir, grokKeyA, grokIDA, silentLogger())

	if _, _, err := clearGrokSessionCounted(agentID); err != nil {
		t.Fatalf("clearGrokSessionCounted: %v", err)
	}
	if _, err := os.Stat(grokThreadRefDir(dir)); !os.IsNotExist(err) {
		t.Errorf("thread refs still present: err=%v", err)
	}
}

// A ref pointing at a session grok no longer has must be dropped before
// the turn, not after a failed resume burns it.
func TestGrokSessionExists(t *testing.T) {
	withGrokHome(t)
	dir := t.TempDir()

	if grokSessionExists(dir, grokIDA) {
		t.Error("reported an absent session as present")
	}
	if grokSessionExists(dir, "not-a-uuid") {
		t.Error("reported a malformed id as present")
	}
	if err := os.MkdirAll(filepath.Join(grokSessionDir(dir), grokIDA), 0o755); err != nil {
		t.Fatal(err)
	}
	if !grokSessionExists(dir, grokIDA) {
		t.Error("reported a present session as absent")
	}
}
