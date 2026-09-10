package server

import (
	"context"
	"testing"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func seedServerHandoffBlob(t *testing.T, st *store.Store, uri, homePeer, sha string) {
	t.Helper()
	if _, err := st.InsertOrReplaceBlobRef(context.Background(), &store.BlobRefRecord{
		URI:      uri,
		Scope:    "global",
		HomePeer: homePeer,
		Size:     10,
		SHA256:   sha,
	}, store.BlobRefInsertOptions{}); err != nil {
		t.Fatalf("seed blob %s: %v", uri, err)
	}
}

func TestRunHandoffOp_OnlyAvatarBlobMoves(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	ctx := context.Background()
	st := srv.agents.Store()
	srv.peerID = &peer.Identity{DeviceID: "peer-src"}
	if _, err := st.UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "peer-tgt",
		Name:     "target",
		URL:      "http://target.example",
		Status:   store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("target peer: %v", err)
	}

	prefix := "kojo://global/agents/ag_x/"
	avatarURI := prefix + "avatar.png"
	attachURI := prefix + "attach/m_1/chart.png"
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_x", Name: "x"}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	seedServerHandoffBlob(t, st, avatarURI, "peer-src", "aaa")
	seedServerHandoffBlob(t, st, attachURI, "peer-src", "bbb")
	if _, err := st.AcquireAgentLock(ctx, "ag_x", "peer-src", store.NowMillis(), 60_000); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	begin, err := srv.runHandoffOp(ctx, "ag_x", "begin", "peer-tgt")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if len(begin.Blobs) != 1 || begin.Blobs[0].URI != avatarURI {
		t.Fatalf("begin blobs = %+v, want only %s", begin.Blobs, avatarURI)
	}
	avatar, _ := st.GetBlobRef(ctx, avatarURI)
	if !avatar.HandoffPending {
		t.Fatalf("avatar not marked handoff_pending after begin")
	}
	attach, _ := st.GetBlobRef(ctx, attachURI)
	if attach.HandoffPending {
		t.Fatalf("attach was marked handoff_pending; must stay out of handoff")
	}

	complete, err := srv.runHandoffOp(ctx, "ag_x", "complete", "peer-tgt")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(complete.Blobs) != 1 || complete.Blobs[0].URI != avatarURI {
		t.Fatalf("complete blobs = %+v, want only %s", complete.Blobs, avatarURI)
	}
	avatar, _ = st.GetBlobRef(ctx, avatarURI)
	if avatar.HomePeer != "peer-tgt" || avatar.HandoffPending {
		t.Fatalf("avatar state = home %q pending %v, want peer-tgt false",
			avatar.HomePeer, avatar.HandoffPending)
	}
	attach, _ = st.GetBlobRef(ctx, attachURI)
	if attach.HomePeer != "peer-src" || attach.HandoffPending {
		t.Fatalf("attach state changed = home %q pending %v, want peer-src false",
			attach.HomePeer, attach.HandoffPending)
	}
}
