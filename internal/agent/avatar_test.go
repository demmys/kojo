package agent

import "testing"

func TestIsAllowedImageExt(t *testing.T) {
	allowed := []string{".png", ".jpg", ".jpeg", ".webp", ".svg", ".PNG", ".JPG", ".Webp"}
	for _, ext := range allowed {
		if !isAllowedImageExt(ext) {
			t.Errorf("expected %q to be allowed", ext)
		}
	}

	disallowed := []string{".gif", ".bmp", ".exe", ".html", "", ".PnG1"}
	for _, ext := range disallowed {
		if isAllowedImageExt(ext) {
			t.Errorf("expected %q to be disallowed", ext)
		}
	}
}
