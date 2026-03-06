package agent

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Agent represents a persistent AI persona (friend).
type Agent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Persona   string `json:"persona"`   // persona description (markdown)
	Model     string `json:"model"`     // e.g. "sonnet", "opus"
	Tool      string `json:"tool"`      // CLI tool: "claude", "codex", "gemini"
	CronExpr  string `json:"cronExpr"`  // cron expression for periodic execution (optional)
	CreatedAt string `json:"createdAt"` // RFC3339
	UpdatedAt string `json:"updatedAt"` // RFC3339

	// HasAvatar indicates whether a custom avatar file exists.
	HasAvatar bool `json:"hasAvatar"`
	// AvatarHash is derived from the avatar file's modtime for cache busting.
	AvatarHash string `json:"avatarHash,omitempty"`

	// LastMessage is a preview of the most recent message (for list display).
	LastMessage *MessagePreview `json:"lastMessage,omitempty"`
}

// MessagePreview is a short summary for agent list display.
type MessagePreview struct {
	Content   string `json:"content"`
	Role      string `json:"role"`
	Timestamp string `json:"timestamp"`
}

// AgentConfig is the request body for creating an agent.
type AgentConfig struct {
	Name     string `json:"name"`
	Persona  string `json:"persona"`
	Model    string `json:"model"`
	Tool     string `json:"tool"`
	CronExpr string `json:"cronExpr"`
}

// AgentUpdateConfig is the request body for PATCH updates.
// Pointer fields distinguish "not provided" (nil) from "set to empty" ("").
type AgentUpdateConfig struct {
	Name     *string `json:"name"`
	Persona  *string `json:"persona"`
	Model    *string `json:"model"`
	Tool     *string `json:"tool"`
	CronExpr *string `json:"cronExpr"`
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ag_" + hex.EncodeToString(b)
}

func newAgent(cfg AgentConfig) *Agent {
	now := time.Now().UTC().Format(time.RFC3339)
	a := &Agent{
		ID:        generateID(),
		Name:      cfg.Name,
		Persona:   cfg.Persona,
		Model:     cfg.Model,
		Tool:      cfg.Tool,
		CronExpr:  cfg.CronExpr,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if a.Tool == "" {
		a.Tool = "claude"
	}
	if a.Model == "" {
		a.Model = "sonnet"
	}
	if a.CronExpr == "" {
		a.CronExpr = "*/30 * * * *"
	}
	return a
}
