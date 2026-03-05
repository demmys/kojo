package agent

import (
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// avatarFileName returns the expected avatar file path for an agent.
// Checks for common image extensions.
func avatarFilePath(agentID string) string {
	dir := agentDir(agentID)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".svg"} {
		p := filepath.Join(dir, "avatar"+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// hasAvatar returns true if the agent has a custom avatar file.
func hasAvatar(agentID string) bool {
	return avatarFilePath(agentID) != ""
}

// ServeAvatar serves the agent's avatar image, falling back to a generated SVG.
func ServeAvatar(w http.ResponseWriter, r *http.Request, a *Agent) {
	if path := avatarFilePath(a.ID); path != "" {
		http.ServeFile(w, r, path)
		return
	}
	// Generate SVG fallback
	svg := generateSVGAvatar(a.Name)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(svg))
}

// SaveAvatar saves an uploaded avatar file for an agent.
func SaveAvatar(agentID string, src io.Reader, ext string) error {
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Remove any existing avatars
	for _, e := range []string{".png", ".jpg", ".jpeg", ".webp", ".svg"} {
		os.Remove(filepath.Join(dir, "avatar"+e))
	}

	dst, err := os.Create(filepath.Join(dir, "avatar"+ext))
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}

// GenerateAvatarWithAI generates an avatar using nanobanana.sh.
func GenerateAvatarWithAI(agentID string, persona string, name string, prompt string, logger *slog.Logger) (string, error) {
	scriptPath := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "nanobanana", "scripts", "nanobanana.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		return "", fmt.Errorf("nanobanana.sh not found at %s", scriptPath)
	}

	// Build image generation prompt
	imagePrompt := fmt.Sprintf("Character portrait avatar of %s, ", name)
	if persona != "" {
		// Extract key traits from persona (first 100 runes to avoid breaking UTF-8)
		runes := []rune(persona)
		if len(runes) > 100 {
			runes = runes[:100]
		}
		imagePrompt += string(runes) + ", "
	}
	if prompt != "" {
		imagePrompt += prompt + ", "
	}
	imagePrompt += "flat illustration style, clean background, centered face, square format"

	// Create temp dir for output
	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}

	cmd := exec.Command("bash", scriptPath, "generate",
		"--aspect", "1:1",
		"--size", "512px",
		"--output", tmpDir,
		imagePrompt,
	)
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Debug("nanobanana.sh failed", "output", string(output), "err", err)
		return "", fmt.Errorf("avatar generation failed: %w", err)
	}

	// Find the generated image
	entries, err := os.ReadDir(tmpDir)
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("no image generated")
	}

	// Return path to first image file
	for _, e := range entries {
		if !e.IsDir() {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" {
				return filepath.Join(tmpDir, e.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("no image file found in output")
}

// GenerateSVGAvatarFile creates an SVG avatar file in a temp directory and returns its path.
// Used as fallback when AI avatar generation is unavailable.
func GenerateSVGAvatarFile(name string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}
	svg := generateSVGAvatar(name)
	p := filepath.Join(tmpDir, "avatar.svg")
	if err := os.WriteFile(p, []byte(svg), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// generateSVGAvatar creates a fallback SVG avatar using name-derived gradient and initials.
func generateSVGAvatar(name string) string {
	hash := md5.Sum([]byte(name))

	// Generate two colors from hash for gradient
	h1 := int(hash[0])%360
	h2 := (h1 + 60 + int(hash[1])%120) % 360

	// Get initials (first letter of first two words, or first two letters)
	initials := "?"
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		initials = strings.ToUpper(string([]rune(parts[0])[0:1]) + string([]rune(parts[1])[0:1]))
	} else if len(name) > 0 {
		runes := []rune(name)
		if len(runes) >= 2 {
			initials = strings.ToUpper(string(runes[0:2]))
		} else {
			initials = strings.ToUpper(string(runes[0:1]))
		}
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <defs>
    <linearGradient id="g" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
      <stop offset="0%%" style="stop-color:hsl(%d,70%%,50%%)" />
      <stop offset="100%%" style="stop-color:hsl(%d,70%%,40%%)" />
    </linearGradient>
  </defs>
  <rect width="100" height="100" rx="20" fill="url(#g)" />
  <text x="50" y="50" text-anchor="middle" dominant-baseline="central"
    font-family="system-ui,-apple-system,sans-serif" font-size="36" font-weight="600" fill="white">%s</text>
</svg>`, h1, h2, initials)
}
