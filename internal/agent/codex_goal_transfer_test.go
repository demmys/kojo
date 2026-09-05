package agent

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

const testGoalDDL = `CREATE TABLE thread_goals(thread_id TEXT PRIMARY KEY,goal_id TEXT NOT NULL,objective TEXT NOT NULL,status TEXT NOT NULL,token_budget INTEGER,tokens_used INTEGER NOT NULL,time_used_seconds INTEGER NOT NULL,created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL);
CREATE TABLE thread_goal_continuation_deferrals(thread_id TEXT PRIMARY KEY REFERENCES thread_goals(thread_id));`

func TestGoalTransferPreservesUsageAndRollback(t *testing.T) {
	_, root := setupCodexTransferTest(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "goals_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(testGoalDDL); err != nil {
		t.Fatal(err)
	}
	id := "019e7cc9-dd5e-7971-b654-7840c683879e"
	if _, err = db.Exec("INSERT INTO thread_goals VALUES (?,?,?,?,?,?,?,?,?)", id, "goal-1", "fix it", "paused", 10000, 450, 7, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO thread_goal_continuation_deferrals VALUES (?)", id); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readCodexGoalTransfer(id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil || !snapshot.Deferred {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if _, err = db.Exec("UPDATE thread_goals SET tokens_used=900"); err != nil {
		t.Fatal(err)
	}
	transfer := &CodexSessionTransfer{Threads: []CodexThreadTransfer{{ThreadID: id, NativeGoal: snapshot}}}
	commit, rollback, err := stageCodexGoals(root, transfer)
	if err != nil {
		t.Fatal(err)
	}
	var used int
	if err = db.QueryRow("SELECT tokens_used FROM thread_goals").Scan(&used); err != nil || used != 450 {
		t.Fatalf("used=%d err=%v", used, err)
	}
	rollback()
	commit()
	if err = db.QueryRow("SELECT tokens_used FROM thread_goals").Scan(&used); err != nil || used != 900 {
		t.Fatalf("rollback used=%d err=%v", used, err)
	}
	commit, _, err = stageCodexGoals(root, transfer)
	if err != nil {
		t.Fatal(err)
	}
	commit()
	if err = db.QueryRow("SELECT tokens_used FROM thread_goals").Scan(&used); err != nil || used != 450 {
		t.Fatalf("commit used=%d err=%v", used, err)
	}
}
func TestGoalTransferRejectsThreadMismatch(t *testing.T) {
	row := &CodexSQLiteRow{Columns: []string{"thread_id"}, Values: []CodexSQLiteValue{{Type: "text", Text: "other"}}}
	if validateGoalRow(row, "target") == nil {
		t.Fatal("accepted cross-thread goal")
	}
}
