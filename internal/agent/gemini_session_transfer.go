package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// gemini (OpenCode) session transfer for §3.7 device switch.
//
// OpenCode keeps every session of the agent in one SQLite DB inside the
// agent's own OpenCode home (<agentDir>/.opencode-home/data/opencode/
// opencode.db). kojo's resume refs live next to it under kojo/
// (session_id for the main conversation, threads/key-<uuid> per
// SessionKey).
//
// The DB itself is not shipped: it holds every session (OneShot ones
// included) bound to source's absolute paths and is written concurrently
// by running OpenCode processes. A handoff instead carries, for every kojo
// ref, OpenCode's own export of that session (`opencode export <id>`:
// session info plus all messages and parts as JSON). Target stages the
// exports under kojo/pending-import/<id>.json and swaps in the shipped
// refs; the next chat turn or PTY launch imports them with
// `opencode import` (which re-homes the session to target's agent dir;
// verified against opencode 1.19.3: `-s <id>` resumes an imported session
// from a different absolute path) before resolving the ref. OneShot
// sessions (no ref) stay behind, as for grok.

// GeminiSessionRef is one kojo resume ref. Name is "" for the main
// conversation (kojo/session_id) or the threads/ file name
// ("key-<uuid>") for a SessionKey thread.
type GeminiSessionRef struct {
	Name      string
	SessionID string
}

// GeminiSessionFile is one OpenCode session export. RelPath is
// "<sessionID>.json"; Content is the `opencode export` JSON.
type GeminiSessionFile struct {
	RelPath string
	Content []byte
}

// GeminiSessionTransfer is the complete gemini handoff payload.
type GeminiSessionTransfer struct {
	Refs  []GeminiSessionRef
	Files []GeminiSessionFile
}

var (
	geminiSessionFileMaxBytes          int64 = 128 << 20
	geminiSessionTransferMaxTotalBytes int64 = 256 << 20
)

const geminiSessionTransferMaxFiles = 1024

var (
	geminiThreadRefNamePattern = regexp.MustCompile(`^key-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	geminiExportFilePattern    = regexp.MustCompile(`^(ses_[0-9a-f]{12}[0-9A-Za-z]{14})\.json$`)
)

var geminiSessionTransferLocks keyedMutex

// exportOpenCodeSessionFn is swapped by tests (no opencode binary).
var exportOpenCodeSessionFn = exportOpenCodeSession

// geminiRefuseSymlinks errors when any existing path in paths is a
// symlink or not a directory; missing paths are fine. The agent can
// write its own dir, and a planted link inside its OpenCode home would
// otherwise redirect kojo's reads, writes and swaps (rename aside +
// RemoveAll on commit) outside it.
func geminiRefuseSymlinks(paths ...string) error {
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a real directory", p)
		}
	}
	return nil
}

// geminiCheckRealDirs applies geminiRefuseSymlinks to the fixed
// OpenCode/kojo state directories under the agent dir and refuses any
// symlink inside the OpenCode home (refuseOpenCodeHomeLinks).
func geminiCheckRealDirs(absAgentDir string) error {
	home := openCodeHome(absAgentDir)
	if err := geminiRefuseSymlinks(
		home,
		filepath.Join(home, "data"),
		openCodeDataDir(home),
		geminiKojoDir(absAgentDir),
		geminiThreadRefDir(absAgentDir),
		geminiPendingImportDir(absAgentDir),
	); err != nil {
		return err
	}
	// A file link deeper down (opencode.db-wal, a session file) would be
	// followed by the DB reads and the export/import the transfer runs.
	return refuseOpenCodeHomeLinks(home)
}

func lockGeminiSessionTransfer(agentID string) func() {
	return geminiSessionTransferLocks.Lock(agentID)
}

// readGeminiRefRaw reads a ref file without the self-healing delete
// readGeminiSessionIDFor performs (a transfer read must not mutate source).
func readGeminiRefRaw(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isGeminiSessionID(id) {
		return ""
	}
	return id
}

// listGeminiRefs returns every valid kojo ref, main first, threads sorted.
func listGeminiRefs(absAgentDir string) []GeminiSessionRef {
	var refs []GeminiSessionRef
	if id := readGeminiRefRaw(geminiSessionIDFile(absAgentDir)); id != "" {
		refs = append(refs, GeminiSessionRef{SessionID: id})
	}
	entries, err := os.ReadDir(geminiThreadRefDir(absAgentDir))
	if err != nil {
		return refs
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() && geminiThreadRefNamePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if id := readGeminiRefRaw(filepath.Join(geminiThreadRefDir(absAgentDir), n)); id != "" {
			refs = append(refs, GeminiSessionRef{Name: n, SessionID: id})
		}
	}
	return refs
}

// validateOpenCodeExport checks that body is an OpenCode session export
// of sessionID.
func validateOpenCodeExport(body []byte, sessionID string) error {
	var exp struct {
		Info *struct {
			ID string `json:"id"`
		} `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &exp); err != nil {
		return fmt.Errorf("opencode export of %s is not JSON: %w", sessionID, err)
	}
	if exp.Info == nil || exp.Info.ID != sessionID {
		return fmt.Errorf("opencode export does not describe session %s", sessionID)
	}
	if exp.Messages == nil {
		return fmt.Errorf("opencode export of %s has no messages", sessionID)
	}
	return nil
}

// geminiSessionExport returns the export for sessionID: a still-pending
// import (the session came from another device and has not been imported
// here yet) is shipped as is (or fails when it is unreadable or invalid),
// anything else is exported from the DB.
// errGeminiNoSession means the ref is stale.
var errGeminiNoSession = errors.New("session not found")

func geminiSessionExport(agentID, absAgentDir, sessionID string) ([]byte, error) {
	pending := filepath.Join(geminiPendingImportDir(absAgentDir), sessionID+".json")
	if fi, err := os.Lstat(pending); err == nil && fi.Mode().IsRegular() {
		if fi.Size() > geminiSessionFileMaxBytes {
			return nil, errGeminiOversized{size: fi.Size()}
		}
		// The pending export is the newest copy of the session (the
		// local DB, if it has one, predates the switch): failing to read
		// it skips the session instead of shipping the stale DB copy.
		body, rerr := os.ReadFile(pending)
		if rerr != nil {
			return nil, fmt.Errorf("read pending export of %s: %w", sessionID, rerr)
		}
		if verr := validateOpenCodeExport(body, sessionID); verr != nil {
			return nil, fmt.Errorf("pending export: %w", verr)
		}
		return body, nil
	}
	if exists, known := openCodeSessionExists(openCodeHome(absAgentDir), sessionID); known && !exists {
		return nil, errGeminiNoSession
	}
	return exportOpenCodeSessionFn(context.Background(), agentID, sessionID)
}

// geminiExportParallelism bounds the concurrent `opencode export`
// processes (each takes about half a second to start).
const geminiExportParallelism = 4

type geminiExportResult struct {
	body []byte
	err  error
}

// exportGeminiSessions exports every distinct session the refs point at,
// in ref order, geminiExportParallelism at a time. Once the exports so far
// fill the transfer budget the remaining sessions are not exported (their
// result is errGeminiOverBudget), which bounds memory to the budget plus
// one batch.
func exportGeminiSessions(agentID, absAgentDir string, refs []GeminiSessionRef) map[string]geminiExportResult {
	var ids []string
	seen := map[string]bool{}
	for _, r := range refs {
		if !seen[r.SessionID] {
			seen[r.SessionID] = true
			ids = append(ids, r.SessionID)
		}
	}
	out := make(map[string]geminiExportResult, len(ids))
	var total int64
	for start := 0; start < len(ids); start += geminiExportParallelism {
		batch := ids[start:min(start+geminiExportParallelism, len(ids))]
		if total >= geminiSessionTransferMaxTotalBytes {
			for _, id := range batch {
				out[id] = geminiExportResult{err: errGeminiOverBudget}
			}
			continue
		}
		results := make([]geminiExportResult, len(batch))
		var wg sync.WaitGroup
		for i, id := range batch {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				body, err := geminiSessionExport(agentID, absAgentDir, id)
				if err == nil && int64(len(body)) > geminiSessionFileMaxBytes {
					body, err = nil, errGeminiOversized{size: int64(len(body))}
				}
				results[i] = geminiExportResult{body: body, err: err}
			}(i, id)
		}
		wg.Wait()
		for i, id := range batch {
			out[id] = results[i]
			total += int64(len(results[i].body))
		}
	}
	return out
}

// errGeminiOverBudget marks a session not exported because earlier ones
// already filled the transfer budget.
var errGeminiOverBudget = errors.New("transfer budget exhausted")

type errGeminiOversized struct{ size int64 }

func (e errGeminiOversized) Error() string { return fmt.Sprintf("export is %d bytes", e.size) }

// ReadGeminiSessionFiles collects the agent's referenced OpenCode sessions
// for handoff. Returns (nil, skipped, nil) when there is nothing to move.
// A session whose export is oversized (or whose inclusion would exceed the
// total budget) or cannot be produced is dropped together with every ref
// pointing at it and reported in skipped; target then starts that
// conversation fresh with the recent-messages bootstrap.
func ReadGeminiSessionFiles(agentID string) (*GeminiSessionTransfer, []SkippedSessionFile, error) {
	if agentID == "" {
		return nil, nil, nil
	}
	unlock := lockGeminiSessionTransfer(agentID)
	defer unlock()

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.ReadGeminiSessionFiles: abs path: %w", err)
	}
	if err := geminiCheckRealDirs(absAgentDir); err != nil {
		return nil, nil, fmt.Errorf("agent.ReadGeminiSessionFiles: %w", err)
	}
	refs := listGeminiRefs(absAgentDir)
	if len(refs) == 0 {
		return nil, nil, nil
	}

	exports := exportGeminiSessions(agentID, absAgentDir, refs)

	out := &GeminiSessionTransfer{}
	var skipped []SkippedSessionFile
	var total int64
	shipped := map[string]bool{} // sessionID → export shipped
	for _, ref := range refs {
		wasShipped, seenBefore := shipped[ref.SessionID]
		if seenBefore {
			if wasShipped {
				out.Refs = append(out.Refs, ref)
			}
			continue
		}
		shipped[ref.SessionID] = false
		label := "gemini-session/" + ref.SessionID
		body, eerr := exports[ref.SessionID].body, exports[ref.SessionID].err
		var big errGeminiOversized
		switch {
		case errors.Is(eerr, errGeminiNoSession):
			continue // stale ref: nothing to resume
		case errors.As(eerr, &big):
			skipped = append(skipped, SkippedSessionFile{Path: label, Reason: "oversized", SizeBytes: big.size})
			continue
		case errors.Is(eerr, errGeminiOverBudget):
			skipped = append(skipped, SkippedSessionFile{Path: label, Reason: "capacity"})
			continue
		case eerr != nil:
			slog.Default().Warn("gemini session transfer: export failed",
				"agent", agentID, "sessionId", ref.SessionID, "err", eerr)
			skipped = append(skipped, SkippedSessionFile{Path: label, Reason: "unreadable"})
			continue
		}
		size := int64(len(body))
		if size > geminiSessionFileMaxBytes || total+size > geminiSessionTransferMaxTotalBytes {
			reason := "oversized"
			if size <= geminiSessionFileMaxBytes {
				reason = "capacity"
			}
			skipped = append(skipped, SkippedSessionFile{Path: label, Reason: reason, SizeBytes: size})
			continue
		}
		if len(out.Files)+1 > geminiSessionTransferMaxFiles {
			skipped = append(skipped, SkippedSessionFile{Path: label, Reason: "capacity", SizeBytes: size})
			continue
		}
		out.Files = append(out.Files, GeminiSessionFile{RelPath: ref.SessionID + ".json", Content: body})
		total += size
		shipped[ref.SessionID] = true
		out.Refs = append(out.Refs, ref)
	}
	if len(out.Refs) == 0 {
		return nil, skipped, nil
	}
	return out, skipped, nil
}

// geminiSwap is one path replacement inside a staged transfer: final is
// moved aside to backup (if it existed) and staged (if non-empty) is
// renamed into final.
type geminiSwap struct {
	final, staged, backup string
	placed                bool
}

// applyGeminiSwaps performs the swaps in order and returns commit /
// rollback callbacks. On error every swap already performed is undone
// and the remaining staged paths are removed.
func applyGeminiSwaps(agentID string, swaps []*geminiSwap, release func()) (commit, rollback func(), err error) {
	undo := func() {
		for i := len(swaps) - 1; i >= 0; i-- {
			s := swaps[i]
			if s.placed {
				if rerr := os.RemoveAll(s.final); rerr != nil {
					slog.Default().Warn("gemini session transfer: rollback failed to remove new path",
						"agent", agentID, "path", s.final, "err", rerr)
				}
				s.placed = false
			}
			if s.backup != "" {
				if rerr := os.Rename(s.backup, s.final); rerr != nil {
					slog.Default().Warn("gemini session transfer: rollback failed to restore",
						"agent", agentID, "backup", s.backup, "final", s.final, "err", rerr)
				}
				s.backup = ""
			}
		}
	}
	cleanupStaged := func() {
		for _, s := range swaps {
			if s.staged != "" && !s.placed {
				_ = os.RemoveAll(s.staged)
			}
		}
	}
	for _, s := range swaps {
		if _, statErr := os.Lstat(s.final); statErr == nil {
			bk := fmt.Sprintf("%s.kojo-bk-%d", s.final, os.Getpid())
			for i := 0; ; i++ {
				if _, e := os.Lstat(bk); os.IsNotExist(e) {
					break
				}
				bk = fmt.Sprintf("%s.kojo-bk-%d-%d", s.final, os.Getpid(), i)
			}
			if rerr := os.Rename(s.final, bk); rerr != nil {
				undo()
				cleanupStaged()
				return nil, nil, fmt.Errorf("backup %s: %w", s.final, rerr)
			}
			s.backup = bk
		}
		if s.staged != "" {
			if err := os.MkdirAll(filepath.Dir(s.final), 0o700); err != nil {
				undo()
				cleanupStaged()
				return nil, nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(s.final), err)
			}
			if rerr := os.Rename(s.staged, s.final); rerr != nil {
				undo()
				cleanupStaged()
				return nil, nil, fmt.Errorf("place %s: %w", s.final, rerr)
			}
			s.placed = true
		}
	}
	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		defer release()
		for _, s := range swaps {
			if s.backup != "" {
				if rerr := os.RemoveAll(s.backup); rerr != nil {
					slog.Default().Warn("gemini session transfer: commit failed to drop backup",
						"agent", agentID, "path", s.backup, "err", rerr)
				}
			}
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		defer release()
		undo()
	}
	return commit, rollback, nil
}

// StageGeminiSession materialises transfer under target's agent dir:
// a fresh kojo/pending-import/ holding exactly the shipped exports (the
// previous one is moved aside), plus kojo/session_id and kojo/threads/
// replaced by the shipped refs. The exports are imported into OpenCode's
// DB before the agent's next turn. Two-phase like StageGrokSession:
// commit drops the backups, rollback restores them. Returns (nil, nil,
// nil) for an empty transfer.
func StageGeminiSession(agentID string, transfer *GeminiSessionTransfer) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: agent_id required")
	}
	if transfer == nil || len(transfer.Refs) == 0 || len(transfer.Files) == 0 {
		return nil, nil, nil
	}
	releaseOnce := sync.OnceFunc(lockGeminiSessionTransfer(agentID))
	handedOff := false
	defer func() {
		if !handedOff {
			releaseOnce()
		}
	}()

	if len(transfer.Files) > geminiSessionTransferMaxFiles {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: too many files (%d > %d)",
			len(transfer.Files), geminiSessionTransferMaxFiles)
	}

	// Validate refs.
	refNames := map[string]bool{}
	for _, r := range transfer.Refs {
		if r.Name != "" && !geminiThreadRefNamePattern.MatchString(r.Name) {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: invalid ref name %q", r.Name)
		}
		if !isGeminiSessionID(r.SessionID) {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: invalid session_id %q", r.SessionID)
		}
		if refNames[r.Name] {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: duplicate ref %q", r.Name)
		}
		refNames[r.Name] = true
	}

	// Validate files: flat <sessionID>.json exports of that session.
	ids := map[string]bool{}
	var total int64
	for _, f := range transfer.Files {
		m := geminiExportFilePattern.FindStringSubmatch(f.RelPath)
		if m == nil {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: refusing relpath %q", f.RelPath)
		}
		if ids[m[1]] {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: duplicate relpath %q", f.RelPath)
		}
		ids[m[1]] = true
		if int64(len(f.Content)) > geminiSessionFileMaxBytes {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: file %q exceeds per-file cap", f.RelPath)
		}
		total += int64(len(f.Content))
		if total > geminiSessionTransferMaxTotalBytes {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: total payload exceeds %d bytes",
				geminiSessionTransferMaxTotalBytes)
		}
		if err := validateOpenCodeExport(f.Content, m[1]); err != nil {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: %w", err)
		}
	}
	for _, r := range transfer.Refs {
		if !ids[r.SessionID] {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: ref %q has no session file for %s", r.Name, r.SessionID)
		}
	}

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: abs agent dir: %w", err)
	}
	if err := os.MkdirAll(absAgentDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: mkdir agent dir: %w", err)
	}
	if err := geminiCheckRealDirs(absAgentDir); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: %w", err)
	}
	kojoDir := geminiKojoDir(absAgentDir)
	if err := os.MkdirAll(kojoDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: mkdir %s: %w", kojoDir, err)
	}
	if err := geminiCheckRealDirs(absAgentDir); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: %w", err)
	}

	var staged []string
	fail := func(format string, a ...any) (func(), func(), error) {
		for _, p := range staged {
			_ = os.RemoveAll(p)
		}
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: "+format, a...)
	}

	importStage, err := os.MkdirTemp(kojoDir, ".pending-import-staging-*")
	if err != nil {
		return fail("mkdir staging: %w", err)
	}
	staged = append(staged, importStage)
	for _, f := range transfer.Files {
		if err := os.WriteFile(filepath.Join(importStage, f.RelPath), f.Content, 0o600); err != nil {
			return fail("write %s: %w", f.RelPath, err)
		}
	}

	var mainStage, threadsStage string
	for _, r := range transfer.Refs {
		if r.Name == "" {
			tf, terr := os.CreateTemp(kojoDir, ".session_id-*.tmp")
			if terr != nil {
				return fail("create ref temp: %w", terr)
			}
			mainStage = tf.Name()
			staged = append(staged, mainStage)
			_, werr := tf.WriteString(r.SessionID)
			cerr := tf.Close()
			if werr != nil || cerr != nil {
				return fail("write main ref: %v %v", werr, cerr)
			}
			continue
		}
		if threadsStage == "" {
			threadsStage, err = os.MkdirTemp(kojoDir, ".threads-staging-*")
			if err != nil {
				return fail("mkdir threads staging: %w", err)
			}
			staged = append(staged, threadsStage)
		}
		if err := os.WriteFile(filepath.Join(threadsStage, r.Name), []byte(r.SessionID), 0o600); err != nil {
			return fail("write thread ref: %w", err)
		}
	}

	swaps := []*geminiSwap{
		{final: geminiPendingImportDir(absAgentDir), staged: importStage},
		{final: geminiSessionIDFile(absAgentDir), staged: mainStage},
		{final: geminiThreadRefDir(absAgentDir), staged: threadsStage},
	}
	commit, rollback, err = applyGeminiSwaps(agentID, swaps, releaseOnce)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: %w", err)
	}
	handedOff = true
	return commit, rollback, nil
}

// StageGeminiSessionCleanup purges target-local gemini resume state (every
// kojo ref and staged import) when the authoritative inbound snapshot
// carries no gemini block, so the next turn starts a fresh OpenCode
// session. Sessions already in OpenCode's DB are left in place
// (unreferenced). Same two-phase contract as StageGrokSessionCleanup;
// (nil, nil, nil) when there is nothing to purge.
func StageGeminiSessionCleanup(agentID string) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageGeminiSessionCleanup: agent_id required")
	}
	releaseOnce := sync.OnceFunc(lockGeminiSessionTransfer(agentID))
	handedOff := false
	defer func() {
		if !handedOff {
			releaseOnce()
		}
	}()
	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSessionCleanup: abs agent dir: %w", err)
	}
	if err := geminiCheckRealDirs(absAgentDir); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSessionCleanup: %w", err)
	}
	var swaps []*geminiSwap
	for _, p := range []string{
		geminiPendingImportDir(absAgentDir),
		geminiSessionIDFile(absAgentDir),
		geminiThreadRefDir(absAgentDir),
	} {
		if _, statErr := os.Lstat(p); statErr == nil {
			swaps = append(swaps, &geminiSwap{final: p})
		}
	}
	if len(swaps) == 0 {
		return nil, nil, nil
	}
	commit, rollback, err = applyGeminiSwaps(agentID, swaps, releaseOnce)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGeminiSessionCleanup: %w", err)
	}
	handedOff = true
	return commit, rollback, nil
}
