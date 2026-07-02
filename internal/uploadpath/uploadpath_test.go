package uploadpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	want := filepath.Join(os.TempDir(), "kojo", "upload")
	if got := Dir(); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestSanitizeName pins the upload-sanitization security invariant: the
// output must be byte-identical to filepath.Base followed by a Replacer
// mapping "/", "\\" and NUL to "_".
func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "photo.jpg", "photo.jpg"},
		{"strips directory", "/etc/passwd", "passwd"},
		{"forward slash residual", "a/b.txt", "b.txt"},
		{"backslash to underscore", "a\\b.txt", "a_b.txt"},
		{"nul to underscore", "a\x00b.txt", "a_b.txt"},
		{"traversal", "../../secret", "secret"},
		{"mixed", "dir\\sub\x00name.png", "dir_sub_name.png"},
		{"empty", "", "."}, // filepath.Base("") == "."
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeName(tt.in); got != tt.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
