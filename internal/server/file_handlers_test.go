package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/peer"
)

func TestHandleUploadStreamsPrivateFileAtomically(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{peerID: &peer.Identity{DeviceID: "peer-a"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.handleUpload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Path   string `json:"path"`
		PeerID string `json:"peerId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PeerID != "peer-a" {
		t.Fatalf("peerId=%q", out.PeerID)
	}
	info, err := os.Stat(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" || bytes.HasPrefix([]byte(entry.Name()), []byte(".upload-")) {
			t.Fatalf("temporary upload remained: %s", entry.Name())
		}
	}
}

func TestValidateAttachmentsRejectsDifferentHolder(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	path := filepath.Join(uploadDir, "attachment.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	att := agent.MessageAttachment{Path: path, PeerID: "old-holder"}
	if got := validateAttachmentsForPeer([]agent.MessageAttachment{att}, "new-holder"); len(got) != 0 {
		t.Fatalf("different-holder attachment accepted: %#v", got)
	}
	if got := validateAttachmentsForPeer([]agent.MessageAttachment{att}, "old-holder"); len(got) != 1 {
		t.Fatalf("matching-holder attachment rejected: %#v", got)
	}
}
