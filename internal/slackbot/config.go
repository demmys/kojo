// Package slackbot provides Slack Socket Mode integration for Kojo agents.
package slackbot

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// isTokenNotFound reports whether the error from TokenProvider.GetToken
// indicates the key is absent rather than a real I/O failure. The credential
// store implementation surfaces sql.ErrNoRows directly in that case.
func isTokenNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// TokenProvider reads/writes encrypted Slack tokens from a credential store.
type TokenProvider interface {
	GetToken(provider, agentID, sourceID, key string) (string, error)
	SetToken(provider, agentID, sourceID, key, value string, expiresAt time.Time) error
	DeleteToken(provider, agentID, sourceID, key string) error
	DeleteTokensBySource(provider, agentID, sourceID string) error
}

const (
	tokenProvider = "slack"
	tokenSourceID = "" // agent-level, no per-source scoping

	keyAppToken = "app_token"
	keyBotToken = "bot_token"
)

// StoreTokens saves the app and bot tokens to the credential store.
// If the second write fails, the first is rolled back to its previous value
// so the store never contains a half-updated pair.
func StoreTokens(tp TokenProvider, agentID, appToken, botToken string) error {
	noExpiry := time.Time{}

	// Snapshot the current app token so we can restore it on partial failure.
	// A non-nil error other than "not found" means we cannot distinguish
	// "previously empty" from "read failed" — in that case, bail out before
	// touching the store so we don't risk deleting a valid existing value
	// during rollback.
	oldAppToken, getErr := tp.GetToken(tokenProvider, agentID, tokenSourceID, keyAppToken)
	if getErr != nil && !isTokenNotFound(getErr) {
		return fmt.Errorf("snapshot app token: %w", getErr)
	}

	if err := tp.SetToken(tokenProvider, agentID, tokenSourceID, keyAppToken, appToken, noExpiry); err != nil {
		return fmt.Errorf("store app token: %w", err)
	}
	if err := tp.SetToken(tokenProvider, agentID, tokenSourceID, keyBotToken, botToken, noExpiry); err != nil {
		// Rollback: restore previous app token to avoid half-updated state.
		// If there was no previous value, delete the newly written key.
		// Surface rollback failures — if they happen, the store is left in a
		// half-updated state and the caller needs to know.
		var rollbackErr error
		if oldAppToken != "" {
			rollbackErr = tp.SetToken(tokenProvider, agentID, tokenSourceID, keyAppToken, oldAppToken, noExpiry)
		} else {
			rollbackErr = tp.DeleteToken(tokenProvider, agentID, tokenSourceID, keyAppToken)
		}
		if rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("store bot token: %w", err),
				fmt.Errorf("rollback app token: %w", rollbackErr),
			)
		}
		return fmt.Errorf("store bot token: %w", err)
	}
	return nil
}

// LoadTokens retrieves the app and bot tokens from the credential store.
func LoadTokens(tp TokenProvider, agentID string) (appToken, botToken string, err error) {
	appToken, err = tp.GetToken(tokenProvider, agentID, tokenSourceID, keyAppToken)
	if err != nil {
		return "", "", fmt.Errorf("load app token: %w", err)
	}
	botToken, err = tp.GetToken(tokenProvider, agentID, tokenSourceID, keyBotToken)
	if err != nil {
		return "", "", fmt.Errorf("load bot token: %w", err)
	}
	return appToken, botToken, nil
}

// DeleteTokens removes all Slack tokens for an agent.
func DeleteTokens(tp TokenProvider, agentID string) error {
	return tp.DeleteTokensBySource(tokenProvider, agentID, tokenSourceID)
}
