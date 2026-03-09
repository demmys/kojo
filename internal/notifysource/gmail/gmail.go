// Package gmail implements a Gmail notification source using the Gmail API.
package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/notifysource"
)

const (
	gmailAPIBase    = "https://gmail.googleapis.com/gmail/v1"
	tokenRefreshURL = "https://oauth2.googleapis.com/token"
)

// Source implements notifysource.Source for Gmail.
type Source struct {
	query  string
	tokens notifysource.TokenAccessor
}

// New creates a new Gmail notification source.
func New(cfg notifysource.Config, tokens notifysource.TokenAccessor) (notifysource.Source, error) {
	query := cfg.Query
	if query == "" {
		query = "is:unread"
	}
	return &Source{
		query:  query,
		tokens: tokens,
	}, nil
}

func (s *Source) Type() string { return "gmail" }

// Poll checks for new emails since the given cursor (Gmail historyId).
func (s *Source) Poll(ctx context.Context, cursor string) (*notifysource.PollResult, error) {
	accessToken, err := s.ensureAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}

	if cursor == "" {
		return s.initialPoll(ctx, accessToken)
	}
	return s.historyPoll(ctx, accessToken, cursor)
}

// Validate checks if the OAuth2 tokens are valid.
func (s *Source) Validate(ctx context.Context) error {
	_, err := s.ensureAccessToken(ctx)
	return err
}

// initialPoll fetches the current historyId and recent unread messages.
func (s *Source) initialPoll(ctx context.Context, token string) (*notifysource.PollResult, error) {
	// Get profile for current historyId
	profile, err := s.getProfile(ctx, token)
	if err != nil {
		return nil, err
	}

	// List recent unread messages
	msgs, err := s.listMessages(ctx, token, s.query, 10)
	if err != nil {
		return nil, err
	}

	var items []notifysource.Notification
	for _, m := range msgs {
		items = append(items, messageToNotification(m))
	}

	return &notifysource.PollResult{
		Items:  items,
		Cursor: strconv.FormatUint(profile.HistoryID, 10),
	}, nil
}

// historyPoll uses Gmail history API to find new messages since cursor.
func (s *Source) historyPoll(ctx context.Context, token, cursor string) (*notifysource.PollResult, error) {
	historyID, err := strconv.ParseUint(cursor, 10, 64)
	if err != nil {
		// Invalid cursor, fall back to initial poll
		return s.initialPoll(ctx, token)
	}

	params := url.Values{
		"startHistoryId": {cursor},
		"historyTypes":   {"messageAdded"},
		"labelId":        {"INBOX"},
	}

	body, err := s.apiGet(ctx, token, "/users/me/history?"+params.Encode())
	if err != nil {
		// Only reinitialize on 404 (expired historyId); other errors should propagate
		if isNotFoundError(err) {
			return s.initialPoll(ctx, token)
		}
		return nil, fmt.Errorf("history poll: %w", err)
	}

	var resp historyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse history: %w", err)
	}

	// Collect new message IDs
	seen := make(map[string]bool)
	var messageIDs []string
	for _, h := range resp.History {
		for _, added := range h.MessagesAdded {
			id := added.Message.ID
			if !seen[id] {
				seen[id] = true
				messageIDs = append(messageIDs, id)
			}
		}
	}

	newCursor := cursor
	if resp.HistoryID > 0 {
		newCursor = strconv.FormatUint(resp.HistoryID, 10)
	}

	if len(messageIDs) == 0 {
		return &notifysource.PollResult{Cursor: newCursor}, nil
	}

	// Fetch message details for all new messages.
	// Only advance the cursor if ALL messages are fetched successfully;
	// otherwise keep the old cursor so we retry on the next poll.
	var items []notifysource.Notification
	allFetched := true
	for _, id := range messageIDs {
		msg, err := s.getMessage(ctx, token, id)
		if err != nil {
			allFetched = false
			break
		}
		if s.query != "" && strings.Contains(s.query, "is:unread") {
			if !contains(msg.LabelIDs, "UNREAD") {
				continue
			}
		}
		items = append(items, messageToNotification(msg))
	}

	if !allFetched {
		// Don't advance cursor or return partial items to avoid duplicates on retry
		return nil, fmt.Errorf("fetch messages failed (fetched %d/%d)", len(items), len(messageIDs))
	}

	_ = historyID // used for error handling above

	return &notifysource.PollResult{
		Items:  items,
		Cursor: newCursor,
	}, nil
}

// ensureAccessToken returns a valid access token, refreshing if expired.
func (s *Source) ensureAccessToken(ctx context.Context) (string, error) {
	token, exp, err := s.tokens.GetTokenExpiry("access_token")
	if err == nil && token != "" && time.Now().Before(exp.Add(-60*time.Second)) {
		return token, nil
	}

	// Need to refresh
	refreshToken, err := s.tokens.GetToken("refresh_token")
	if err != nil {
		return "", fmt.Errorf("no refresh token: %w", err)
	}
	clientID, err := s.tokens.GetToken("client_id")
	if err != nil {
		return "", fmt.Errorf("no client_id: %w", err)
	}
	clientSecret, err := s.tokens.GetToken("client_secret")
	if err != nil {
		return "", fmt.Errorf("no client_secret: %w", err)
	}

	newToken, expiresIn, err := refreshAccessToken(ctx, clientID, clientSecret, refreshToken)
	if err != nil {
		return "", err
	}

	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)
	if err := s.tokens.SetToken("access_token", newToken, expiry); err != nil {
		return "", fmt.Errorf("save access token: %w", err)
	}

	return newToken, nil
}

func refreshAccessToken(ctx context.Context, clientID, clientSecret, refreshToken string) (string, int, error) {
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenRefreshURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token refresh failed (%d): %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parse token response: %w", err)
	}

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
}

// API helpers

// apiError represents a Gmail API error with status code.
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Gmail API error (%d): %s", e.StatusCode, e.Body)
}

func isNotFoundError(err error) bool {
	var ae *apiError
	if ok := errors.As(err, &ae); ok {
		return ae.StatusCode == http.StatusNotFound
	}
	return false
}

func (s *Source) apiGet(ctx context.Context, token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", gmailAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

func (s *Source) getProfile(ctx context.Context, token string) (*profile, error) {
	body, err := s.apiGet(ctx, token, "/users/me/profile")
	if err != nil {
		return nil, err
	}
	var p profile
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Source) listMessages(ctx context.Context, token, query string, maxResults int) ([]*message, error) {
	params := url.Values{
		"q":          {query},
		"maxResults": {strconv.Itoa(maxResults)},
	}
	body, err := s.apiGet(ctx, token, "/users/me/messages?"+params.Encode())
	if err != nil {
		return nil, err
	}

	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var msgs []*message
	for _, m := range resp.Messages {
		msg, err := s.getMessage(ctx, token, m.ID)
		if err != nil {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func (s *Source) getMessage(ctx context.Context, token, id string) (*message, error) {
	body, err := s.apiGet(ctx, token, "/users/me/messages/"+id+"?format=metadata&metadataHeaders=From&metadataHeaders=Subject")
	if err != nil {
		return nil, err
	}
	var msg message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// types

type profile struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    uint64 `json:"historyId"`
}

type message struct {
	ID         string   `json:"id"`
	LabelIDs   []string `json:"labelIds"`
	InternalDate string `json:"internalDate"` // milliseconds since epoch
	Payload    struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

func (m *message) header(name string) string {
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

type historyResponse struct {
	History   []historyRecord `json:"history"`
	HistoryID uint64          `json:"historyId"`
}

type historyRecord struct {
	MessagesAdded []struct {
		Message struct {
			ID       string   `json:"id"`
			LabelIDs []string `json:"labelIds"`
		} `json:"message"`
	} `json:"messagesAdded"`
}

func messageToNotification(msg *message) notifysource.Notification {
	from := msg.header("From")
	subject := msg.header("Subject")

	title := "From: " + from
	body := "Subject: " + subject

	var receivedAt time.Time
	if ms, err := strconv.ParseInt(msg.InternalDate, 10, 64); err == nil {
		receivedAt = time.UnixMilli(ms)
	}

	return notifysource.Notification{
		Title:      title,
		Body:       body,
		ReceivedAt: receivedAt,
		Meta:       map[string]string{"messageId": msg.ID},
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
