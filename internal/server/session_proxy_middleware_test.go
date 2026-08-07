package server

import "testing"

func TestPeerProxyIncludesRemoteAttachmentThumbnail(t *testing.T) {
	if !isPeerProxyPath("/api/v1/files/thumb") {
		t.Fatal("remote attachment thumbnail route is not proxied to its holder")
	}
}
