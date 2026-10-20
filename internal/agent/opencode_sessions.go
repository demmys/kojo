package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/proctree"
	_ "modernc.org/sqlite"
)

// openCodeAuxTimeout bounds the short OpenCode subcommands kojo runs
// (export / import / session delete).
const openCodeAuxTimeout = 2 * time.Minute

// openOpenCodeDB opens the agent's OpenCode session DB read-only, or
// returns (nil, nil) when it does not exist yet.
func openOpenCodeDB(home string) (*sql.DB, error) {
	// Never follow a planted link out of the home (e.g. data ->
	// ~/.local/share): that would read the operator's own OpenCode DB.
	if err := geminiRefuseSymlinks(home, filepath.Join(home, "data"), openCodeDataDir(home)); err != nil {
		return nil, err
	}
	path := openCodeDBPath(home)
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("opencode db %s is not a regular file", path)
	}
	// SQLite opens the sidecars by name next to the DB and follows links.
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		if fi, err := os.Lstat(path + side); err == nil && !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("opencode db sidecar %s is not a regular file", path+side)
		}
	}
	return sql.Open("sqlite", codexSQLiteDSN(path, true))
}

// openCodeSessionExists reports whether sessionID is in the agent's
// OpenCode DB. known=false means the DB could not be read (locked,
// migrated schema...): callers then keep the ref and let OpenCode decide.
func openCodeSessionExists(home, sessionID string) (exists, known bool) {
	if !isGeminiSessionID(sessionID) {
		return false, true
	}
	db, err := openOpenCodeDB(home)
	if err != nil {
		return false, false
	}
	if db == nil {
		return false, true
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = ?`, sessionID).Scan(&n); err != nil {
		return false, false
	}
	return n > 0, true
}

// openCodeSessionDirMismatch reports whether sessionID is in the DB with
// a directory other than dir (or its symlink-resolved form). `opencode
// run -s` executes such a session in its own directory's instance while
// the event stream it prints is subscribed in the cwd's instance, so the
// turn would never produce output nor exit; callers start fresh instead.
// Unknown (no DB, unreadable, no row) is reported as no mismatch.
func openCodeSessionDirMismatch(home, sessionID, dir string) (bool, string) {
	db, err := openOpenCodeDB(home)
	if err != nil || db == nil {
		return false, ""
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT directory FROM session WHERE id = ?`, sessionID).Scan(&got); err != nil {
		return false, ""
	}
	return !sameOpenCodeDir(got, dir), got
}

// sameOpenCodeDir compares a session's stored directory (OpenCode keeps
// the resolved path, e.g. /private/tmp on macOS) with dir. When both
// exist the file system decides (os.SameFile follows links and knows
// whether the volume folds case: a case-sensitive APFS volume can hold
// /x/Agent and /x/agent side by side); otherwise the resolved paths are
// compared, case-folded only on Windows.
func sameOpenCodeDir(stored, dir string) bool {
	if stored == "" || stored == dir {
		return true
	}
	if a, err := os.Stat(stored); err == nil {
		if b, err := os.Stat(dir); err == nil {
			return os.SameFile(a, b)
		}
	}
	norm := func(p string) string {
		p = filepath.Clean(p)
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		return p
	}
	return norm(stored) == norm(dir)
}

// openCodeLatestInteractiveSession returns the most recently updated root
// session started in workDir that is not one of kojo's chat sessions
// (those carry openCodeChatTitle), or "" when there is none. It is what
// a PTY restart resumes: OpenCode's own --continue would pick a chat
// session that the agent's next chat turn resumes concurrently.
func openCodeLatestInteractiveSession(home, workDir string) string {
	db, err := openOpenCodeDB(home)
	if err != nil || db == nil {
		return ""
	}
	defer db.Close()
	dirs := []string{workDir}
	if real, rerr := filepath.EvalSymlinks(workDir); rerr == nil && real != workDir {
		dirs = append(dirs, real)
	}
	var id string
	err = db.QueryRow(`SELECT id FROM session
		WHERE parent_id IS NULL AND time_archived IS NULL AND title != ? AND directory IN (?, ?)
		ORDER BY time_updated DESC LIMIT 1`,
		openCodeChatTitle, dirs[0], dirs[len(dirs)-1]).Scan(&id)
	if err != nil || !isGeminiSessionID(id) {
		return ""
	}
	return id
}

// openCodeAuxEnv is the environment of kojo's own OpenCode subcommands:
// the same isolation as a turn, but no API key and no turn config (so no
// MCP server is spawned).
func openCodeAuxEnv(agentID, dir string) []string {
	return openCodeAuxEnvIn(agentID, openCodeHome(dir), dir)
}

func openCodeAuxEnvIn(agentID, home, cwd string) []string {
	env := filterEnv(geminiStripEnvPrefixes, agentID, cwd)
	env = append(env, "PWD="+cwd)
	return append(env, openCodeIsolationEnv(home, 0)...)
}

// runOpenCodeAux runs `opencode <args>` in the agent dir with the
// isolated environment and returns stdout. A timeout kills the tree.
func runOpenCodeAux(ctx context.Context, ocPath, agentID, dir string, args ...string) ([]byte, error) {
	return runOpenCodeIn(ctx, ocPath, agentID, openCodeHome(dir), dir, 0, args...)
}

// runOpenCodeIn runs `opencode <args>` against home with cwd (and PWD,
// which OpenCode prefers when resolving its project directory) set to
// cwd. maxStdout > 0 caps the captured stdout: once the output would
// exceed it the process tree is killed and errGeminiOversized returned,
// so a huge export never has to fit in memory.
func runOpenCodeIn(ctx context.Context, ocPath, agentID, home, cwd string, maxStdout int64, args ...string) ([]byte, error) {
	if err := ensureOpenCodeHome(home); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, openCodeAuxTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ocPath, args...)
	cmd.Env = openCodeAuxEnvIn(agentID, home, cwd)
	cmd.Dir = cwd
	cmd.Stdin = nil
	setProcGroup(cmd)
	// Cancel can run after Wait has reaped the process (exec's ctx
	// watcher races Wait's return), so the kill is tied to the start
	// time recorded right after Start: a reused pid is left alone.
	var identMu sync.Mutex
	var ident proctree.Ident
	cmd.Cancel = func() error {
		identMu.Lock()
		id := ident
		identMu.Unlock()
		proctree.KillTreeIdent(cmd.Process.Pid, id, time.Second)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	stdout := &cappedBuffer{max: maxStdout, onOverflow: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = &limitedWriter{w: &stderr, limit: 4096}
	identMu.Lock()
	err := cmd.Start()
	if err == nil {
		ident = proctree.Identify(cmd.Process.Pid)
	}
	identMu.Unlock()
	if err == nil {
		err = cmd.Wait()
	}
	if stdout.over {
		return nil, errGeminiOversized{size: stdout.seen}
	}
	if err != nil {
		if msg := strings.TrimSpace(stripANSI(stderr.String())); msg != "" {
			return stdout.buf.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.buf.Bytes(), err
	}
	return stdout.buf.Bytes(), nil
}

// cappedBuffer collects up to max bytes (max <= 0: unbounded). The first
// write past the cap drops the buffer, marks it over and calls
// onOverflow; later writes are only counted. exec feeds it from a single
// goroutine and it is read only after Wait, so it needs no lock.
type cappedBuffer struct {
	buf        bytes.Buffer
	max        int64
	seen       int64
	over       bool
	onOverflow func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.seen += int64(len(p))
	if c.over {
		return len(p), nil
	}
	if c.max > 0 && c.seen > c.max {
		c.over = true
		c.buf = bytes.Buffer{}
		if c.onOverflow != nil {
			c.onOverflow()
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// createOpenCodeTUISessionFn is swapped by tests.
var createOpenCodeTUISessionFn = createOpenCodeTUISession

// createOpenCodeTUISession creates an empty OpenCode session for an
// interactive (TUI) launch in workDir and returns its id, so the launch
// can pass `-s <id>` and kojo knows exactly which session a Restart has
// to resume (the TUI otherwise picks its own id, and resolving it later
// by "latest session in this directory" can land on another terminal's
// session). OpenCode has no "create session" subcommand, so this imports
// an export with no messages, run from workDir so OpenCode re-homes it
// there exactly as the TUI would. The title keeps OpenCode's default
// prefix so the TUI still generates a real title from the first prompt.
func createOpenCodeTUISession(ctx context.Context, ocPath, agentID, home, workDir string) (string, error) {
	now := time.Now()
	id, err := newOpenCodeSessionID(now)
	if err != nil {
		return "", err
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	ms := now.UnixMilli()
	body, err := json.Marshal(map[string]any{
		"info": map[string]any{
			"id":        id,
			"slug":      "kojo-" + hex.EncodeToString(rnd[:]),
			"projectID": "global",
			"directory": workDir,
			"title":     "New session - " + now.UTC().Format("2006-01-02T15:04:05.000Z"),
			"version":   "kojo",
			"time":      map[string]any{"created": ms, "updated": ms},
		},
		"messages": []any{},
	})
	if err != nil {
		return "", err
	}
	if err := ensureOpenCodeHome(home); err != nil {
		return "", err
	}
	kdir := openCodeKojoDir(home)
	if err := geminiRefuseSymlinks(home, kdir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(kdir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(kdir, ".tui-session-*.json")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_ = f.Chmod(0o600)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	out, err := runOpenCodeIn(ctx, ocPath, agentID, home, workDir, 0, "import", tmp)
	if err != nil {
		return "", err
	}
	if !bytes.Contains(out, []byte("Imported session")) {
		msg := strings.TrimSpace(stripANSI(string(out)))
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return "", fmt.Errorf("opencode import did not report the session: %q", msg)
	}
	if exists, known := openCodeSessionExists(home, id); !known || !exists {
		return "", fmt.Errorf("created session %s is not in the opencode db", id)
	}
	return id, nil
}

const openCodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// newOpenCodeSessionID mirrors OpenCode's Identifier.descending("session"):
// "ses_" + the low 48 bits of ^(ms*0x1000+counter) as 12 hex digits + 14
// base62 characters (bytes % 62, like OpenCode). Descending ids sort
// newest first, which OpenCode's session list relies on.
func newOpenCodeSessionID(now time.Time) (string, error) {
	v := ^(uint64(now.UnixMilli())*0x1000 + 1) & (1<<48 - 1)
	var rb [14]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	var tail [14]byte
	for i, b := range rb {
		tail[i] = openCodeBase62[int(b)%62]
	}
	return fmt.Sprintf("ses_%012x%s", v, tail[:]), nil
}

// deleteOpenCodeSession removes a OneShot turn's session from the DB.
func deleteOpenCodeSession(ocPath, agentID, dir, sessionID string, logger *slog.Logger) {
	if !isGeminiSessionID(sessionID) {
		return
	}
	if _, err := runOpenCodeAux(context.Background(), ocPath, agentID, dir, "session", "delete", sessionID); err != nil && logger != nil {
		logger.Warn("gemini: oneshot session cleanup failed", "agent", agentID, "sessionId", sessionID, "err", err)
	}
}

// exportOpenCodeSession returns `opencode export <id>` (the session and
// all of its messages as JSON).
func exportOpenCodeSession(ctx context.Context, agentID, sessionID string) ([]byte, error) {
	if !isGeminiSessionID(sessionID) {
		return nil, fmt.Errorf("invalid opencode session id %q", sessionID)
	}
	ocPath, err := exec.LookPath("opencode")
	if err != nil {
		return nil, errors.New("opencode not found in PATH")
	}
	// Cap the read (with a little slack for the banner/whitespace trimmed
	// below; the exact limit is enforced on the trimmed body) so an
	// oversized session is killed mid-export instead of buffered whole.
	dir := agentDir(agentID)
	out, err := runOpenCodeIn(ctx, ocPath, agentID, openCodeHome(dir), dir, geminiSessionFileMaxBytes+64<<10, "export", sessionID)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	if err := validateOpenCodeExport(out, sessionID); err != nil {
		return nil, err
	}
	return out, nil
}

func hasPendingOpenCodeImport(agentDirPath, sessionID string) bool {
	if !isGeminiSessionID(sessionID) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(geminiPendingImportDir(agentDirPath), sessionID+".json"))
	return err == nil && fi.Mode().IsRegular()
}

// importPendingOpenCodeSessions imports the session exports a device
// switch staged for the agent (`opencode import`, which re-homes them to
// the agent dir) and removes each one that imported. A failed export is
// kept and retried on the next turns; after openCodeImportMaxAttempts it
// is renamed to *.failed (kept, no longer retried every turn).
func importPendingOpenCodeSessions(ctx context.Context, agentID string, logger *slog.Logger) {
	dir := agentDir(agentID)
	pending := geminiPendingImportDir(dir)
	if _, err := os.Lstat(pending); err != nil {
		return
	}
	release := lockGeminiSessionTransfer(agentID)
	defer release()
	if err := geminiRefuseSymlinks(openCodeHome(dir), geminiKojoDir(dir), pending); err != nil {
		if logger != nil {
			logger.Warn("gemini: refusing pending session import", "agent", agentID, "err", err)
		}
		return
	}
	entries, err := os.ReadDir(pending)
	if err != nil {
		return
	}
	ocPath, err := exec.LookPath("opencode")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		id := strings.TrimSuffix(name, ".json")
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") || !isGeminiSessionID(id) {
			continue
		}
		path := filepath.Join(pending, name)
		// import merges into an existing session with the same id
		// (messages already present are kept as they were), so a session
		// that went A → B → A would keep A's stale copy of any part B
		// changed. Drop the local copy first; the export is authoritative.
		// When the local copy cannot be dropped (or its presence cannot
		// be read), the import is skipped and the export kept for the
		// next turn: importing over it would merge stale parts.
		home := openCodeHome(dir)
		exists, known := openCodeSessionExists(home, id)
		if !known {
			if logger != nil {
				logger.Warn("gemini: cannot read the opencode db; session import postponed", "agent", agentID, "sessionId", id)
			}
			// Counted like a failed import: a turn resuming this session
			// fails while the export is pending, so it must not stay
			// pending forever.
			noteOpenCodeImportFailure(path)
			continue
		}
		if exists {
			_, derr := runOpenCodeAux(ctx, ocPath, agentID, dir, "session", "delete", id)
			if still, known := openCodeSessionExists(home, id); derr != nil || !known || still {
				if ctx.Err() != nil {
					return
				}
				if logger != nil {
					logger.Warn("gemini: dropping local copy before import failed; import postponed", "agent", agentID, "sessionId", id, "err", derr)
				}
				noteOpenCodeImportFailure(path)
				continue
			}
		}
		out, ierr := runOpenCodeAux(ctx, ocPath, agentID, dir, "import", path)
		if ierr == nil && bytes.Contains(out, []byte("Imported session")) {
			_ = os.Remove(path)
			_ = os.Remove(path + openCodeImportAttemptsSuffix)
			if logger != nil {
				logger.Info("gemini: imported transferred opencode session", "agent", agentID, "sessionId", id)
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if logger != nil {
			logger.Warn("gemini: opencode session import failed", "agent", agentID, "sessionId", id, "err", ierr)
		}
		noteOpenCodeImportFailure(path)
	}
}

// openCodeImportMaxAttempts is how many turns retry a pending import
// that fails (a transient lock or timeout usually clears) before the
// export is renamed to *.failed: kept, but no longer retried every turn.
const openCodeImportMaxAttempts = 3

const openCodeImportAttemptsSuffix = ".attempts"

// noteOpenCodeImportFailure counts a failed attempt for the pending
// export at path and parks it as *.failed once the budget is spent.
func noteOpenCodeImportFailure(path string) {
	counter := path + openCodeImportAttemptsSuffix
	n := 0
	if fi, err := os.Lstat(counter); err == nil && fi.Mode().IsRegular() {
		if b, err := os.ReadFile(counter); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	n++
	if n >= openCodeImportMaxAttempts {
		_ = os.Rename(path, path+".failed")
		_ = os.Remove(counter)
		return
	}
	_ = atomicfile.WriteBytes(counter, []byte(strconv.Itoa(n)), 0o600)
}
