package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// openUploadPath
// ---------------------------------------------------------------------------

func TestOpenUploadPath(t *testing.T) {
	// Redirect uploadDir to a per-test directory.
	tmpRoot := t.TempDir()
	uploadRoot := filepath.Join(tmpRoot, "kojo-upload")
	if err := os.MkdirAll(uploadRoot, 0o755); err != nil {
		t.Fatalf("mkdir upload root: %v", err)
	}
	orig := uploadDir
	uploadDir = uploadRoot
	t.Cleanup(func() { uploadDir = orig })

	t.Run("regular file inside upload dir is opened", func(t *testing.T) {
		p := filepath.Join(uploadRoot, "ok.txt")
		if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, size, kind := openUploadPath(p)
		if kind != "" {
			t.Fatalf("kind = %q, want empty", kind)
		}
		if f == nil {
			t.Fatal("expected non-nil file")
		}
		t.Cleanup(func() { f.Close() })
		if size != 5 {
			t.Errorf("size = %d, want 5", size)
		}
		// Read through the fd to confirm the data really comes from the
		// validated inode.
		buf := make([]byte, 5)
		if _, err := f.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != "hello" {
			t.Errorf("data = %q, want %q", buf, "hello")
		}
	})

	t.Run("empty path rejected", func(t *testing.T) {
		f, _, kind := openUploadPath("")
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrEmpty {
			t.Errorf("kind = %q, want %q", kind, uploadErrEmpty)
		}
	})

	t.Run("path outside upload dir rejected", func(t *testing.T) {
		outside := filepath.Join(tmpRoot, "outside.txt")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(outside)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		dir := filepath.Join(uploadRoot, "subdir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(dir)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrIsDir {
			t.Errorf("kind = %q, want %q", kind, uploadErrIsDir)
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		f, _, kind := openUploadPath(filepath.Join(uploadRoot, "does-not-exist"))
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrNotFound {
			t.Errorf("kind = %q, want %q", kind, uploadErrNotFound)
		}
	})

	t.Run("symlink to outside upload dir rejected", func(t *testing.T) {
		secret := filepath.Join(tmpRoot, "secret.txt")
		if err := os.WriteFile(secret, []byte("s3cr3t"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(uploadRoot, "evil-link")
		if err := os.Symlink(secret, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		f, _, kind := openUploadPath(link)
		if f != nil {
			f.Close()
			t.Error("symlink escape: expected rejection but got opened file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("user message never includes path", func(t *testing.T) {
		// Sanity check: the user-facing strings are fixed and contain
		// no path interpolation.
		for _, kind := range []string{
			uploadErrEmpty, uploadErrInvalid, uploadErrNotFound,
			uploadErrOutside, uploadErrIsDir, uploadErrNotFile,
			uploadErrOpenFail, uploadErrStatFail, uploadErrSwapped,
		} {
			msg := uploadPathUserMessage(kind)
			if strings.Contains(msg, "/") || strings.Contains(msg, `\`) {
				t.Errorf("kind %q produced message containing path separator: %q", kind, msg)
			}
			if msg == "" {
				t.Errorf("kind %q produced empty message", kind)
			}
		}
	})
}
