package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTempAvatarPath(t *testing.T) {
	// Create a temp directory that mimics kojo-avatar-* structure
	tmpBase := os.TempDir()
	avatarDir, err := os.MkdirTemp(tmpBase, "kojo-avatar-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(avatarDir)

	// Create a test image file
	testFile := filepath.Join(avatarDir, "avatar.png")
	if err := os.WriteFile(testFile, []byte("fake-png"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid temp avatar path", func(t *testing.T) {
		absPath, err := ValidateTempAvatarPath(testFile)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should resolve to an absolute path
		if !filepath.IsAbs(absPath) {
			t.Errorf("expected absolute path, got %q", absPath)
		}
	})

	t.Run("rejects path outside temp dir", func(t *testing.T) {
		home, _ := os.UserHomeDir()
		_, err := ValidateTempAvatarPath(filepath.Join(home, "avatar.png"))
		if err == nil {
			t.Error("expected error for path outside temp dir")
		}
	})

	t.Run("rejects non kojo-avatar directory", func(t *testing.T) {
		otherDir, err := os.MkdirTemp(tmpBase, "other-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(otherDir)
		otherFile := filepath.Join(otherDir, "avatar.png")
		os.WriteFile(otherFile, []byte("fake"), 0o644)

		_, err = ValidateTempAvatarPath(otherFile)
		if err == nil {
			t.Error("expected error for non kojo-avatar directory")
		}
	})

	t.Run("rejects unsupported extension", func(t *testing.T) {
		badFile := filepath.Join(avatarDir, "avatar.exe")
		os.WriteFile(badFile, []byte("fake"), 0o644)

		_, err := ValidateTempAvatarPath(badFile)
		if err == nil {
			t.Error("expected error for unsupported extension")
		}
	})

	t.Run("rejects nonexistent file", func(t *testing.T) {
		_, err := ValidateTempAvatarPath(filepath.Join(avatarDir, "nonexistent.png"))
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})

	t.Run("rejects directory", func(t *testing.T) {
		subDir := filepath.Join(avatarDir, "subdir.png")
		os.Mkdir(subDir, 0o755)

		_, err := ValidateTempAvatarPath(subDir)
		if err == nil {
			t.Error("expected error for directory")
		}
	})
}
