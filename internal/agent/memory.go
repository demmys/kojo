package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// buildSystemPrompt constructs the full system prompt for an agent chat.
// It injects persona, long-term memory, daily notes, and FTS5 search results.
func buildSystemPrompt(a *Agent, userMessage string, logger *slog.Logger) string {
	var sb strings.Builder

	// Persona
	if a.Persona != "" {
		sb.WriteString("# Persona\n\n")
		sb.WriteString(a.Persona)
		sb.WriteString("\n\n")
	}

	// Long-term memory (MEMORY.md)
	dir := agentDir(a.ID)
	memoryPath := filepath.Join(dir, "MEMORY.md")
	if data, err := os.ReadFile(memoryPath); err == nil && len(data) > 0 {
		sb.WriteString("# Memory\n\n")
		sb.WriteString(string(data))
		sb.WriteString("\n\n")
	}

	// Daily notes
	today := time.Now().Format("2006-01-02")
	dailyPath := filepath.Join(dir, "memory", today+".md")
	if data, err := os.ReadFile(dailyPath); err == nil && len(data) > 0 {
		sb.WriteString("# Today's Notes\n\n")
		sb.WriteString(string(data))
		sb.WriteString("\n\n")
	}

	// FTS5 search - inject relevant memory context based on user message
	if userMessage != "" {
		idx, err := OpenMemoryIndex(a.ID, logger)
		if err == nil {
			defer idx.Close()
			// Incremental index update
			idx.IndexNewMessages(a.ID)
			idx.IndexFilesIfStale(a.ID)
			if context := idx.BuildContextFromQuery(userMessage); context != "" {
				sb.WriteString(context)
				sb.WriteString("\n")
			}
		}
	}

	// Instructions
	sb.WriteString("# Instructions\n\n")
	sb.WriteString("- Stay in character at all times.\n")
	sb.WriteString(fmt.Sprintf("- Your data directory is: %s\n", dir))
	sb.WriteString("- MEMORY.md contains your persistent long-term memory. When you learn important information, update it using the Edit tool.\n")
	sb.WriteString(fmt.Sprintf("- Record daily thoughts and observations in memory/%s.md using the Edit or Write tool.\n", today))
	sb.WriteString("- Keep your responses conversational and in character.\n")
	sb.WriteString(fmt.Sprintf("- Today's date is %s.\n", today))

	return sb.String()
}

// ensureAgentDir creates the agent's data directory and default files.
func ensureAgentDir(a *Agent) error {
	dir := agentDir(a.ID)
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		return err
	}

	// Create MEMORY.md if it doesn't exist
	memPath := filepath.Join(dir, "MEMORY.md")
	if _, err := os.Stat(memPath); os.IsNotExist(err) {
		initial := fmt.Sprintf("# %s's Memory\n\nThis file stores persistent memories. Update it as you learn new things.\n", a.Name)
		if err := os.WriteFile(memPath, []byte(initial), 0o644); err != nil {
			return err
		}
	}

	// Create persona.md
	personaPath := filepath.Join(dir, "persona.md")
	if a.Persona != "" {
		if err := os.WriteFile(personaPath, []byte(a.Persona), 0o644); err != nil {
			return err
		}
	}

	return nil
}
