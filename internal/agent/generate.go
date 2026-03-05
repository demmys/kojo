package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const geminiModel = "gemini-2.5-flash"
const geminiAPI = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// GenerateName generates a character name using Gemini API based on persona description.
func GenerateName(persona string, userPrompt string) (string, error) {
	apiKey, err := loadGeminiAPIKey()
	if err != nil {
		return "", err
	}

	prompt := "以下の人格設定にふさわしいキャラクター名を1つだけ生成して。名前のみ出力。\n\n人格: " + persona
	if userPrompt != "" {
		prompt += "\n\n追加要望: " + userPrompt
	}

	result, err := callGemini(apiKey, prompt)
	if err != nil {
		return "", err
	}

	// Clean up the result - trim whitespace and quotes
	name := strings.TrimSpace(result)
	name = strings.Trim(name, "\"「」")
	return name, nil
}

// loadGeminiAPIKey reads the API key from nanobanana credentials file.
func loadGeminiAPIKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot get home dir: %w", err)
	}

	credPath := filepath.Join(home, ".config", "nanobanana", "credentials")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return "", fmt.Errorf("cannot read credentials at %s: %w", credPath, err)
	}

	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("empty API key in %s", credPath)
	}
	return key, nil
}

// callGemini makes a simple text generation request to the Gemini API.
func callGemini(apiKey string, prompt string) (string, error) {
	url := fmt.Sprintf(geminiAPI, geminiModel, apiKey)

	body := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]string{
					{"text": prompt},
				},
			},
		},
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	resp, err := http.Post(url, "application/json", strings.NewReader(string(bodyJSON)))
	if err != nil {
		return "", fmt.Errorf("gemini API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gemini API decode error: %w", err)
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini API returned no content")
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}
