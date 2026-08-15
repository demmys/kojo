package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const customAPIKeyProvider = "custom_api"

// CustomAPIKeyMaxBytes bounds secret material accepted from HTTP and peer-sync
// surfaces before encryption.
const CustomAPIKeyMaxBytes = 16 << 10

type customAPIKeyRecord struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
}

// LoadCustomAPIKey retrieves an agent-scoped custom inference API key.
// Missing keys are not errors because many loopback llama-server endpoints
// intentionally run without authentication.
func LoadCustomAPIKey(creds *CredentialStore, agentID, baseURL string) (string, error) {
	if creds == nil || agentID == "" {
		return "", nil
	}
	encoded, err := creds.GetToken(customAPIKeyProvider, agentID, "", "api_key")
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var record customAPIKeyRecord
	if err := json.Unmarshal([]byte(encoded), &record); err != nil {
		return "", errors.New("invalid custom API key record")
	}
	if strings.TrimSpace(baseURL) != record.BaseURL {
		return "", nil
	}
	return record.APIKey, nil
}

// StoreCustomAPIKey encrypts and stores an agent-scoped custom inference key.
// An empty value clears the credential.
func StoreCustomAPIKey(creds *CredentialStore, agentID, baseURL, apiKey string) error {
	if creds == nil {
		return errors.New("credential store is not available")
	}
	if agentID == "" {
		return errors.New("agent ID is required")
	}
	if len(apiKey) > CustomAPIKeyMaxBytes {
		return errors.New("custom API key exceeds size limit")
	}
	if strings.TrimSpace(apiKey) == "" {
		return creds.DeleteToken(customAPIKeyProvider, agentID, "", "api_key")
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return errors.New("custom API base URL is required")
	}
	record, err := json.Marshal(customAPIKeyRecord{BaseURL: baseURL, APIKey: strings.TrimSpace(apiKey)})
	if err != nil {
		return err
	}
	return creds.SetToken(customAPIKeyProvider, agentID, "", "api_key", string(record), time.Time{})
}
