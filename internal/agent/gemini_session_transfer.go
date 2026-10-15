package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// gemini session transfer for §3.7 device switch.
//
// gemini-cli keeps one JSONL per conversation inside the agent's own
// GEMINI_CLI_HOME (geminiCLIHome = <agentDir>/.gemini-cli-home):
//
//	.gemini/tmp/<slug>/chats/session-<ts>-<id[:8]>.jsonl   (main sessions)
//	.gemini/tmp/<slug>/chats/<sessionId>/<subagentId>.jsonl (subagent logs)
//
// <slug> is the CLI's short project id: slugify(basename(cwd)) as recorded
// in .gemini/projects.json and the .project_root ownership markers. kojo's
// resume refs live next to it under kojo/ (session_id for the main
// conversation, threads/key-<uuid> per SessionKey).
//
// Verified against gemini-cli 0.64.0: copying ONLY the session JSONL into
// a fresh home under tmp/<slug>/chats/ is enough for `--resume <id>` from
// a different absolute cwd with the same basename — projects.json, the
// .project_root markers and the header's projectHash are not consulted
// for the lookup (the CLI registers the new path on first use). The
// markers must NOT be copied: they hold source's absolute path and would
// make target's CLI treat the slug as owned by another project and claim
// "<slug>-1" instead, losing the session.
//
// A handoff therefore carries every kojo ref (main + per-SessionKey
// threads) whose session file exists, plus those sessions' JSONLs and
// subagent logs. Target swaps in a fresh .gemini/tmp holding exactly
// those files under its own slug, and replaces its refs. OneShot sessions
// (no ref) stay behind, as for grok.

// GeminiSessionRef is one kojo resume ref. Name is "" for the main
// conversation (kojo/session_id) or the threads/ file name
// ("key-<uuid>") for a SessionKey thread.
type GeminiSessionRef struct {
	Name      string
	SessionID string
}

// GeminiSessionFile is one file from source's chats dir. RelPath is
// relative to chats/: "session-<ts>-<short>.jsonl" or
// "<sessionId>/<name>.jsonl" for a subagent log.
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
	geminiMainFilePattern      = regexp.MustCompile(`^session-[0-9A-Za-z_\-]+\.jsonl$`)
	geminiSubagentFilePattern  = regexp.MustCompile(`^[0-9A-Za-z_\-]+\.jsonl$`)
)

var geminiSessionTransferLocks keyedMutex

// geminiRefuseSymlinks errors when any existing path in paths is a
// symlink or not a directory; missing paths are fine. The agent can
// write its own dir, and a planted link inside its gemini home would
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

// geminiCheckRealDirs applies geminiRefuseSymlinks to the fixed gemini
// state directories under the agent dir: GEMINI_CLI_HOME, its .gemini
// and .gemini/tmp, and the kojo ref dirs. Per-project tmp/<slug>/chats
// dirs are checked where they are walked (geminiCollectSession).
func geminiCheckRealDirs(absAgentDir string) error {
	home := geminiCLIHome(absAgentDir)
	return geminiRefuseSymlinks(
		home,
		filepath.Join(home, ".gemini"),
		filepath.Join(home, ".gemini", "tmp"),
		geminiKojoDir(absAgentDir),
		geminiThreadRefDir(absAgentDir),
	)
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

// geminiSessionTransferFiles collects one session's files: its main
// JSONL(s) plus regular *.jsonl files in chats/<id>/ (subagent logs).
type geminiCollected struct {
	rel  string
	path string
	size int64
	core bool
}

func geminiCollectSession(absAgentDir, sessionID string) []geminiCollected {
	var out []geminiCollected
	for _, p := range geminiSessionFiles(absAgentDir, sessionID) {
		chats := filepath.Dir(p)
		if geminiRefuseSymlinks(filepath.Dir(chats), chats) != nil {
			continue // tmp/<slug> or chats is a link: not ours to ship
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, geminiCollected{rel: filepath.Base(p), path: p, size: fi.Size(), core: true})
		sub := filepath.Join(chats, sessionID)
		if geminiRefuseSymlinks(sub) != nil {
			continue
		}
		entries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !geminiSubagentFilePattern.MatchString(e.Name()) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			out = append(out, geminiCollected{
				rel: sessionID + "/" + e.Name(), path: filepath.Join(sub, e.Name()), size: info.Size(),
			})
		}
	}
	return out
}

// ReadGeminiSessionFiles collects the agent's referenced gemini sessions
// for handoff. Returns (nil, skipped, nil) when there is nothing to move.
// A session whose main JSONL is oversized (or whose inclusion would
// exceed the total budget) is dropped together with every ref pointing
// at it and reported in skipped; target then starts that conversation
// fresh with the recent-messages bootstrap. Oversized subagent logs are
// skipped individually.
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

	out := &GeminiSessionTransfer{}
	var skipped []SkippedSessionFile
	var total int64
	shipped := map[string]bool{} // sessionID → files shipped
	seenRel := map[string]bool{}
	for _, ref := range refs {
		wasShipped, seenBefore := shipped[ref.SessionID]
		if seenBefore {
			if wasShipped {
				out.Refs = append(out.Refs, ref)
			}
			continue
		}
		collected := geminiCollectSession(absAgentDir, ref.SessionID)
		var mainSize int64
		hasMain := false
		for _, c := range collected {
			if c.core {
				hasMain = true
				mainSize += c.size
			}
		}
		if !hasMain {
			// Stale ref (session file deleted / GC'd): nothing to resume.
			shipped[ref.SessionID] = false
			continue
		}
		oversized := false
		for _, c := range collected {
			if c.core && c.size > geminiSessionFileMaxBytes {
				oversized = true
			}
		}
		if oversized || total+mainSize > geminiSessionTransferMaxTotalBytes {
			reason := "oversized"
			if !oversized {
				reason = "capacity"
			}
			skipped = append(skipped, SkippedSessionFile{
				Path: "gemini-session/" + ref.SessionID, Reason: reason, SizeBytes: mainSize,
			})
			shipped[ref.SessionID] = false
			continue
		}
		var files []GeminiSessionFile
		var sessionBytes int64
		for _, c := range collected {
			if seenRel[strings.ToLower(c.rel)] {
				continue
			}
			if !c.core && (c.size > geminiSessionFileMaxBytes ||
				total+sessionBytes+c.size > geminiSessionTransferMaxTotalBytes) {
				skipped = append(skipped, SkippedSessionFile{Path: c.rel, Reason: "oversized", SizeBytes: c.size})
				continue
			}
			body, rerr := os.ReadFile(c.path)
			if rerr != nil {
				if c.core {
					return nil, skipped, fmt.Errorf("agent.ReadGeminiSessionFiles: read %s: %w", c.rel, rerr)
				}
				skipped = append(skipped, SkippedSessionFile{Path: c.rel, Reason: "unreadable"})
				continue
			}
			files = append(files, GeminiSessionFile{RelPath: c.rel, Content: body})
			sessionBytes += int64(len(body))
		}
		if len(out.Files)+len(files) > geminiSessionTransferMaxFiles {
			return nil, skipped, fmt.Errorf("agent.ReadGeminiSessionFiles: more than %d files", geminiSessionTransferMaxFiles)
		}
		for _, f := range files {
			seenRel[strings.ToLower(f.RelPath)] = true
		}
		out.Files = append(out.Files, files...)
		total += sessionBytes
		shipped[ref.SessionID] = true
		out.Refs = append(out.Refs, ref)
	}
	if len(out.Refs) == 0 {
		return nil, skipped, nil
	}
	return out, skipped, nil
}

// geminiNormalizeProjectPath mirrors gemini-cli's normalizePath: absolute,
// lower-cased on case-insensitive platforms.
func geminiNormalizeProjectPath(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// geminiSlugify mirrors gemini-cli's ProjectRegistry.slugify.
func geminiSlugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "project"
	}
	return out
}

// geminiProjectSlug predicts the short project id target's CLI will use
// for cwd absAgentDir inside its GEMINI_CLI_HOME, following
// ProjectRegistry.getShortId: an existing projects.json mapping for the
// path wins (gemini-cli 0.64 verifies it only against markers we never
// write), otherwise a marker already owned by the path, otherwise the
// first free slugify(basename)[-N] that no other path maps to or marks.
// excludeTmp ignores the tmp/ markers (they are about to be swapped out).
func geminiProjectSlug(home, absAgentDir string, excludeTmp bool) string {
	candidates := []string{absAgentDir}
	if real, err := filepath.EvalSymlinks(absAgentDir); err == nil && real != absAgentDir {
		candidates = append([]string{real}, candidates...)
	}
	norm := map[string]bool{}
	for _, c := range candidates {
		norm[geminiNormalizeProjectPath(c)] = true
	}
	gdir := filepath.Join(home, ".gemini")

	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	if data, err := os.ReadFile(filepath.Join(gdir, "projects.json")); err == nil {
		_ = json.Unmarshal(data, &reg)
	}
	baseDirs := []string{filepath.Join(gdir, "history")}
	if !excludeTmp {
		baseDirs = append(baseDirs, filepath.Join(gdir, "tmp"))
	}
	markerOwner := func(slug string) (owner string, found bool) {
		for _, bd := range baseDirs {
			data, err := os.ReadFile(filepath.Join(bd, slug, ".project_root"))
			if err == nil {
				o := geminiNormalizeProjectPath(strings.TrimSpace(string(data)))
				if !norm[o] {
					return o, true
				}
				owner, found = o, true
			}
		}
		return owner, found
	}
	taken := map[string]bool{}
	for path, slug := range reg.Projects {
		if !norm[geminiNormalizeProjectPath(path)] {
			taken[slug] = true
			continue
		}
		if slug == "" || strings.ContainsAny(slug, `/\`) || slug == "." || slug == ".." {
			continue
		}
		// getShortId drops a mapping whose marker another path owns.
		if owner, ok := markerOwner(slug); ok && !norm[owner] {
			continue
		}
		return slug
	}
	for _, bd := range baseDirs {
		entries, err := os.ReadDir(bd)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if owner, ok := markerOwner(e.Name()); ok && norm[owner] {
				return e.Name()
			}
		}
	}
	base := geminiSlugify(filepath.Base(absAgentDir))
	for i := 0; ; i++ {
		cand := base
		if i > 0 {
			cand = fmt.Sprintf("%s-%d", base, i)
		}
		if taken[cand] {
			continue
		}
		if owner, ok := markerOwner(cand); ok && !norm[owner] {
			continue
		}
		return cand
	}
}

// geminiSessionHeaderID reads the sessionId from a JSONL body's first line.
func geminiSessionHeaderID(body []byte) string {
	line := body
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var hdr struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &hdr) != nil {
		return ""
	}
	return hdr.SessionID
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
			if err := os.MkdirAll(filepath.Dir(s.final), 0o755); err != nil {
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
// a fresh .gemini/tmp/<slug>/chats/ holding exactly the shipped files
// (the previous tmp/ is moved aside), plus kojo/session_id and
// kojo/threads/ replaced by the shipped refs. Two-phase like
// StageGrokSession: commit drops the backups, rollback restores them.
// Returns (nil, nil, nil) for an empty transfer.
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

	// Validate files: flat main JSONLs or one-level <sessionId>/<x>.jsonl.
	seen := map[string]bool{}
	mainIDs := map[string]bool{}
	var total int64
	for _, f := range transfer.Files {
		rel := f.RelPath
		parts := strings.Split(rel, "/")
		switch {
		case len(parts) == 1 && geminiMainFilePattern.MatchString(parts[0]):
			mainIDs[geminiSessionHeaderID(f.Content)] = true
		case len(parts) == 2 && isGeminiSessionID(parts[0]) && geminiSubagentFilePattern.MatchString(parts[1]):
		default:
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: refusing relpath %q", rel)
		}
		key := strings.ToLower(rel)
		if seen[key] {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: duplicate relpath %q", rel)
		}
		seen[key] = true
		if int64(len(f.Content)) > geminiSessionFileMaxBytes {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: file %q exceeds per-file cap", rel)
		}
		total += int64(len(f.Content))
		if total > geminiSessionTransferMaxTotalBytes {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: total payload exceeds %d bytes",
				geminiSessionTransferMaxTotalBytes)
		}
	}
	for _, r := range transfer.Refs {
		if !mainIDs[r.SessionID] {
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
	home := geminiCLIHome(absAgentDir)
	gdir := filepath.Join(home, ".gemini")
	kojoDir := geminiKojoDir(absAgentDir)
	for _, d := range []string{gdir, kojoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, nil, fmt.Errorf("agent.StageGeminiSession: mkdir %s: %w", d, err)
		}
	}
	slug := geminiProjectSlug(home, absAgentDir, true)

	var staged []string
	fail := func(format string, a ...any) (func(), func(), error) {
		for _, p := range staged {
			_ = os.RemoveAll(p)
		}
		return nil, nil, fmt.Errorf("agent.StageGeminiSession: "+format, a...)
	}

	tmpStage, err := os.MkdirTemp(gdir, ".tmp-staging-*")
	if err != nil {
		return fail("mkdir staging: %w", err)
	}
	staged = append(staged, tmpStage)
	chats := filepath.Join(tmpStage, slug, "chats")
	for _, f := range transfer.Files {
		dst := filepath.Join(chats, filepath.FromSlash(f.RelPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fail("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, f.Content, 0o644); err != nil {
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
		if err := os.WriteFile(filepath.Join(threadsStage, r.Name), []byte(r.SessionID), 0o644); err != nil {
			return fail("write thread ref: %w", err)
		}
	}

	swaps := []*geminiSwap{
		{final: filepath.Join(gdir, "tmp"), staged: tmpStage},
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

// StageGeminiSessionCleanup purges target-local gemini session state
// (.gemini/tmp and every kojo ref) when the authoritative inbound
// snapshot carries no gemini block. Same two-phase contract as
// StageGrokSessionCleanup; (nil, nil, nil) when there is nothing to purge.
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
		filepath.Join(geminiCLIHome(absAgentDir), ".gemini", "tmp"),
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
