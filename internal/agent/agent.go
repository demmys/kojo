package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// allowedIntervals defines the valid intervalMinutes values.
// Sub-hourly values must divide 60; hourly values must divide 24 (in hours).
var allowedIntervals = map[int]bool{
	0: true, 10: true, 30: true, 60: true,
	180: true, 360: true, 720: true, 1440: true,
}

// ValidInterval returns true if the given interval is in the allowed set.
func ValidInterval(minutes int) bool {
	return allowedIntervals[minutes]
}

// Agent represents a persistent AI persona (friend).
type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Persona         string `json:"persona"`         // persona description (markdown)
	Model           string `json:"model"`           // e.g. "sonnet", "opus"
	Tool            string `json:"tool"`            // CLI tool: "claude", "codex", "gemini"
	IntervalMinutes int    `json:"intervalMinutes"` // periodic execution interval in minutes (0 = disabled)
	CreatedAt       string `json:"createdAt"`       // RFC3339
	UpdatedAt       string `json:"updatedAt"`       // RFC3339

	// Legacy field — only used during migration from cronExpr-based configs.
	// Not included in JSON output; consumed by store.Load migration.
	LegacyCronExpr string `json:"cronExpr,omitempty"`

	// HasAvatar indicates whether a custom avatar file exists.
	HasAvatar bool `json:"hasAvatar"`
	// AvatarHash is derived from the avatar file's modtime for cache busting.
	AvatarHash string `json:"avatarHash,omitempty"`

	// PublicProfile is a short outward-facing description generated from persona.
	// Shared with other agents via the directory endpoint. Does not expose internal persona details.
	PublicProfile         string `json:"publicProfile,omitempty"`
	PublicProfileOverride bool   `json:"publicProfileOverride,omitempty"`

	// LastMessage is a preview of the most recent message (for list display).
	LastMessage *MessagePreview `json:"lastMessage,omitempty"`
}

// MessagePreview is a short summary for agent list display.
type MessagePreview struct {
	Content   string `json:"content"`
	Role      string `json:"role"`
	Timestamp string `json:"timestamp"`
}

// DirectoryEntry is the minimal public info shared with other agents.
type DirectoryEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublicProfile string `json:"publicProfile"`
}

// AgentConfig is the request body for creating an agent.
type AgentConfig struct {
	Name            string `json:"name"`
	Persona         string `json:"persona"`
	Model           string `json:"model"`
	Tool            string `json:"tool"`
	IntervalMinutes *int   `json:"intervalMinutes"` // nil = use default (30)
}

// AgentUpdateConfig is the request body for PATCH updates.
// Pointer fields distinguish "not provided" (nil) from "set to zero".
type AgentUpdateConfig struct {
	Name                  *string `json:"name"`
	Persona               *string `json:"persona"`
	PublicProfile         *string `json:"publicProfile"`
	PublicProfileOverride *bool   `json:"publicProfileOverride"`
	Model                 *string `json:"model"`
	Tool                  *string `json:"tool"`
	IntervalMinutes       *int    `json:"intervalMinutes"`
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ag_" + hex.EncodeToString(b)
}

func newAgent(cfg AgentConfig) (*Agent, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	interval := 30 // default
	if cfg.IntervalMinutes != nil {
		interval = *cfg.IntervalMinutes
	}
	if !ValidInterval(interval) {
		return nil, fmt.Errorf("unsupported interval: %d minutes", interval)
	}
	a := &Agent{
		ID:              generateID(),
		Name:            cfg.Name,
		Persona:         cfg.Persona,
		Model:           cfg.Model,
		Tool:            cfg.Tool,
		IntervalMinutes: interval,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if a.Tool == "" {
		a.Tool = "claude"
	}
	if a.Model == "" {
		a.Model = "sonnet"
	}
	return a, nil
}

// intervalToCron converts an interval in minutes and an agent ID to a cron
// expression with a deterministic per-agent offset derived from the ID hash.
// This spreads agents across time so they don't all fire simultaneously.
// Only supports values in allowedIntervals. Returns "" if interval <= 0.
func intervalToCron(intervalMinutes int, agentID string) string {
	if intervalMinutes <= 0 {
		return ""
	}

	// Deterministic offset from agent ID (spread across full day = 1440 min)
	h := fnv.New32a()
	h.Write([]byte(agentID))
	hash := int(h.Sum32())

	if intervalMinutes >= 60 {
		hours := intervalMinutes / 60
		minuteOfDay := hash % 1440 // 0..1439
		minuteOffset := minuteOfDay % 60
		hourOffset := (minuteOfDay / 60) % hours

		if hours >= 24 {
			// Once a day
			return fmt.Sprintf("%d %d * * *", minuteOffset, minuteOfDay/60%24)
		}
		// Every N hours at a fixed minute
		hourList := make([]string, 0, 24/hours)
		for hr := hourOffset; hr < 24; hr += hours {
			hourList = append(hourList, fmt.Sprintf("%d", hr))
		}
		return fmt.Sprintf("%d %s * * *", minuteOffset, strings.Join(hourList, ","))
	}

	// Sub-hourly (10 or 30 — both divide 60 evenly)
	offset := hash % intervalMinutes
	mins := make([]string, 0, 60/intervalMinutes)
	for m := offset; m < 60; m += intervalMinutes {
		mins = append(mins, fmt.Sprintf("%d", m))
	}
	return fmt.Sprintf("%s * * * *", strings.Join(mins, ","))
}
