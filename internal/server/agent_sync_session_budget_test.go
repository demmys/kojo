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
	req := &peerAgentSyncRequest{
		SourceDeviceID: "src", OpID: "op", Agent: &store.AgentRecord{ID: "ag_x"},
		Messages: []*store.MessageRecord{
			{ID: "m1", AgentID: "ag_x", Role: "user", Content: "hello"},
			nil,
		},
		AgentToken: "token", SinceMessageSeq: 7,
		Credentials:     &emptyCredentials,
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
