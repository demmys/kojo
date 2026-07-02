// Package uploadpath centralizes the on-disk location and filename
// sanitization for user-uploaded attachments so the WebUI upload handler
// and the Slack file-download path agree byte-for-byte.
package uploadpath

import (
	"os"
	"path/filepath"
	"strings"
)

// Dir returns the directory where uploaded attachments are stored:
// {os.TempDir()}/kojo/upload. Callers create it on demand.
func Dir() string {
	return filepath.Join(os.TempDir(), "kojo", "upload")
}

// SanitizeName removes path separators and other problematic characters
// from a filename. It strips any directory components via filepath.Base,
// then replaces "/", "\\" and NUL with "_". This is a security boundary
// with one caveat inherited from filepath.Base: "." and ".." pass through
// unchanged, so the output must not be used as a bare path element.
// Callers join it with a prefix ({unixnano}_{name}), which makes the
// result a single benign filename component under Dir().
func SanitizeName(name string) string {
	name = filepath.Base(name) // strip any directory components
	// Replace problematic characters with underscore.
	replacer := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_")
	return replacer.Replace(name)
}
