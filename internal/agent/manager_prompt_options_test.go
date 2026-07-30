package agent

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestPrepareChatOptionsDisableKojoAttachmentInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	a := Agent{ID: "ag_transport_prompt"}
	applyPrepareChatOptions(&a, prepareChatOptions{disableKojoAttachmentInstructions: true})

	prompt := buildSystemPrompt(&a,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		"", nil, false)
	for _, forbidden := range []string{
		"Sending file attachments to the user",
		"attachments.md",
		attachStagingSubpath,
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("transport-disabled prompt still contains %q", forbidden)
		}
	}
}
