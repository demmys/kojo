package server

import (
	"context"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/slackbot"
	"github.com/loppo-llc/kojo/internal/store"
)

func TestApplySlackOwnershipToSyncRecordStripsPeerCopy(t *testing.T) {
	srv := newQueueTestServer(t) // no slackHub: peer-only ownership rules
	rec := &store.AgentRecord{
		ID:        "ag_peer_slack_strip",
		Version:   4,
		UpdatedAt: 10,
		ETag:      "source-etag",
		Settings: map[string]any{
			"model":    "opus",
			"SLACKBOT": map[string]any{"enabled": true},
		},
	}
	if err := srv.applySlackOwnershipToSyncRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Settings["SLACKBOT"]; ok {
		t.Fatal("peer retained Hub-owned Slack setting")
	}
	if rec.Settings["model"] != "opus" {
		t.Fatalf("unrelated setting changed: %#v", rec.Settings)
	}
	if rec.Version != 5 || rec.UpdatedAt <= 10 || rec.ETag != "" {
		t.Fatalf("metadata was not refreshed: version=%d updated=%d etag=%q",
			rec.Version, rec.UpdatedAt, rec.ETag)
	}
}

func TestApplySlackOwnershipToSyncRecordPreservesHubValue(t *testing.T) {
	srv := newQueueTestServer(t)
	srv.slackHub = &slackbot.Hub{} // ownership marker; no bot is started
	ctx := context.Background()
	current, err := srv.agents.Store().InsertAgent(ctx, &store.AgentRecord{
		ID:      "ag_hub_slack_preserve",
		Name:    "hub-copy",
		Version: 8,
		Settings: map[string]any{
			"slackBot": map[string]any{"enabled": true, "threadReplies": true},
		},
	}, store.AgentInsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	incoming := &store.AgentRecord{
		ID:        current.ID,
		Version:   3,
		UpdatedAt: 10,
		ETag:      "holder-etag",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": false},
		},
	}
	if err := srv.applySlackOwnershipToSyncRecord(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	got, ok := incoming.Settings["slackBot"].(map[string]any)
	if !ok || got["enabled"] != true || got["threadReplies"] != true {
		t.Fatalf("Hub Slack setting not preserved: %#v", incoming.Settings["slackBot"])
	}
	if incoming.Settings["model"] != "codex" {
		t.Fatalf("holder-owned setting changed: %#v", incoming.Settings)
	}
	wantVersion := current.Version + 1
	if wantVersion < 4 {
		wantVersion = 4 // incoming version (3) + 1
	}
	if incoming.Version != wantVersion || incoming.ETag != "" {
		t.Fatalf("merged metadata = version %d etag %q, want version %d and recompute",
			incoming.Version, incoming.ETag, wantVersion)
	}
	if err := srv.agents.Store().SyncAgentFromPeer(ctx, store.AgentSyncPayload{Agent: incoming}); err != nil {
		t.Fatal(err)
	}
	persisted, err := srv.agents.Store().GetAgent(ctx, incoming.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ETag == "" || persisted.ETag == "holder-etag" {
		t.Fatalf("persisted merged ETag was not recomputed: %q", persisted.ETag)
	}
	retry := &store.AgentRecord{
		ID:        current.ID,
		Name:      incoming.Name,
		Seq:       persisted.Seq,
		Version:   3,
		UpdatedAt: 10,
		ETag:      "holder-etag",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": false},
		},
	}
	if err := srv.applySlackOwnershipToSyncRecord(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if retry.Version != persisted.Version || retry.UpdatedAt != persisted.UpdatedAt || retry.ETag != persisted.ETag {
		t.Fatalf("retry metadata churned: got v=%d updated=%d etag=%q; want v=%d updated=%d etag=%q",
			retry.Version, retry.UpdatedAt, retry.ETag,
			persisted.Version, persisted.UpdatedAt, persisted.ETag)
	}
}

func TestApplySlackOwnershipDoesNotDiscardNewerSourceMetadata(t *testing.T) {
	srv := newQueueTestServer(t)
	srv.slackHub = &slackbot.Hub{}
	ctx := context.Background()
	current, err := srv.agents.Store().InsertAgent(ctx, &store.AgentRecord{
		ID:   "ag_hub_slack_newer_source",
		Name: "same-content",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": true},
		},
	}, store.AgentInsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	incoming := &store.AgentRecord{
		ID:          current.ID,
		Name:        current.Name,
		PersonaRef:  current.PersonaRef,
		WorkspaceID: current.WorkspaceID,
		Seq:         current.Seq,
		Version:     current.Version + 5,
		UpdatedAt:   current.UpdatedAt + int64((24*time.Hour)/time.Millisecond),
		ETag:        "newer-holder-etag",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": false},
		},
	}

	if err := srv.applySlackOwnershipToSyncRecord(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	if incoming.Version != current.Version+6 {
		t.Fatalf("version = %d, want %d", incoming.Version, current.Version+6)
	}
	if incoming.ETag != "" {
		t.Fatalf("etag = %q, want recompute", incoming.ETag)
	}
	wantUpdatedAt := current.UpdatedAt + int64((24*time.Hour)/time.Millisecond)
	if incoming.UpdatedAt != wantUpdatedAt {
		t.Fatalf("updatedAt = %d, want preserved future source timestamp %d", incoming.UpdatedAt, wantUpdatedAt)
	}
}

func TestApplySlackOwnershipDoesNotDiscardNewerEqualVersionTimestamp(t *testing.T) {
	srv := newQueueTestServer(t)
	srv.slackHub = &slackbot.Hub{}
	ctx := context.Background()
	current, err := srv.agents.Store().InsertAgent(ctx, &store.AgentRecord{
		ID:   "ag_hub_slack_newer_equal_version",
		Name: "same-content",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": true},
		},
	}, store.AgentInsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantUpdatedAt := current.UpdatedAt + int64((24*time.Hour)/time.Millisecond)
	incoming := &store.AgentRecord{
		ID:          current.ID,
		Name:        current.Name,
		PersonaRef:  current.PersonaRef,
		WorkspaceID: current.WorkspaceID,
		Seq:         current.Seq,
		Version:     current.Version,
		UpdatedAt:   wantUpdatedAt,
		ETag:        "newer-holder-etag",
		Settings: map[string]any{
			"model":    "codex",
			"slackBot": map[string]any{"enabled": false},
		},
	}

	if err := srv.applySlackOwnershipToSyncRecord(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	if incoming.Version != current.Version+1 {
		t.Fatalf("version = %d, want %d", incoming.Version, current.Version+1)
	}
	if incoming.UpdatedAt != wantUpdatedAt {
		t.Fatalf("updatedAt = %d, want %d", incoming.UpdatedAt, wantUpdatedAt)
	}
	if incoming.ETag != "" {
		t.Fatalf("etag = %q, want recompute", incoming.ETag)
	}
}
