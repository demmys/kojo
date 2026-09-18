package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TypeSafe (Jev) System One client. Jev answers typed questions (noul /
// choice / score) over a JSON state with calibrated probabilities instead
// of generated text, which makes it a cheap, fast substitute for the
// small "judgment" LLM calls kojo used to route through the claude CLI.
// Docs: https://docs.typesafe.ai/api

const (
	typesafeDefaultBaseURL = "https://api.typesafe.ai"
	typesafeDefaultModel   = "jev-latest"
	// typesafeMaxResponseBytes caps how much of a System One response is
	// read so a misbehaving upstream can't exhaust memory.
	typesafeMaxResponseBytes = 1 << 20
)

// typesafeHTTPClient is shared by every System One call. Per-call
// deadlines come from the caller's context; the client timeout is only a
// backstop against callers that forget one.
var typesafeHTTPClient = &http.Client{Timeout: 30 * time.Second}

// typesafeBaseURL resolves the API base, honoring TYPESAFE_BASE_URL (the
// SDKs' override) so tests and proxies can redirect the client.
func typesafeBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("TYPESAFE_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return typesafeDefaultBaseURL
}

// LoadTypeSafeAPIKey loads the TypeSafe (Jev) API key.
// Priority: 1) encrypted credential store (provider "typesafe"),
// 2) TYPESAFE_API_KEY env var, 3) ~/.config/typesafe/credentials
// (raw key, one line — same convention as the grok-research fallback).
func LoadTypeSafeAPIKey(creds *CredentialStore) (string, error) {
	if creds != nil {
		if key, err := creds.GetToken("typesafe", "", "", "api_key"); err == nil {
			if key = strings.TrimSpace(key); key != "" {
				return key, nil
			}
		}
	}
	if key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); key != "" {
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot get home dir: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "typesafe", "credentials"))
	if err != nil {
		return "", fmt.Errorf("TypeSafe API key not configured (check Settings or TYPESAFE_API_KEY): %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("TypeSafe API key not configured (check Settings or TYPESAFE_API_KEY)")
	}
	return key, nil
}

// jevQuestion is one entry of a System One request's questions map.
// Criteria shape depends on Type: noul → {"true":..,"false":..} (optional),
// choice → map option→description (required), score → ordered []string.
type jevQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// jevAnswer is the union of the three answer shapes; only the fields for
// the question's type are populated.
type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// jevSystemOne is the raw POST /v1/systemone call. state may be a string
// or any JSON-marshalable value; the caller owns the question design.
// Replaced by unit tests through the jevCall seam.
func jevSystemOne(ctx context.Context, apiKey string, state any, questions map[string]jevQuestion) (*jevResponse, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("typesafe: missing API key")
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("typesafe: no questions")
	}
	body, err := json.Marshal(map[string]any{
		"state":     state,
		"model":     typesafeDefaultModel,
		"questions": questions,
	})
	if err != nil {
		return nil, fmt.Errorf("typesafe: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, typesafeBaseURL()+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("typesafe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := typesafeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, typesafeMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("typesafe: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("typesafe: HTTP %d: %s", resp.StatusCode, headRunes(strings.TrimSpace(string(raw)), 300))
	}
	var out jevResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("typesafe: decode response: %w", err)
	}
	if len(out.Answers) == 0 {
		return nil, fmt.Errorf("typesafe: response has no answers")
	}
	return &out, nil
}

// jevCall is the System One seam swapped out by unit tests.
var jevCall = jevSystemOne
