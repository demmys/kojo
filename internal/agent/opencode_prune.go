package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// OpenCode replays every stored part of a session on every model request:
// a finished turn's intermediate steps (assistant messages that only ran
// tools) go back to the model on each later step with their full tool
// output and, on Gemini, a thoughtSignature blob per call. Long agents
// pile up hundreds of them — measured at ~120k of ~147k tokens per step in
// one session — while the turn's final answer already carries what the
// user saw. pruneOpenCodeSession deletes those intermediate messages from
// turns older than the last few, keeping every user message and every
// assistant message with text.
const (
	// openCodePruneKeepTurns is how many of the most recent user turns
	// keep their tool steps untouched (the model may still refer to them).
	openCodePruneKeepTurns = 3
	// openCodePruneMinBytes batches the pruning: deleting history shifts
	// the request prefix and costs one cache miss, so it runs only once
	// this much prunable part data (~16k tokens) has built up.
	openCodePruneMinBytes = 64 << 10
	openCodePruneTimeout  = 10 * time.Second
)

// openCodePruneMessage is the subset of an OpenCode message row needed to
// decide whether it is a finished intermediate tool step.
type openCodePruneMessage struct {
	id        string
	role      string
	finish    string
	completed bool
	summary   bool
	parts     int
	tools     int
	blocking  bool // a part that must survive (text, file, pending tool, ...)
	bytes     int64
	// tailRef: a compaction part names this message as the first one of
	// the retained tail. OpenCode's filterCompacted walks back until it
	// meets that id; deleting it would resend the whole pre-compaction
	// history.
	tailRef bool
}

// pruneOpenCodeSession removes finished, tool-only assistant messages that
// sit before the last openCodePruneKeepTurns user messages of sessionID.
// It must run while no `opencode run` holds the session (before the turn
// starts). Returns the number of messages removed; 0 with a nil error when
// there is nothing (or not yet enough) to prune or the DB is in a shape
// this code does not know.
func pruneOpenCodeSession(ctx context.Context, home, sessionID string) (int, error) {
	if !isGeminiSessionID(sessionID) {
		return 0, nil
	}
	db, err := openOpenCodeDBMode(home, false)
	if err != nil || db == nil {
		return 0, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, openCodePruneTimeout)
	defer cancel()

	// A pending revert points at message IDs, and rows in the newer
	// session_message table mean a storage layout this code was not
	// written against: leave such sessions alone.
	var revert sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT revert FROM session WHERE id = ?`, sessionID).Scan(&revert); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if revert.Valid && revert.String != "" && revert.String != "null" {
		return 0, nil
	}
	var hasV2 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_message'`).Scan(&hasV2); err != nil {
		return 0, err
	}
	if hasV2 > 0 {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_message WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
			return 0, err
		}
		if n > 0 {
			return 0, nil
		}
	}

	msgs, err := loadOpenCodePruneMessages(ctx, db, sessionID)
	if err != nil {
		return 0, err
	}
	ids := openCodePruneCandidates(msgs, openCodePruneKeepTurns, openCodePruneMinBytes)
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	removed := 0
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM part WHERE message_id = ? AND session_id = ?`, id, sessionID); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM message WHERE id = ? AND session_id = ?`, id, sessionID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

func loadOpenCodePruneMessages(ctx context.Context, db *sql.DB, sessionID string) ([]*openCodePruneMessage, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	var msgs []*openCodePruneMessage
	byID := map[string]*openCodePruneMessage{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var info struct {
			Role   string `json:"role"`
			Finish string `json:"finish"`
			Time   struct {
				Completed json.Number `json:"completed"`
			} `json:"time"`
			Summary json.RawMessage `json:"summary"`
		}
		m := &openCodePruneMessage{id: id}
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			// Unknown shape: keep it, and treat it as a turn boundary so
			// nothing around it is judged on a guess.
			m.role, m.blocking = "user", true
		} else {
			m.role = info.Role
			m.finish = info.Finish
			m.completed = info.Time.Completed != "" && info.Time.Completed != "0"
			sum := strings.TrimSpace(string(info.Summary))
			m.summary = sum != "" && sum != "null" && sum != "false"
		}
		msgs = append(msgs, m)
		byID[id] = m
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	prows, err := db.QueryContext(ctx,
		`SELECT message_id, data FROM part WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	var tailRefs []string
	for prows.Next() {
		var mid, data string
		if err := prows.Scan(&mid, &data); err != nil {
			return nil, err
		}
		m := byID[mid]
		if m == nil {
			continue
		}
		m.parts++
		m.bytes += int64(len(data))
		var p struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			TailStartID string `json:"tail_start_id"`
			State       struct {
				Status string `json:"status"`
			} `json:"state"`
		}
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			m.blocking = true
			continue
		}
		switch p.Type {
		case "step-start", "step-finish", "reasoning":
		case "tool":
			m.tools++
			if p.State.Status != "completed" && p.State.Status != "error" {
				m.blocking = true
			}
		case "text":
			if strings.TrimSpace(p.Text) != "" {
				m.blocking = true
			}
		case "compaction":
			m.blocking = true
			if p.TailStartID != "" {
				tailRefs = append(tailRefs, p.TailStartID)
			}
		default:
			// file / patch / subtask / ...: not a plain tool step.
			m.blocking = true
		}
	}
	if err := prows.Err(); err != nil {
		return nil, err
	}
	for _, id := range tailRefs {
		if m := byID[id]; m != nil {
			m.tailRef = true
		}
	}
	return msgs, nil
}

// openCodePruneCandidates picks the finished tool-only assistant messages
// before the last keepTurns user messages, or nothing when their combined
// part data is below minBytes or a run looks live in the kept window.
func openCodePruneCandidates(msgs []*openCodePruneMessage, keepTurns int, minBytes int64) []string {
	users := 0
	cutoff := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].role == "user" {
			users++
			if users == keepTurns {
				cutoff = i
				break
			}
		}
	}
	if cutoff <= 0 {
		return nil
	}
	// An unfinished assistant message in the kept window means a run may
	// still be live on this session (a concurrent turn on the same key):
	// leave the history it is reading alone. A crashed run's leftover
	// ages out of the window and stops blocking.
	for _, m := range msgs[cutoff:] {
		if m.role == "assistant" && !m.completed {
			return nil
		}
	}
	var ids []string
	var total int64
	for _, m := range msgs[:cutoff] {
		if m.role != "assistant" || !m.completed || m.summary || m.blocking || m.tailRef ||
			m.finish != "tool-calls" || m.tools == 0 {
			continue
		}
		ids = append(ids, m.id)
		total += m.bytes
	}
	if total < minBytes {
		return nil
	}
	return ids
}

// pruneOpenCodeSessionLogged runs pruneOpenCodeSession best-effort: a
// failure only costs the token savings, never the turn.
func pruneOpenCodeSessionLogged(ctx context.Context, home, agentID, sessionID string, logger *slog.Logger) {
	n, err := pruneOpenCodeSession(ctx, home, sessionID)
	if err != nil {
		logger.Warn("gemini: opencode history prune failed", "agent", agentID, "sessionId", sessionID, "err", fmt.Sprint(err))
		return
	}
	if n > 0 {
		logger.Info("gemini: pruned finished tool steps from opencode history",
			"agent", agentID, "sessionId", sessionID, "messages", n)
	}
}

// openCodeLiveSessions tracks the kojo turns running `opencode run` per
// (home, session). Only the first one prunes, and every turn waits for an
// in-flight prune before it starts OpenCode: rows are never deleted under a
// live run, and no run loads the session halfway through a prune.
var openCodeLiveSessions = struct {
	sync.Mutex
	m map[string]*openCodeLiveSession
}{m: map[string]*openCodeLiveSession{}}

type openCodeLiveSession struct {
	n     int
	prune sync.Mutex
}

// acquireOpenCodeLiveSession registers a turn on the session, runs prune
// when no other turn holds it, and returns once no prune is in flight.
// release must be called after the turn's process exited (idempotent).
func acquireOpenCodeLiveSession(home, sessionID string, prune func()) (release func()) {
	key := home + "\x00" + sessionID
	openCodeLiveSessions.Lock()
	e := openCodeLiveSessions.m[key]
	if e == nil {
		e = &openCodeLiveSession{}
		openCodeLiveSessions.m[key] = e
	}
	e.n++
	first := e.n == 1
	if first {
		// Taken before the registry unlocks (a fresh entry's mutex is
		// free), so no later turn can slip in ahead of the prune.
		e.prune.Lock()
	}
	openCodeLiveSessions.Unlock()

	if first {
		if prune != nil {
			prune()
		}
	} else {
		e.prune.Lock()
	}
	e.prune.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			openCodeLiveSessions.Lock()
			defer openCodeLiveSessions.Unlock()
			if e.n--; e.n == 0 {
				delete(openCodeLiveSessions.m, key)
			}
		})
	}
}
