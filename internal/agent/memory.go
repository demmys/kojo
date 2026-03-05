package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxPersonaSummaryRunes is the threshold above which we use an LLM-generated
// summary instead of the full persona text in the system prompt.
const maxPersonaSummaryRunes = 500

// readPersonaFile reads the full content of persona.md for an agent.
// Returns (content, true) on success (including empty file and missing file).
// Missing file returns ("", true) — treated as "persona cleared".
// Returns ("", false) only on unexpected I/O errors (permission denied, etc.).
func readPersonaFile(agentID string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(agentDir(agentID), "persona.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", true // file deleted = persona cleared
		}
		return "", false // real I/O error
	}
	return string(data), true
}

// writePersonaFile writes persona content to persona.md.
// Empty content removes the file (ENOENT is not an error).
func writePersonaFile(agentID string, content string) error {
	p := filepath.Join(agentDir(agentID), "persona.md")
	if content == "" {
		err := os.Remove(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// truncatePersona returns the first maxPersonaSummaryRunes of persona text.
func truncatePersona(persona string) string {
	runes := []rune(persona)
	if len(runes) > maxPersonaSummaryRunes {
		return string(runes[:maxPersonaSummaryRunes]) + "…"
	}
	return persona
}

// getPersonaSummary returns a concise summary of the persona for system prompt injection.
// It caches the summary in persona_summary.md and regenerates when persona.md is newer.
// Fallback chain: Gemini API → agent's CLI tool → truncation.
func getPersonaSummary(agentID string, persona string, tool string, logger *slog.Logger) string {
	dir := agentDir(agentID)
	personaPath := filepath.Join(dir, "persona.md")
	summaryPath := filepath.Join(dir, "persona_summary.md")

	// Use cached summary if persona.md hasn't changed since last generation
	pInfo, pErr := os.Stat(personaPath)
	sInfo, sErr := os.Stat(summaryPath)
	if sErr == nil && pErr == nil && !pInfo.ModTime().After(sInfo.ModTime()) {
		if data, err := os.ReadFile(summaryPath); err == nil && len(data) > 0 {
			return string(data)
		}
	}

	// 1. Try Gemini API (fast, works for all agents)
	var summary string
	if result, err := SummarizePersona(persona); err != nil {
		logger.Warn("Gemini persona summary failed", "agent", agentID, "err", err)
	} else {
		summary = result
	}

	// 2. Fallback: agent's own CLI tool (claude -p / codex exec)
	if summary == "" && (tool == "claude" || tool == "codex") {
		if result, err := SummarizeWithCLI(tool, persona); err != nil {
			logger.Warn("CLI persona summary failed", "agent", agentID, "tool", tool, "err", err)
		} else {
			summary = result
		}
	}

	// 3. Final fallback: truncation
	if summary == "" {
		summary = truncatePersona(persona)
	}

	// Cache — but only if persona.md hasn't been updated since we started.
	// This prevents a slow background goroutine from overwriting a newer summary.
	if pErr != nil {
		// persona.md didn't exist at start — cache unconditionally
		_ = os.WriteFile(summaryPath, []byte(summary), 0o644)
	} else if pNow, err := os.Stat(personaPath); err == nil &&
		pNow.ModTime().Equal(pInfo.ModTime()) {
		_ = os.WriteFile(summaryPath, []byte(summary), 0o644)
	}
	return summary
}

// buildSystemPrompt constructs the full system prompt for an agent chat.
// It injects persona, long-term memory, daily notes, and FTS5 search results.
func buildSystemPrompt(a *Agent, userMessage string, logger *slog.Logger) string {
	var sb strings.Builder

	// Persona — if long, inject LLM-generated summary + path to full file
	dir := agentDir(a.ID)
	personaPath := filepath.Join(dir, "persona.md")
	if a.Persona != "" {
		runes := []rune(a.Persona)
		if len(runes) > maxPersonaSummaryRunes {
			summary := getPersonaSummary(a.ID, a.Persona, a.Tool, logger)
			sb.WriteString("# Persona (Summary)\n\n")
			sb.WriteString(summary)
			sb.WriteString("\n\n")
			sb.WriteString(fmt.Sprintf("Full persona: %s\n\n", personaPath))
		} else {
			sb.WriteString("# Persona\n\n")
			sb.WriteString(a.Persona)
			sb.WriteString("\n\n")
		}
	}

	// Long-term memory (MEMORY.md)
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
	sb.WriteString(fmt.Sprintf("- %s defines your personality. You can edit it to evolve.\n", personaPath))
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

	// Write persona.md
	if err := writePersonaFile(a.ID, a.Persona); err != nil {
		return err
	}

	return nil
}
