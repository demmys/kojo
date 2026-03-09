// Package notifysource defines the interface and types for external
// notification sources (Gmail, Discord, etc.) that can be polled for new items.
package notifysource

import (
	"context"
	"time"
)

// Notification represents a single notification item from an external source.
type Notification struct {
	Title      string            `json:"title"`
	Body       string            `json:"body"`
	ReceivedAt time.Time         `json:"receivedAt"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// PollResult contains the result of a polling operation.
type PollResult struct {
	Items  []Notification
	Cursor string // opaque cursor for next poll (e.g., Gmail historyId)
}

// Source is the interface that notification source implementations must satisfy.
type Source interface {
	// Type returns the source type identifier (e.g., "gmail", "discord").
	Type() string

	// Poll checks for new items since the given cursor.
	// An empty cursor means first poll (source should return recent items).
	Poll(ctx context.Context, cursor string) (*PollResult, error)

	// Validate checks if credentials and configuration are valid.
	Validate(ctx context.Context) error
}

// Config holds the configuration for a notification source instance.
type Config struct {
	ID              string            `json:"id"`
	Type            string            `json:"type"`
	Enabled         bool              `json:"enabled"`
	IntervalMinutes int               `json:"intervalMinutes"`
	Query           string            `json:"query,omitempty"`
	Options         map[string]string `json:"options,omitempty"`
}

// Factory creates a Source from configuration and a token accessor.
type Factory func(cfg Config, tokens TokenAccessor) (Source, error)

// TokenAccessor provides read/write access to encrypted tokens for a source.
type TokenAccessor interface {
	GetToken(key string) (string, error)
	SetToken(key, value string, expiresAt time.Time) error
	GetTokenExpiry(key string) (string, time.Time, error)
}
