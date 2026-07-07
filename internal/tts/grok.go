package tts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// grokTTSBase is the xAI REST API base for TTS. Overridable in tests so we
// can point synthesis and the voice-catalog fetch at an httptest stub
// instead of the live api.x.ai.
var grokTTSBase = "https://api.x.ai"

// DefaultGrokVoice is the xAI TTS voice used when an agent on the grok
// provider has no explicit voice. Matches the xAI docs default.
const DefaultGrokVoice = "eve"

// GrokVoiceInfo mirrors an entry from GET /v1/tts/voices. Gender is
// "male" / "female" / "" as reported by the API.
type GrokVoiceInfo struct {
	Name   string `json:"name"`             // voice_id, e.g. "eve"
	Label  string `json:"label"`            // display name, e.g. "Eve"
	Gender string `json:"gender,omitempty"` // "male" | "female" | ""
}

// GrokVoiceCatalog is the built-in xAI TTS voice list captured from
// GET /v1/tts/voices (2026-07). Used as the validation allow-list and as
// the fallback catalog when a live fetch is unavailable (no API key).
// Custom cloned voices are not in this list; see IsValidGrokVoice.
var GrokVoiceCatalog = []GrokVoiceInfo{
	{"altair", "Altair", "male"},
	{"ara", "Ara", "female"},
	{"atlas", "Atlas", "male"},
	{"carina", "Carina", "female"},
	{"castor", "Castor", "male"},
	{"celeste", "Celeste", "female"},
	{"cosmo", "Cosmo", "male"},
	{"eve", "Eve", "female"},
	{"helios", "Helios", "male"},
	{"helix", "Helix", "male"},
	{"iris", "Iris", "female"},
	{"kepler", "Kepler", "male"},
	{"leo", "Leo", "male"},
	{"lumen", "Lumen", "male"},
	{"luna", "Luna", "female"},
	{"lux", "Lux", "male"},
	{"naksh", "Naksh", "male"},
	{"orion", "Orion", "male"},
	{"perseus", "Perseus", "male"},
	{"rex", "Rex", "male"},
	{"rigel", "Rigel", "male"},
	{"sal", "Sal", "male"},
	{"sirius", "Sirius", "male"},
	{"ursa", "Ursa", "female"},
	{"zagan", "Zagan", "male"},
	{"zenith", "Zenith", "male"},
}

// IsValidGrokVoice reports whether name is acceptable as a grok voice_id.
// Built-in voice ids are matched case-insensitively against the catalog.
// Custom cloned voice ids are not enumerable here, so any other non-empty
// token within a sane length/charset (letters, digits, '-', '_') is also
// accepted; a genuinely bogus id surfaces as an API error at synth time.
func IsValidGrokVoice(name string) bool {
	if name == "" {
		return false
	}
	if isBuiltinGrokVoice(name) {
		return true
	}
	if len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// isBuiltinGrokVoice reports whether name is one of the built-in xAI voice
// ids (matched case-insensitively).
func isBuiltinGrokVoice(name string) bool {
	lower := strings.ToLower(name)
	for _, v := range GrokVoiceCatalog {
		if v.Name == lower {
			return true
		}
	}
	return false
}

// grokVoiceCache memoizes the live /v1/tts/voices catalog for the process
// so the capability endpoint doesn't re-hit xAI on every settings load.
var (
	grokVoiceMu      sync.Mutex
	grokVoiceCached  []GrokVoiceInfo
	grokVoiceExpires time.Time
	grokVoiceKey     string // scope: which apiKey+base the cache belongs to
)

// grokVoiceTTL bounds how long a fetched catalog is reused before a
// refresh. The built-in list changes rarely.
const grokVoiceTTL = 6 * time.Hour

// GrokVoices returns the xAI voice catalog, fetching it from
// GET /v1/tts/voices and caching it in-process. On any fetch failure it
// falls back to the static GrokVoiceCatalog so the UI always has a list.
func GrokVoices(ctx context.Context, apiKey string) []GrokVoiceInfo {
	// Scope the cache to the credential + endpoint so a key/account switch
	// (or a test pointing at a stub base) never serves the wrong catalog.
	scope := grokTTSBase + "\x00" + hashKey(apiKey)

	grokVoiceMu.Lock()
	if grokVoiceCached != nil && grokVoiceKey == scope && time.Now().Before(grokVoiceExpires) {
		out := grokVoiceCached
		grokVoiceMu.Unlock()
		return out
	}
	grokVoiceMu.Unlock()

	fetched, err := fetchGrokVoices(ctx, apiKey)
	if err != nil || len(fetched) == 0 {
		return GrokVoiceCatalog
	}
	grokVoiceMu.Lock()
	grokVoiceCached = fetched
	grokVoiceKey = scope
	grokVoiceExpires = time.Now().Add(grokVoiceTTL)
	grokVoiceMu.Unlock()
	return fetched
}

// hashKey returns a short, non-reversible tag for an API key so it can be
// used as a cache-scope discriminator without holding the raw secret.
func hashKey(k string) string {
	if k == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:8])
}

func fetchGrokVoices(ctx context.Context, apiKey string) ([]GrokVoiceInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokTTSBase+"/v1/tts/voices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok voices http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Voices []struct {
			VoiceID string `json:"voice_id"`
			Name    string `json:"name"`
			Gender  string `json:"gender"`
		} `json:"voices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]GrokVoiceInfo, 0, len(parsed.Voices))
	for _, v := range parsed.Voices {
		if v.VoiceID == "" {
			continue
		}
		out = append(out, GrokVoiceInfo{Name: v.VoiceID, Label: v.Name, Gender: v.Gender})
	}
	return out, nil
}

// callGrok posts a single POST /v1/tts request and returns the raw audio
// bytes together with the concrete container format ("mp3" or "wav").
//
// Style handling: xAI TTS has NO free-text style parameter. Delivery is
// controlled by voice choice and inline speech tags embedded in the text
// itself ([pause], [laugh], <whisper>…</whisper>, etc.). The agent's
// StylePrompt is therefore NOT sent — prepending it would make the model
// read the instruction aloud, which is exactly the bug this provider is
// meant to avoid. See tts_handlers/AgentSettings for the UI note.
func (s *Service) callGrok(ctx context.Context, voice, text, format string) ([]byte, string, error) {
	if s.getXAIKey == nil {
		return nil, "", fmt.Errorf("xai api key not configured")
	}
	apiKey, err := s.getXAIKey()
	if err != nil {
		return nil, "", fmt.Errorf("xai api key: %w", err)
	}
	if voice == "" {
		voice = DefaultGrokVoice
	}

	// Grok returns the requested container directly. Ask for wav only when
	// the caller wants wav; otherwise mp3 (universally playable, and the
	// smaller of the two). opus is not offered by the REST endpoint.
	codec := "mp3"
	if format == "wav" {
		codec = "wav"
	}
	outFmt := map[string]any{
		"codec":       codec,
		"sample_rate": 24000,
	}
	if codec == "mp3" {
		outFmt["bit_rate"] = 128000
	}
	payload := map[string]any{
		"text":     text,
		"voice_id": voice,
		// "auto" lets xAI detect the language so mixed JA/EN notification
		// text (code, paths, logs) is read correctly rather than forced to
		// one locale.
		"language":      "auto",
		"output_format": outFmt,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grokTTSBase+"/v1/tts", strings.NewReader(string(buf)))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("grok request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("grok tts http %d: %s", resp.StatusCode, truncate(string(body), 400))
	}
	if len(body) == 0 {
		return nil, "", fmt.Errorf("grok tts returned empty audio")
	}
	return body, codec, nil
}
