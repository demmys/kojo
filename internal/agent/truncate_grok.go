package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Grok session state is a directory, not a single file:
//
//	$GROK_HOME/sessions/<encoded(cwd)>/<uuid>/
//	  chat_history.jsonl   one JSON message per line (system / user /
//	                       reasoning / assistant / tool_result). This is
//	                       the conversation the model actually replays.
//	  events.jsonl         UI/telemetry event log. Every turn opens with
//	                       a `turn_started` record carrying an RFC3339
//	                       `ts` AND `conversation_message_count` — the
//	                       number of chat_history.jsonl lines that
//	                       existed when the turn began.
//	  updates.jsonl        ACP session updates, unix-seconds `timestamp`.
//	  rewind_points.jsonl  grok's own per-prompt file snapshots, RFC3339
//	                       `created_at`.
//	  summary.json         session metadata incl. num_chat_messages
//	                       (== chat_history.jsonl line count) and
//	                       num_messages (== updates.jsonl line count).
//
// That `turn_started` record is what makes a *prefix-preserving* truncate
// possible. Grok's chat_history records carry no timestamp of their own,
// which is why truncation used to drop the whole session; but events.jsonl
// gives us the exact (ts → chat line count) mapping, so we can cut
// chat_history at a real turn boundary instead of nuking the context.
//
// Verified against a live session: turn_number=1 reported
// conversation_message_count=21, and line 21 (0-based) of
// chat_history.jsonl was that turn's `user` record.

// grokSessionCut is where a truncate lands inside one grok session.
type grokSessionCut struct {
	// found is false when no turn started at-or-after the threshold,
	// i.e. the whole session predates the cut and must not be touched.
	found bool
	// chatLines is how many chat_history.jsonl lines to keep.
	chatLines int
	// eventLines is how many events.jsonl lines to keep (the index of
	// the turn_started record that opens the first dropped turn).
	eventLines int
	// inconsistent marks a session whose events.jsonl cannot be
	// trusted to locate a boundary (non-monotonic message counts).
	// The caller drops such a session instead of slicing it.
	inconsistent bool
	// firstTurn reports that the dropped turn is the earliest one in
	// the file, so nothing conversational survives and the caller
	// removes the session outright.
	firstTurn bool
}

// findGrokSessionCut locates the first turn that is NOT entirely finished
// before `since`, and cuts at its start.
//
// Matching on "the first turn_started at-or-after since" is wrong: kojo's
// boundary is the pivot *message's* timestamp, and when the pivot is an
// assistant reply that timestamp lands in the middle of its turn — after
// that turn's turn_started. Cutting on the next turn_started would leave
// the very reply the user rewound past sitting in grok's context. So a
// turn is dropped when its turn_ended (the ts of the next turn_ended
// record after its turn_started, absent for an interrupted turn) is
// at-or-after since.
//
// A malformed or timestamp-less line is skipped rather than treated as a
// boundary — same forgiving stance as the Claude JSONL classifier.
func findGrokSessionCut(eventsPath string, since time.Time) (grokSessionCut, error) {
	lines, err := readJSONLLines(eventsPath)
	if err != nil {
		return grokSessionCut{}, err
	}

	type turnStart struct {
		index int
		count int
	}
	var (
		pending    *turnStart
		turnsSeen  int
		firstTurn  bool
		candidate  *turnStart
		takeAsCut  func(ts *turnStart)
		cutAtFirst bool
		prevCount  = -1
	)
	takeAsCut = func(ts *turnStart) {
		candidate = ts
		cutAtFirst = firstTurn
	}

	for i, raw := range lines {
		var ev struct {
			Type  string `json:"type"`
			TS    string `json:"ts"`
			Count *int   `json:"conversation_message_count"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "turn_started":
			if ev.TS == "" || ev.Count == nil {
				continue
			}
			if _, perr := time.Parse(time.RFC3339Nano, ev.TS); perr != nil {
				continue
			}
			// An unterminated previous turn (crash / interrupt) has
			// no end: treat it as reaching the present.
			if pending != nil {
				takeAsCut(pending)
				break
			}
			// conversation_message_count must never move backwards:
			// it is a running line count. If it does, the log has
			// been rewritten or interleaved and we cannot trust it
			// to locate a cut — say so and let the caller drop the
			// session rather than slice at a bogus offset.
			if *ev.Count < prevCount {
				return grokSessionCut{found: true, inconsistent: true}, nil
			}
			prevCount = *ev.Count
			turnsSeen++
			firstTurn = turnsSeen == 1
			pending = &turnStart{index: i, count: *ev.Count}
		case "turn_ended":
			if pending == nil {
				continue
			}
			ended, perr := time.Parse(time.RFC3339Nano, ev.TS)
			if ev.TS == "" || perr != nil {
				// Unreadable end marker: skip it and let the next
				// valid boundary decide, rather than declaring the
				// turn unterminated and cutting a healthy prefix.
				continue
			}
			if !ended.Before(since) {
				takeAsCut(pending)
			}
			pending = nil
		}
		if candidate != nil {
			break
		}
	}
	if candidate == nil && pending != nil {
		// Trailing turn never ended — it is still "in progress" as far
		// as the log is concerned, so it is not entirely before since.
		takeAsCut(pending)
	}
	if candidate != nil {
		return grokSessionCut{
			found:      true,
			chatLines:  candidate.count,
			eventLines: candidate.index,
			firstTurn:  cutAtFirst,
		}, nil
	}
	return grokSessionCut{}, nil
}

// truncateGrokSessions trims every grok session under the agent's cwd back
// to the last turn that started before `since`, preserving the conversation
// prefix (and therefore the agent's context) instead of dropping the
// session wholesale.
//
// Sessions whose *first* turn is at-or-after the threshold have no
// surviving conversation and are removed entirely; the resume pointer is
// cleared only when it referenced such a session, so a trimmed session
// stays resumable.
//
// Best-effort across sessions: a per-session failure is logged by the
// caller via the returned error while the remaining sessions still get
// trimmed.
func truncateGrokSessions(agentID string, since time.Time, logger *slog.Logger) (entriesRemoved, sessionsRemoved, filesRemoved int, err error) {
	release := lockGrokSessionTransfer(agentID)
	defer release()

	dir := agentDir(agentID)
	sessionsDir := grokSessionDir(dir)
	if sessionsDir == "" {
		return 0, 0, 0, nil
	}
	entries, derr := os.ReadDir(sessionsDir)
	if derr != nil {
		if os.IsNotExist(derr) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, derr
	}

	dropped := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !isGrokSessionID(e.Name()) {
			continue
		}
		sub := filepath.Join(sessionsDir, e.Name())
		removed, deleted, files, serr := truncateGrokSessionDir(sub, since)
		entriesRemoved += removed
		filesRemoved += files
		if deleted {
			sessionsRemoved++
			dropped[e.Name()] = true
		}
		if serr != nil {
			if logger != nil {
				logger.Warn("truncate grok session", "path", sub, "err", serr)
			}
			if err == nil {
				err = serr
			}
		}
	}

	// Only invalidate resume pointers that referenced a session we
	// actually deleted. Clearing them unconditionally is what made the
	// old wholesale-drop behaviour lose context even when the prefix
	// would have survived.
	if len(dropped) > 0 {
		if perr := clearGrokSessionRefsIn(dir, dropped); perr != nil && err == nil {
			err = perr
		}
	}
	return entriesRemoved, sessionsRemoved, filesRemoved, err
}

// truncateGrokSessionDir trims a single <uuid>/ session directory.
// deleted reports that the whole subtree was removed because no turn
// survived the cut.
func truncateGrokSessionDir(sessionPath string, since time.Time) (entriesRemoved int, deleted bool, filesRemoved int, err error) {
	cut, cerr := findGrokSessionCut(filepath.Join(sessionPath, "events.jsonl"), since)
	if cerr != nil {
		return 0, false, 0, cerr
	}
	if !cut.found {
		// Nothing started at-or-after the threshold: the session is
		// entirely pre-T. Leave it byte-identical — mtime feeds
		// resume/idle decisions elsewhere.
		return 0, false, 0, nil
	}

	// Dropping the session's first turn leaves only the bootstrap
	// system/user_info records, which is not a usable --resume target.
	// Same for a cut that would keep no chat lines at all, or one whose
	// advertised line count exceeds what chat_history.jsonl actually
	// holds (a torn or externally-rewritten session): resuming that
	// would replay a mismatched prefix, so start fresh instead.
	chatPath := filepath.Join(sessionPath, "chat_history.jsonl")
	chatLines, cerr2 := readJSONLLines(chatPath)
	if cerr2 != nil {
		return 0, false, 0, cerr2
	}
	if cut.inconsistent || cut.firstTurn || cut.chatLines <= 0 || cut.chatLines > len(chatLines) {
		files := countFilesUnder(sessionPath)
		if rerr := os.RemoveAll(sessionPath); rerr != nil && !os.IsNotExist(rerr) {
			return 0, false, 0, rerr
		}
		return 0, true, files, nil
	}

	// From here on the session is being rewritten file by file. Each
	// individual write is atomic, but a failure partway through would
	// leave chat_history trimmed while summary.json still advertises the
	// old counts — an inconsistent --resume target. Drop the session in
	// that case: losing the prefix is strictly better than resuming a
	// session whose files disagree with each other.
	defer func() {
		if err == nil || deleted {
			return
		}
		files := countFilesUnder(sessionPath)
		rerr := os.RemoveAll(sessionPath)
		// Report deleted even when RemoveAll fails: the session is
		// half-rewritten and must not be resumed, and `deleted` is
		// what makes the caller clear the resume pointer. A stale
		// unreferenced directory is the lesser evil.
		deleted, entriesRemoved = true, 0
		if rerr == nil || os.IsNotExist(rerr) {
			filesRemoved = files
		}
	}()

	chatRemoved, err := truncateJSONLToLineCount(chatPath, cut.chatLines)
	if err != nil {
		return 0, false, 0, err
	}
	entriesRemoved += chatRemoved

	if _, eerr := truncateJSONLToLineCount(filepath.Join(sessionPath, "events.jsonl"), cut.eventLines); eerr != nil {
		return entriesRemoved, false, 0, eerr
	}

	// updates.jsonl carries unix-seconds timestamps; rewind_points.jsonl
	// carries RFC3339 created_at. Both are cut on the same threshold so
	// grok's UI replay and its own rewind snapshots stop where the
	// conversation now stops.
	updatesKept, uerr := filterJSONLLines(filepath.Join(sessionPath, "updates.jsonl"), func(raw []byte) bool {
		var u struct {
			Timestamp *int64 `json:"timestamp"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &u) != nil || u.Timestamp == nil {
			return true
		}
		return *u.Timestamp < since.Unix()
	})
	if uerr != nil {
		return entriesRemoved, false, 0, uerr
	}
	if _, rerr := filterJSONLLines(filepath.Join(sessionPath, "rewind_points.jsonl"), func(raw []byte) bool {
		var rp struct {
			CreatedAt string `json:"created_at"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &rp) != nil || rp.CreatedAt == "" {
			return true
		}
		t, perr := time.Parse(time.RFC3339Nano, rp.CreatedAt)
		if perr != nil {
			return true
		}
		return t.Before(since)
	}); rerr != nil {
		return entriesRemoved, false, 0, rerr
	}

	if serr := patchGrokSummary(filepath.Join(sessionPath, "summary.json"), cut.chatLines, updatesKept, since); serr != nil {
		return entriesRemoved, false, 0, serr
	}
	return entriesRemoved, false, 0, nil
}

// patchGrokSummary rewrites the counters summary.json advertises so they
// match the trimmed files. Edited as a generic map so fields we don't know
// about survive the round-trip verbatim.
func patchGrokSummary(path string, chatLines, updateLines int, since time.Time) error {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc map[string]json.RawMessage
	if uerr := json.Unmarshal(body, &doc); uerr != nil {
		// An unreadable summary can't be brought in line with the
		// files we just trimmed, so the session is inconsistent by
		// definition. Report it and let the caller drop the session.
		return fmt.Errorf("summary.json unparseable: %w", uerr)
	}
	set := func(key string, v any) {
		enc, merr := json.Marshal(v)
		if merr == nil {
			doc[key] = enc
		}
	}
	if _, ok := doc["num_chat_messages"]; ok {
		set("num_chat_messages", chatLines)
	}
	if _, ok := doc["num_messages"]; ok {
		set("num_messages", updateLines)
	}
	stamp := since.UTC().Format(time.RFC3339Nano)
	for _, key := range []string{"updated_at", "last_active_at"} {
		if _, ok := doc[key]; ok {
			set(key, stamp)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(out, '\n'), 0o644)
}

// readJSONLLines reads a file into per-line byte slices, keeping the
// trailing newline so a rewrite is byte-identical for surviving lines.
// A missing file yields no lines and no error.
func readJSONLLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	lines := make([][]byte, 0, 64)
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			cp := make([]byte, len(line))
			copy(cp, line)
			lines = append(lines, cp)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}
	return lines, nil
}

// writeJSONLLines rewrites path atomically from the given raw lines.
func writeJSONLLines(path string, lines [][]byte) error {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
	}
	return atomicWriteFile(path, buf.Bytes(), 0o644)
}

// truncateJSONLToLineCount keeps the first n lines of a JSONL file and
// reports how many were dropped. A file already at-or-below n lines is
// left byte-identical.
func truncateJSONLToLineCount(path string, n int) (removed int, err error) {
	lines, err := readJSONLLines(path)
	if err != nil || len(lines) <= n {
		return 0, err
	}
	removed = len(lines) - n
	return removed, writeJSONLLines(path, lines[:n])
}

// filterJSONLLines keeps the lines for which keep() returns true and
// reports how many survived. Unparseable lines are the caller's call
// (every current caller keeps them).
func filterJSONLLines(path string, keep func(raw []byte) bool) (kept int, err error) {
	lines, err := readJSONLLines(path)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		return 0, nil
	}
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		if keep(l) {
			out = append(out, l)
		}
	}
	kept = len(out)
	if kept == len(lines) {
		return kept, nil
	}
	return kept, writeJSONLLines(path, out)
}

// countFilesUnder counts regular files in a subtree, best-effort.
func countFilesUnder(root string) int {
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// clearGrokSessionRefsIn removes only the resume pointers that reference
// one of the given (just-deleted) session IDs. Pointers to sessions we
// merely trimmed are left in place — that is the whole point of a
// prefix-preserving truncate.
func clearGrokSessionRefsIn(agentDirPath string, dropped map[string]bool) error {
	var firstErr error
	drop := func(path string) {
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			if !os.IsNotExist(rerr) && firstErr == nil {
				firstErr = rerr
			}
			return
		}
		if !dropped[strings.TrimSpace(string(body))] {
			return
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) && firstErr == nil {
			firstErr = rerr
		}
	}

	drop(grokSessionIDFile(agentDirPath))

	refDir := grokThreadRefDir(agentDirPath)
	entries, derr := os.ReadDir(refDir)
	if derr != nil {
		if !os.IsNotExist(derr) && firstErr == nil {
			firstErr = derr
		}
		return firstErr
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		drop(filepath.Join(refDir, e.Name()))
	}
	return firstErr
}
