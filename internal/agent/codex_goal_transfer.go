package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Goal rows live in a separate Codex database, not state_*.sqlite. Preserve
// accounting and continuation deferrals rather than recreating via goal/set.
type CodexGoalTransfer struct {
	Row      *CodexSQLiteRow `json:"row,omitempty"`
	Deferred bool            `json:"deferred,omitempty"`
}

func readCodexGoalTransfer(threadID string) (*CodexGoalTransfer, error) {
	p := filepath.Join(codexHome(), "goals_1.sqlite")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(p, true))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row, err := queryCodexSQLiteRowTx(tx, "SELECT * FROM thread_goals WHERE thread_id = ?", threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	var deferred bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM thread_goal_continuation_deferrals WHERE thread_id = ?)", threadID).Scan(&deferred); err != nil {
		return nil, err
	}
	return &CodexGoalTransfer{Row: row, Deferred: deferred}, nil
}

func validateGoalRow(row *CodexSQLiteRow, threadID string) error {
	if row == nil {
		return nil
	}
	if len(row.Columns) != len(row.Values) {
		return errors.New("invalid native goal row")
	}
	allowed := map[string]bool{"thread_id": true, "goal_id": true, "objective": true, "status": true, "token_budget": true, "tokens_used": true, "time_used_seconds": true, "created_at_ms": true, "updated_at_ms": true}
	seen := map[string]bool{}
	for i, c := range row.Columns {
		if !allowed[c] || seen[c] {
			return fmt.Errorf("unsupported native goal column %q", c)
		}
		seen[c] = true
		v := row.Values[i]
		switch c {
		case "thread_id":
			if v.Type != "text" || v.Text != threadID {
				return errors.New("native goal thread mismatch")
			}
		case "goal_id":
			if v.Type != "text" || v.Text == "" {
				return errors.New("invalid native goal id")
			}
		case "objective":
			if v.Type != "text" {
				return errors.New("invalid goal objective")
			}
			if err := (&GoalRequest{Action: "start", Objective: v.Text}).Validate(); err != nil {
				return err
			}
		case "status":
			if v.Type != "text" {
				return errors.New("invalid goal status")
			}
			switch v.Text {
			case "active", "paused", "blocked", "usage_limited", "budget_limited", "complete":
			default:
				return errors.New("unknown native goal status")
			}
		case "token_budget":
			if v.Type == "null" {
				continue
			}
			if v.Type != "int" || v.Int <= 0 {
				return errors.New("invalid native goal budget")
			}
		default:
			if v.Type != "int" || v.Int < 0 {
				return errors.New("invalid native goal accounting")
			}
		}
	}
	if len(seen) != len(allowed) {
		return errors.New("incomplete native goal row")
	}
	return nil
}

// Stage uses a transaction and compensating rollback, matching the session
// staging contract. Fail closed when target Codex has not initialized a
// compatible goal database; do not invent a CLI-owned schema on its behalf.
func stageCodexGoals(root string, transfer *CodexSessionTransfer) (func(), func(), error) {
	hasGoals := false
	for _, th := range transfer.Threads {
		if th.NativeGoal != nil {
			hasGoals = true
			if err := validateGoalRow(th.NativeGoal.Row, th.ThreadID); err != nil {
				return nil, nil, err
			}
		}
	}
	p := filepath.Join(root, "goals_1.sqlite")
	if _, err := os.Stat(p); err != nil {
		if !hasGoals && os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("native goal transfer requires initialized Codex goals_1.sqlite on target: %w", err)
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(p, false))
	if err != nil {
		return nil, nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	defer tx.Rollback()
	backups := map[string]*CodexGoalTransfer{}
	for _, th := range transfer.Threads {
		row, e := queryCodexSQLiteRowTx(tx, "SELECT * FROM thread_goals WHERE thread_id = ?", th.ThreadID)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			db.Close()
			return nil, nil, e
		}
		var d bool
		if e = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM thread_goal_continuation_deferrals WHERE thread_id = ?)", th.ThreadID).Scan(&d); e != nil {
			db.Close()
			return nil, nil, e
		}
		backups[th.ThreadID] = &CodexGoalTransfer{Row: row, Deferred: d}
		if e = replaceGoalRows(tx, th.ThreadID, th.NativeGoal); e != nil {
			db.Close()
			return nil, nil, e
		}
	}
	if err = tx.Commit(); err != nil {
		db.Close()
		return nil, nil, err
	}
	done := false
	commit := func() {
		if !done {
			done = true
			db.Close()
		}
	}
	rollback := func() {
		if done {
			return
		}
		done = true
		defer db.Close()
		tx, e := db.Begin()
		if e != nil {
			return
		}
		defer tx.Rollback()
		for id, b := range backups {
			if e = replaceGoalRows(tx, id, b); e != nil {
				return
			}
		}
		_ = tx.Commit()
	}
	return commit, rollback, nil
}
func replaceGoalRows(tx *sql.Tx, id string, g *CodexGoalTransfer) error {
	if _, err := tx.Exec("DELETE FROM thread_goal_continuation_deferrals WHERE thread_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM thread_goals WHERE thread_id = ?", id); err != nil {
		return err
	}
	if g == nil || g.Row == nil {
		return nil
	}
	if err := insertCodexSQLiteRow(tx, "thread_goals", *g.Row); err != nil {
		return err
	}
	if g.Deferred {
		_, err := tx.Exec("INSERT INTO thread_goal_continuation_deferrals(thread_id) VALUES (?)", id)
		return err
	}
	return nil
}

func HasCodexGoals(agentID string) bool {
	entries, err := os.ReadDir(codexThreadRefDir(agentID))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !validCodexThreadRefName(e.Name()) {
			continue
		}
		ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(agentID), e.Name()))
		if err == nil && ref.Goal != nil && ref.Goal.State != nil {
			return true
		}
	}
	return false
}

func clearNativeGoalRows(ids map[string]struct{}) error {
	p := filepath.Join(codexHome(), "goals_1.sqlite")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(p, false))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id := range ids {
		if _, err = tx.Exec("DELETE FROM thread_goal_continuation_deferrals WHERE thread_id = ?", id); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM thread_goals WHERE thread_id = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
