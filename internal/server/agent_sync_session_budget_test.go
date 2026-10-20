package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/store"
)

func TestExactAgentSyncNonSessionJSONSize_MatchesEncodingJSON(t *testing.T) {
	emptyCredentials := []*agent.Credential{}
	customKey := "sk-unsloth-size"
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", OpID: "op", Agent: &store.AgentRecord{ID: "ag_x"},
		Messages: []*store.MessageRecord{
			{ID: "m1", AgentID: "ag_x", Role: "user", Content: "hello"},
			nil,
		},
		AgentToken: "token", SinceMessageSeq: 7,
		Credentials:     &emptyCredentials,
		CustomAPIKey:    &customKey,
		DegradedFlushes: []string{"memory_flush"},
		TransferSkips:   []agent.SkippedSessionFile{{Path: "old.jsonl", Reason: "capacity", SizeBytes: 42}},
	}
	want, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exactAgentSyncNonSessionJSONSize(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(len(want)) {
		t.Fatalf("size=%d, want exact encoding/json size %d", got, len(want))
	}
}

func TestExactAgentSyncNonSessionJSONSize_NilCredentialSliceMatchesEncodingJSON(t *testing.T) {
	var nilCredentials []*agent.Credential
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", OpID: "op", Agent: &store.AgentRecord{ID: "ag_x"},
		Credentials: &nilCredentials,
	}
	want, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exactAgentSyncNonSessionJSONSize(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(len(want)) {
		t.Fatalf("size=%d, want exact encoding/json size %d (%s)", got, len(want), want)
	}
}

func TestFitAgentSyncSessions_KeepsNewestThatFitAndAllHistory(t *testing.T) {
	history := &store.MessageRecord{ID: "m1", AgentID: "ag_x", Role: "user", Content: "canonical history"}
	newest := claudeSessionWire{SessionID: "newest", ContentB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("n", 4096)))}
	older := claudeSessionWire{SessionID: "older", ContentB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("o", 4096)))}

	// Leave enough exact JSON room for newest plus the capacity-skip record,
	// but not for the older rollout body.
	one := &peerAgentSyncRequest{
		SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
		Messages: []*store.MessageRecord{history}, ClaudeSessions: []claudeSessionWire{newest},
	}
	raw, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
		Messages: []*store.MessageRecord{history}, ClaudeSessions: []claudeSessionWire{newest, older},
	}
	kept, skipped, err := fitAgentSyncSessions(req, int64(len(raw)+1024))
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || skipped != 1 {
		t.Fatalf("kept=%d skipped=%d, want 1/1", kept, skipped)
	}
	if len(req.ClaudeSessions) != 1 || req.ClaudeSessions[0].SessionID != "newest" {
		t.Fatalf("sessions=%+v, want newest only", req.ClaudeSessions)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "canonical history" {
		t.Fatalf("history was trimmed: %+v", req.Messages)
	}
	if len(req.TransferSkips) != 1 || req.TransferSkips[0].Path != "older.jsonl" || req.TransferSkips[0].Reason != "capacity" {
		t.Fatalf("transfer skips=%+v", req.TransferSkips)
	}
	finalRaw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(finalRaw)) > int64(len(raw)+1024) {
		t.Fatalf("selected payload exceeds cap: %d > %d", len(finalRaw), len(raw)+1024)
	}
}

func TestFitAgentSyncSessions_HistoryOverCapDropsOnlySessions(t *testing.T) {
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
		Messages: []*store.MessageRecord{{
			ID: "m1", AgentID: "ag_x", Role: "user", Content: strings.Repeat("h", 4096),
		}},
		ClaudeSessions: []claudeSessionWire{{
			SessionID: "active", ContentB64: base64.StdEncoding.EncodeToString([]byte("session")),
		}},
	}
	kept, skipped, err := fitAgentSyncSessions(req, 512)
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 || skipped != 1 || len(req.ClaudeSessions) != 0 {
		t.Fatalf("kept=%d skipped=%d sessions=%+v", kept, skipped, req.ClaudeSessions)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 4096 {
		t.Fatalf("history was trimmed: %+v", req.Messages)
	}
}

func TestFitAgentSyncSessions_RejectsUnfinishedGoalWhenOverCap(t *testing.T) {
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
		CodexSession: &codexSessionWire{Threads: []codexThreadWire{{
			Goal:    &agent.GoalBinding{State: &agent.CodexGoal{Status: "paused"}},
			RefName: "goal.json", ThreadID: "thread-goal",
			RolloutRelPath:    "sessions/goal.jsonl",
			RolloutContentB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("g", 4096))),
		}}},
	}
	if _, _, err := fitAgentSyncSessions(req, 512); err == nil || !strings.Contains(err.Error(), "unfinished native goal") {
		t.Fatalf("err=%v, want unfinished-goal capacity rejection", err)
	}
}

func TestFitAgentSyncSessions_RejectsUnfinishedNativeGoalRowWhenOverCap(t *testing.T) {
	columns := []string{"thread_id", "goal_id", "objective", "status", "token_budget", "tokens_used", "time_used_seconds", "created_at_ms", "updated_at_ms"}
	values := []agent.CodexSQLiteValue{
		{Type: "text", Text: "thread-goal"}, {Type: "text", Text: "goal-1"}, {Type: "text", Text: "keep going"},
		{Type: "text", Text: "active"}, {Type: "null"}, {Type: "int", Int: 1}, {Type: "int", Int: 1}, {Type: "int", Int: 1}, {Type: "int", Int: 1},
	}
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
		CodexSession: &codexSessionWire{Threads: []codexThreadWire{{
			NativeGoal: &agent.CodexGoalTransfer{Row: &agent.CodexSQLiteRow{Columns: columns, Values: values}},
			RefName:    "goal.json", ThreadID: "thread-goal", RolloutRelPath: "sessions/goal.jsonl",
			RolloutContentB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("g", 4096))),
		}}},
	}
	if _, _, err := fitAgentSyncSessions(req, 512); err == nil || !strings.Contains(err.Error(), "unfinished native goal") {
		t.Fatalf("err=%v, want unfinished-goal capacity rejection", err)
	}
}

func geminiWireForTest(body string) *geminiSessionWire {
	return &geminiSessionWire{
		Refs: []geminiSessionRefWire{
			{SessionID: "6b0f37f5-5d76-47ea-9d50-3a3add9c3936"},
			{Name: "key-0c1d2e3f-4a5b-4c6d-8e7f-901234567890", SessionID: "6b0f37f5-5d76-47ea-9d50-3a3add9c3936"},
		},
		Files: []geminiSessionFileWire{{
			RelPath:    "session-2026-10-15T09-13-6b0f37f5.jsonl",
			ContentB64: base64.StdEncoding.EncodeToString([]byte(body)),
		}},
	}
}

func TestFitAgentSyncSessions_GeminiIsOneArtifact(t *testing.T) {
	base := func() *peerAgentSyncRequest {
		return &peerAgentSyncRequest{
			SourceDeviceID: "src", Agent: &store.AgentRecord{ID: "ag_x"},
			Messages:      []*store.MessageRecord{{ID: "m1", AgentID: "ag_x", Role: "user", Content: "hi"}},
			GeminiSession: geminiWireForTest(strings.Repeat("g", 2048)),
		}
	}
	full, err := json.Marshal(base())
	if err != nil {
		t.Fatal(err)
	}

	// Exactly at the cap: kept whole, nothing skipped, exact size.
	req := base()
	kept, skipped, err := fitAgentSyncSessions(req, int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 || skipped != 0 || req.GeminiSession == nil || len(req.GeminiSession.Refs) != 2 || len(req.GeminiSession.Files) != 1 {
		t.Fatalf("kept=%d skipped=%d gemini=%+v", kept, skipped, req.GeminiSession)
	}
	if got, _ := json.Marshal(req); len(got) != len(full) {
		t.Fatalf("size changed: %d != %d", len(got), len(full))
	}

	// One byte short: the whole set (refs + files) is skipped together.
	req = base()
	kept, skipped, err = fitAgentSyncSessions(req, int64(len(full)-1))
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 || skipped != 1 || req.GeminiSession != nil {
		t.Fatalf("kept=%d skipped=%d gemini=%+v", kept, skipped, req.GeminiSession)
	}
	if len(req.TransferSkips) != 1 || req.TransferSkips[0].Path != "gemini-session/6b0f37f5-5d76-47ea-9d50-3a3add9c3936" ||
		req.TransferSkips[0].SizeBytes < 2048 || req.TransferSkips[0].SizeBytes > 2050 { // base64 DecodedLen upper bound
		t.Fatalf("skips=%+v", req.TransferSkips)
	}
	if got, _ := json.Marshal(req); int64(len(got)) > int64(len(full)-1) {
		t.Fatalf("over cap: %d", len(got))
	}
}

func TestSplitAgentSyncIntoChunks_GeminiRidesBegin(t *testing.T) {
	payload := &peerAgentSyncRequest{
		SourceDeviceID: "src", OpID: "op", Agent: &store.AgentRecord{ID: "ag_x"},
		GeminiSession: geminiWireForTest(strings.Repeat("g", 4096)),
	}
	est, err := estimateAgentSyncRawSize(payload)
	if err != nil {
		t.Fatal(err)
	}
	if est < 4096 {
		t.Fatalf("estimate %d ignores gemini files", est)
	}
	begin, _, err := splitAgentSyncIntoChunks(payload, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if begin.GeminiSession == nil || len(begin.GeminiSession.Files) != 1 || len(begin.GeminiSession.Refs) != 2 {
		t.Fatalf("begin gemini = %+v", begin.GeminiSession)
	}
	raw, err := json.Marshal(begin)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var back peerAgentSyncChunkedBeginRequest
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("strict decode of begin: %v", err)
	}
	if back.GeminiSession == nil || back.GeminiSession.Refs[1].Name != payload.GeminiSession.Refs[1].Name {
		t.Fatalf("round trip lost gemini: %+v", back.GeminiSession)
	}
}
