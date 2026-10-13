package server

import (
	"testing"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func TestPeerRole_Hub(t *testing.T) {
	s := &Server{peerID: &peer.Identity{DeviceID: "hub-id"}}
	if got := s.toPeerResponse(&store.PeerRecord{DeviceID: "hub-id"}).Role; got != peerRoleHub {
		t.Fatalf("self row on Hub: role=%q, want hub", got)
	}
	if got := s.toPeerResponse(&store.PeerRecord{DeviceID: "other"}).Role; got != peerRolePeer {
		t.Fatalf("other row on Hub: role=%q, want peer", got)
	}
}

func TestPeerRole_PeerOnly(t *testing.T) {
	s := &Server{peerOnly: true, peerID: &peer.Identity{DeviceID: "self-id"}}
	if got := s.peerRole("hub-id"); got != "" {
		t.Fatalf("unresolved Hub: role=%q, want empty", got)
	}
	hub := ""
	s.SetHubDeviceIDFunc(func() string { return hub })
	if got := s.peerRole("hub-id"); got != "" {
		t.Fatalf("resolver returning empty: role=%q, want empty", got)
	}
	hub = "hub-id"
	if got := s.peerRole("hub-id"); got != peerRoleHub {
		t.Fatalf("Hub row: role=%q, want hub", got)
	}
	if got := s.peerRole("self-id"); got != peerRolePeer {
		t.Fatalf("self row on peer: role=%q, want peer", got)
	}
}
