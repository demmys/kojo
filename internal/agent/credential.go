package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const credentialsFile = "credentials.json"

var credMu sync.Mutex

// Credential represents a stored ID/password pair for an agent.
type Credential struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	TOTPSecret    string `json:"totpSecret,omitempty"`
	TOTPAlgorithm string `json:"totpAlgorithm,omitempty"` // SHA1 (default), SHA256, SHA512
	TOTPDigits    int    `json:"totpDigits,omitempty"`     // 6 (default) or 8
	TOTPPeriod    int    `json:"totpPeriod,omitempty"`     // 30 (default)
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func generateCredID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "cred_" + hex.EncodeToString(b)
}

func loadCredentials(agentID string) ([]*Credential, error) {
	path := filepath.Join(agentDir(agentID), credentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var creds []*Credential
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}
	// Filter out nil entries from malformed JSON
	filtered := creds[:0]
	for _, c := range creds {
		if c != nil {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

func saveCredentials(agentID string, creds []*Credential) error {
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, credentialsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

const maskedValue = "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022"

func maskCredential(c *Credential) *Credential {
	cp := *c
	if cp.Password != "" {
		cp.Password = maskedValue
	}
	if cp.TOTPSecret != "" {
		cp.TOTPSecret = maskedValue
	}
	return &cp
}

// ListCredentials returns all credentials for an agent with passwords masked.
func ListCredentials(agentID string) ([]*Credential, error) {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return nil, err
	}
	masked := make([]*Credential, len(creds))
	for i, c := range creds {
		masked[i] = maskCredential(c)
	}
	return masked, nil
}

// TOTPParams holds TOTP configuration for a credential.
type TOTPParams struct {
	Secret    string
	Algorithm string // SHA1, SHA256, SHA512
	Digits    int    // 6 or 8
	Period    int    // seconds
}

// AddCredential adds a new credential for an agent.
func AddCredential(agentID, label, username, password string, totp *TOTPParams) (*Credential, error) {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	c := &Credential{
		ID:        generateCredID(),
		Label:     label,
		Username:  username,
		Password:  password,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if totp != nil && totp.Secret != "" {
		normalized, err := ValidateTOTPParams(totp.Secret, totp.Algorithm, totp.Digits, totp.Period)
		if err != nil {
			return nil, err
		}
		c.TOTPSecret = normalized
		c.TOTPAlgorithm = totp.Algorithm
		c.TOTPDigits = totp.Digits
		c.TOTPPeriod = totp.Period
	}
	creds = append(creds, c)
	if err := saveCredentials(agentID, creds); err != nil {
		return nil, err
	}

	return maskCredential(c), nil
}

// UpdateCredential updates an existing credential. Only non-nil fields are applied.
func UpdateCredential(agentID, credID string, label, username, password *string, totp *TOTPParams) (*Credential, error) {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return nil, err
	}

	var target *Credential
	for _, c := range creds {
		if c.ID == credID {
			target = c
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("credential not found: %s", credID)
	}

	if label != nil {
		target.Label = *label
	}
	if username != nil {
		target.Username = *username
	}
	if password != nil {
		target.Password = *password
	}
	if totp != nil {
		if totp.Secret != "" {
			normalized, err := ValidateTOTPParams(totp.Secret, totp.Algorithm, totp.Digits, totp.Period)
			if err != nil {
				return nil, err
			}
			totp.Secret = normalized
		}
		target.TOTPSecret = totp.Secret
		target.TOTPAlgorithm = totp.Algorithm
		target.TOTPDigits = totp.Digits
		target.TOTPPeriod = totp.Period
	}
	target.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := saveCredentials(agentID, creds); err != nil {
		return nil, err
	}

	return maskCredential(target), nil
}

// DeleteCredential removes a credential by ID.
func DeleteCredential(agentID, credID string) error {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return err
	}

	found := false
	filtered := make([]*Credential, 0, len(creds))
	for _, c := range creds {
		if c.ID == credID {
			found = true
			continue
		}
		filtered = append(filtered, c)
	}
	if !found {
		return fmt.Errorf("credential not found: %s", credID)
	}

	return saveCredentials(agentID, filtered)
}

// RevealPassword returns the plaintext password for a credential.
func RevealPassword(agentID, credID string) (string, error) {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return "", err
	}

	for _, c := range creds {
		if c.ID == credID {
			return c.Password, nil
		}
	}
	return "", fmt.Errorf("credential not found: %s", credID)
}

// GetTOTPCode generates the current TOTP code for a credential.
func GetTOTPCode(agentID, credID string) (string, int64, error) {
	credMu.Lock()
	defer credMu.Unlock()

	creds, err := loadCredentials(agentID)
	if err != nil {
		return "", 0, err
	}

	for _, c := range creds {
		if c.ID == credID {
			if c.TOTPSecret == "" {
				return "", 0, fmt.Errorf("no TOTP secret configured for credential: %s", credID)
			}
			return GenerateTOTPCode(c.TOTPSecret, c.TOTPAlgorithm, c.TOTPDigits, c.TOTPPeriod)
		}
	}
	return "", 0, fmt.Errorf("credential not found: %s", credID)
}
