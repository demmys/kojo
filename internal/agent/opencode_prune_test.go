package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const pruneTestSession = "ses_e9ae12a0cffewXnAKGhTpEzJl8"

// plantOpenCodeHistory builds the OpenCode message/part tables (with the
// real FK layout) and fills pruneTestSession with turns. Each turn is a
// user message, `steps` tool-only assistant steps and a final text answer.
func plantOpenCodeHistory(t *testing.T, home string, turns, steps, toolBytes int) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(openCodeDataDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(openCodeDBPath(home), false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, title TEXT NOT NULL, time_updated INTEGER NOT NULL, revert TEXT)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL,
			FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO session VALUES (?, '/x', 't', 1, NULL)`, pruneTestSession); err != nil {
		t.Fatal(err)
	}
	clock := 0
	addMsg := func(id, data string) {
		clock++
		if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?)`, id, pruneTestSession, clock, clock, data); err != nil {
			t.Fatal(err)
		}
	}
	addPart := func(mid, id, data string) {
		clock++
		if _, err := db.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?)`, id, mid, pruneTestSession, clock, clock, data); err != nil {
			t.Fatal(err)
		}
	}
	blob := strings.Repeat("x", toolBytes)
	for tu := 0; tu < turns; tu++ {
		u := fmt.Sprintf("msg_t%02d_user", tu)
		addMsg(u, `{"role":"user","time":{"created":1}}`)
		addPart(u, u+"_p", `{"type":"text","text":"ask"}`)
		for s := 0; s < steps; s++ {
			a := fmt.Sprintf("msg_t%02d_step%d", tu, s)
			addMsg(a, `{"role":"assistant","parentID":"`+u+`","time":{"created":1,"completed":2},"finish":"tool-calls"}`)
			addPart(a, a+"_a", `{"type":"step-start"}`)
			addPart(a, a+"_b", `{"type":"reasoning","text":"","metadata":{"google":{"thoughtSignature":"sig"}}}`)
			addPart(a, a+"_c", `{"type":"tool","tool":"bash","state":{"status":"completed","output":"`+blob+`"}}`)
			addPart(a, a+"_d", `{"type":"step-finish","reason":"tool-calls"}`)
		}
		f := fmt.Sprintf("msg_t%02d_final", tu)
		addMsg(f, `{"role":"assistant","parentID":"`+u+`","time":{"created":1,"completed":2},"finish":"stop"}`)
		addPart(f, f+"_a", `{"type":"text","text":"answer"}`)
	}
	return db
}

func openCodeMessageIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM message ORDER BY time_created`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestPruneOpenCodeSession_DropsOldToolSteps(t *testing.T) {
	home := t.TempDir()
	db := plantOpenCodeHistory(t, home, 5, 2, 20<<10)

	n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession)
	if err != nil {
		t.Fatal(err)
	}
	// Turns 0 and 1 are older than the last 3 user turns: 2 steps each.
	if n != 4 {
		t.Fatalf("removed %d messages, want 4", n)
	}
	ids := strings.Join(openCodeMessageIDs(t, db), ",")
	for _, gone := range []string{"msg_t00_step0", "msg_t00_step1", "msg_t01_step0", "msg_t01_step1"} {
		if strings.Contains(ids, gone) {
			t.Errorf("%s survived: %s", gone, ids)
		}
	}
	for _, kept := range []string{"msg_t00_user", "msg_t00_final", "msg_t01_final", "msg_t02_step0", "msg_t04_step1", "msg_t04_final"} {
		if !strings.Contains(ids, kept) {
			t.Errorf("%s was removed: %s", kept, ids)
		}
	}
	var orphans int
	if err := db.QueryRow(`SELECT COUNT(*) FROM part WHERE message_id NOT IN (SELECT id FROM message)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphan parts left", orphans)
	}

	// Second run: nothing left to prune before the window.
	if n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0, nil", n, err)
	}
}

func TestPruneOpenCodeSession_WaitsForThreshold(t *testing.T) {
	home := t.TempDir()
	db := plantOpenCodeHistory(t, home, 5, 2, 100)
	before := len(openCodeMessageIDs(t, db))
	if n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession); err != nil || n != 0 {
		t.Fatalf("prune below threshold = %d, %v; want 0, nil", n, err)
	}
	if after := len(openCodeMessageIDs(t, db)); after != before {
		t.Fatalf("messages changed below threshold: %d -> %d", before, after)
	}
}

func TestPruneOpenCodeSession_KeepsUnsafeMessages(t *testing.T) {
	home := t.TempDir()
	db := plantOpenCodeHistory(t, home, 5, 2, 40<<10)
	// t00_step0 also said something; t00_step1 has a still-running tool;
	// t01_step0 never completed; t01_step1 is a compaction summary.
	for _, q := range []string{
		`INSERT INTO part VALUES ('p_text', 'msg_t00_step0', '` + pruneTestSession + `', 999, 999, '{"type":"text","text":"note"}')`,
		`UPDATE part SET data = '{"type":"tool","state":{"status":"running"}}' WHERE id = 'msg_t00_step1_c'`,
		`UPDATE message SET data = '{"role":"assistant","time":{"created":1},"finish":"tool-calls"}' WHERE id = 'msg_t01_step0'`,
		`UPDATE message SET data = '{"role":"assistant","time":{"created":1,"completed":2},"finish":"tool-calls","summary":true}' WHERE id = 'msg_t01_step1'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession); err != nil || n != 0 {
		t.Fatalf("prune = %d, %v; want 0, nil", n, err)
	}
}

func TestPruneOpenCodeSession_KeepsCompactionTail(t *testing.T) {
	home := t.TempDir()
	db := plantOpenCodeHistory(t, home, 5, 2, 40<<10)
	// A compaction in turn 2 retained the tail from t00_step1 on.
	if _, err := db.Exec(`INSERT INTO part VALUES ('p_comp', 'msg_t02_user', '` + pruneTestSession + `', 999, 999, '{"type":"compaction","auto":true,"tail_start_id":"msg_t00_step1"}')`); err != nil {
		t.Fatal(err)
	}
	n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("removed %d messages, want 3", n)
	}
	if ids := strings.Join(openCodeMessageIDs(t, db), ","); !strings.Contains(ids, "msg_t00_step1") {
		t.Fatalf("compaction tail start was removed: %s", ids)
	}
}

func TestPruneOpenCodeSession_SkipsWhileARunIsLive(t *testing.T) {
	home := t.TempDir()
	db := plantOpenCodeHistory(t, home, 5, 2, 40<<10)
	if _, err := db.Exec(`UPDATE message SET data = '{"role":"assistant","time":{"created":1}}' WHERE id = 'msg_t04_final'`); err != nil {
		t.Fatal(err)
	}
	if n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession); err != nil || n != 0 {
		t.Fatalf("prune with a live run = %d, %v; want 0, nil", n, err)
	}
}

func TestPruneOpenCodeSession_SkipsUnknownLayouts(t *testing.T) {
	for name, q := range map[string]string{
		"revert": `UPDATE session SET revert = '{"messageID":"msg_t00_user"}'`,
		"v2":     `INSERT INTO session_message VALUES ('sm', '` + pruneTestSession + `', 'user', 1, 1, 1, '{}')`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			db := plantOpenCodeHistory(t, home, 5, 2, 40<<10)
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
			if n, err := pruneOpenCodeSession(context.Background(), home, pruneTestSession); err != nil || n != 0 {
				t.Fatalf("prune = %d, %v; want 0, nil", n, err)
			}
		})
	}
}

func TestPruneOpenCodeSession_NoDB(t *testing.T) {
	if n, err := pruneOpenCodeSession(context.Background(), t.TempDir(), pruneTestSession); err != nil || n != 0 {
		t.Fatalf("prune without a DB = %d, %v; want 0, nil", n, err)
	}
}

func TestAcquireOpenCodeLiveSession(t *testing.T) {
	pruned := 0
	prune := func() { pruned++ }

	rel1 := acquireOpenCodeLiveSession("/h", "ses_a", prune)
	rel2 := acquireOpenCodeLiveSession("/h", "ses_a", prune)
	if pruned != 1 {
		t.Fatalf("pruned %d times with two live turns, want 1", pruned)
	}
	rel2()
	rel2()
	rel3 := acquireOpenCodeLiveSession("/h", "ses_b", prune)
	rel3()
	if pruned != 2 {
		t.Fatalf("other session did not prune: %d", pruned)
	}
	rel4 := acquireOpenCodeLiveSession("/h", "ses_a", prune)
	rel4()
	if pruned != 2 {
		t.Fatal("double release freed the live first turn")
	}
	rel1()
	acquireOpenCodeLiveSession("/h", "ses_a", prune)()
	if pruned != 3 {
		t.Fatal("session still held after every release")
	}
}

func TestAcquireOpenCodeLiveSession_WaitsForPrune(t *testing.T) {
	entered, unblock := make(chan struct{}), make(chan struct{})
	go acquireOpenCodeLiveSession("/h", "ses_w", func() {
		close(entered)
		<-unblock
	})
	<-entered
	got := make(chan struct{})
	go func() {
		acquireOpenCodeLiveSession("/h", "ses_w", nil)()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second turn started while the prune was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(unblock)
	<-got
}
