package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SynthesizeRequest is the input to Service.Synthesize. All fields are
// already sanitized at this layer — callers pass agent-derived
// configuration directly.
type SynthesizeRequest struct {
	Provider    string // "" or "gemini" = Gemini; "grok" = xAI Grok
	Model       string // "" = DefaultModel (gemini only; ignored for grok)
	Voice       string // "" = DefaultVoice
	StylePrompt string // "" = DefaultStylePrompt (gemini only; unused for grok)
	Text        string // raw, will be sanitized inside Synthesize
	Format      string // "opus" | "mp3" | "wav"
}

// SynthesizeResult is what Service.Synthesize returns. AudioBytes is the
// fully encoded payload ready to be served as MimeType(Format). Hash is
// the cache key (hex sha256) so handlers can build the audio URL.
type SynthesizeResult struct {
	Hash       string
	Format     string
	AudioBytes []byte
	Cached     bool
}

// Service performs Gemini TTS synthesis and caches results on disk.
//
// The API key is fetched lazily via getAPIKey on every request, so a key
// rotation in the credential store takes effect on the next call without
// needing to rewire the service.
type Service struct {
	getAPIKey func() (string, error) // Gemini key
	getXAIKey func() (string, error) // xAI (Grok) key
	http      *http.Client
}

// NewService constructs a Service. geminiKeyFn returns the current Gemini
// Developer API key; xaiKeyFn returns the current xAI (Grok) API key.
// Either may be nil if that provider is not configured — the missing key
// surfaces as an error on the corresponding synthesize path.
func NewService(geminiKeyFn, xaiKeyFn func() (string, error)) *Service {
	return &Service{
		getAPIKey: geminiKeyFn,
		getXAIKey: xaiKeyFn,
		http: &http.Client{
			// 60 s covers TTS for the largest accepted input.
			Timeout: 60 * time.Second,
		},
	}
}

// Synthesize is the main entry point. The flow is:
//  1. Sanitize and validate input.
//  2. Hash the request and return the cached file if present.
//  3. Call Gemini: the Interactions API for 3.8 models, otherwise
//     :generateContent with safetySettings=OFF.
//  4. Decode the returned audio (raw 24 kHz LE16 PCM).
//  5. Encode to the requested container (ffmpeg for opus/mp3, in-process
//     WAV header for wav).
//  6. Persist to cache and return.
func (s *Service) Synthesize(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	if s == nil {
		return nil, errors.New("tts service not initialized")
	}
	provider := req.Provider
	if provider == "" {
		provider = ProviderGemini
	}
	if provider == ProviderGrok {
		return s.synthesizeGrok(ctx, req)
	}
	if provider != ProviderGemini {
		return nil, fmt.Errorf("invalid provider: %q", provider)
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	if !IsValidModel(model) {
		return nil, fmt.Errorf("invalid model: %q", model)
	}
	voice := req.Voice
	if voice == "" {
		voice = DefaultVoice
	}
	if !IsValidVoice(voice) {
		return nil, fmt.Errorf("invalid voice: %q", voice)
	}
	style := strings.TrimSpace(req.StylePrompt)
	if style == "" {
		style = DefaultStylePrompt
	}
	format := req.Format
	if format == "" {
		format = "wav"
	}
	switch format {
	case "opus", "mp3":
		if !FFmpegAvailable() {
			format = "wav" // graceful degrade
		}
	case "wav":
		// always supported
	default:
		return nil, fmt.Errorf("invalid format: %q", format)
	}

	text := Sanitize(req.Text)
	if text == "" {
		return nil, errors.New("empty text after sanitize")
	}

	hash := hashRequest(ProviderGemini, model, voice, style, text, format, true /* relax/uncensored */)
	if data, ok := cacheGet(hash, format); ok {
		return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: data, Cached: true}, nil
	}

	pcm, err := s.callGemini(ctx, model, voice, style, text)
	if err != nil {
		return nil, err
	}

	// Encoding ladder: try the requested format, fall through to the
	// next-smallest container that the server can produce. Opus → MP3 →
	// WAV. WAV is the in-process header path and never fails.
	var audio []byte
	tryFormats := []string{format}
	if format == "opus" {
		tryFormats = append(tryFormats, "mp3", "wav")
	} else if format == "mp3" {
		tryFormats = append(tryFormats, "wav")
	}
	for _, f := range tryFormats {
		switch f {
		case "wav":
			audio = pcmToWAV(pcm, 24000)
			format = "wav"
		case "opus", "mp3":
			b, encErr := EncodeFFmpeg(ctx, f, pcm, 24000)
			if encErr != nil {
				continue
			}
			audio, format = b, f
		}
		if audio != nil {
			break
		}
	}
	if audio == nil {
		// Should be unreachable — the WAV branch always populates audio.
		return nil, errors.New("encode pipeline produced no audio")
	}
	// Hash is keyed on the *final* format so cache lookups by format
	// land on the right file.
	hash = hashRequest(ProviderGemini, model, voice, style, text, format, true)

	if err := cachePut(hash, format, audio); err != nil {
		// Cache write failures are non-fatal; we still return the audio.
		_ = err
	}
	return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: audio, Cached: false}, nil
}

// Provider identifiers for TTSConfig / SynthesizeRequest.
const (
	ProviderGemini = "gemini"
	ProviderGrok   = "grok"
)

// synthesizeGrok is the xAI Grok synthesis path. Grok returns an encoded
// container (mp3 or wav) directly, so there is no PCM/ffmpeg step — the
// bytes are cached as-is. The style prompt is intentionally not used (see
// callGrok).
func (s *Service) synthesizeGrok(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	voice := req.Voice
	if voice == "" {
		voice = DefaultGrokVoice
	}
	if !IsValidGrokVoice(voice) {
		return nil, fmt.Errorf("invalid grok voice: %q", voice)
	}
	// Grok emits mp3 or wav only; opus requests map to mp3.
	format := req.Format
	switch format {
	case "wav":
	case "opus", "mp3", "":
		format = "mp3"
	default:
		return nil, fmt.Errorf("invalid format: %q", format)
	}

	text := Sanitize(req.Text)
	if text == "" {
		return nil, errors.New("empty text after sanitize")
	}

	// Cache key: provider-scoped, style omitted (grok ignores it), model
	// omitted (single TTS model). Built-in voice ids are lowercased so
	// "Eve"/"eve" share a cache entry; custom voice ids are case-sensitive
	// on xAI's side, so keep them verbatim to avoid Foo/foo collisions.
	keyVoice := voice
	if isBuiltinGrokVoice(voice) {
		keyVoice = strings.ToLower(voice)
	}
	hash := hashRequest(ProviderGrok, "", keyVoice, "", text, format, true)
	if data, ok := cacheGet(hash, format); ok {
		return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: data, Cached: true}, nil
	}

	audio, gotFormat, err := s.callGrok(ctx, voice, text, format)
	if err != nil {
		return nil, err
	}
	if gotFormat != format {
		// Defensive: rehash on the actual container so the /audio lookup
		// resolves the right extension.
		format = gotFormat
		hash = hashRequest(ProviderGrok, "", keyVoice, "", text, format, true)
	}
	if err := cachePut(hash, format, audio); err != nil {
		_ = err // non-fatal, mirror the gemini path
	}
	return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: audio, Cached: false}, nil
}

// LookupCached returns the bytes for a previously synthesized hash. It is
// used by the GET /audio endpoint to serve the file directly with a long
// browser cache. format must match the on-disk extension.
func (s *Service) LookupCached(hash, format string) ([]byte, bool) {
	return cacheGet(hash, format)
}

// geminiAPIBase is the Gemini Developer API root. Tests point it at an
// httptest stub.
var geminiAPIBase = "https://generativelanguage.googleapis.com"

// callGemini posts a single :generateContent request with safetySettings
// fully disabled, parses the inline_data audio block, and returns raw PCM.
func (s *Service) callGemini(ctx context.Context, model, voice, style, text string) ([]byte, error) {
	if s.getAPIKey == nil {
		return nil, errors.New("gemini api key: not configured")
	}
	apiKey, err := s.getAPIKey()
	if err != nil {
		return nil, fmt.Errorf("gemini api key: %w", err)
	}
	if usesInteractionsAPI(model) {
		return s.callGeminiInteractions(ctx, model, apiKey, voice, style, text)
	}

	// All four adjustable categories set to OFF, plus BLOCK_NONE as the
	// schema-compatible fallback in case OFF is rejected by the model.
	const off = "OFF"
	type harm struct {
		Category  string `json:"category"`
		Threshold string `json:"threshold"`
	}
	type voiceCfg struct {
		PrebuiltVoiceConfig struct {
			VoiceName string `json:"voiceName"`
		} `json:"prebuiltVoiceConfig"`
	}
	type speech struct {
		VoiceConfig voiceCfg `json:"voiceConfig"`
	}
	type genCfg struct {
		ResponseModalities []string `json:"responseModalities"`
		SpeechConfig       speech   `json:"speechConfig"`
	}
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	type body struct {
		Contents         []content `json:"contents"`
		GenerationConfig genCfg    `json:"generationConfig"`
		SafetySettings   []harm    `json:"safetySettings"`
	}

	// Gemini TTS models reject the `systemInstruction` field
	// ("Developer instruction is not enabled for this model"). Inline
	// the narrator framing and the agent's style prompt into the single
	// user turn instead.
	prompt := SystemInstruction + "\n\n" + style + "\n\n" + text

	mkBody := func(threshold string) body {
		b := body{
			Contents: []content{{
				Role:  "user",
				Parts: []part{{Text: prompt}},
			}},
			GenerationConfig: genCfg{
				ResponseModalities: []string{"AUDIO"},
			},
			SafetySettings: []harm{
				{"HARM_CATEGORY_HARASSMENT", threshold},
				{"HARM_CATEGORY_HATE_SPEECH", threshold},
				{"HARM_CATEGORY_SEXUALLY_EXPLICIT", threshold},
				{"HARM_CATEGORY_DANGEROUS_CONTENT", threshold},
			},
		}
		b.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName = voice
		return b
	}

	pcm, err := s.doGemini(ctx, model, apiKey, mkBody(off))
	if err != nil && rejectsSafetyThreshold(err) {
		// Some model surfaces don't accept the `OFF` safety threshold and
		// reject the whole request with HTTP 400 / status INVALID_ARGUMENT.
		// Retry with BLOCK_NONE (the next-loosest documented threshold)
		// only for that specific rejection — never for other 4xx/5xx or
		// decode failures that happen to contain the word "invalid".
		pcm, err = s.doGemini(ctx, model, apiKey, mkBody("BLOCK_NONE"))
	}
	return pcm, err
}

// apiError carries the structured Gemini (Google API) error envelope so
// callers can branch on the canonical status code instead of substring-
// matching a formatted message. Gemini returns errors as:
//
//	{"error": {"code": 400, "message": "...", "status": "INVALID_ARGUMENT"}}
//
// where `status` is a google.rpc.Code name.
type apiError struct {
	HTTPStatus int
	Status     string // canonical status name, e.g. "INVALID_ARGUMENT"
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("gemini http %d (%s): %s", e.HTTPStatus, e.Status, truncate(e.Message, 400))
}

// rejectsSafetyThreshold reports whether err is the specific API rejection
// of the requested safety threshold: HTTP 400 with canonical status
// INVALID_ARGUMENT. This is the only condition the BLOCK_NONE retry was
// designed for.
func rejectsSafetyThreshold(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.HTTPStatus == http.StatusBadRequest && apiErr.Status == "INVALID_ARGUMENT"
}

func (s *Service) doGemini(ctx context.Context, model, apiKey string, payload any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := geminiAPIBase + "/v1beta/models/" + model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Prefer header auth — keeps the key out of access logs.
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MiB cap
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}

	type respPart struct {
		InlineData *struct {
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
		} `json:"inlineData,omitempty"`
	}
	type respContent struct {
		Parts []respPart `json:"parts"`
	}
	type candidate struct {
		Content      respContent `json:"content"`
		FinishReason string      `json:"finishReason"`
	}
	type promptFeedback struct {
		BlockReason string `json:"blockReason"`
	}
	var parsed struct {
		Candidates     []candidate    `json:"candidates"`
		PromptFeedback promptFeedback `json:"promptFeedback"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("gemini decode: %w (body=%s)", err, truncate(string(body), 200))
	}
	if parsed.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini blocked: %s", parsed.PromptFeedback.BlockReason)
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("gemini returned no candidates")
	}
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.InlineData != nil && p.InlineData.Data != "" {
			pcm, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("base64 decode: %w", err)
			}
			return pcm, nil
		}
	}
	if reason := parsed.Candidates[0].FinishReason; reason != "" && reason != "STOP" {
		return nil, fmt.Errorf("gemini finish=%s without audio", reason)
	}
	return nil, errors.New("gemini returned no audio data")
}

// parseAPIError turns a non-200 Gemini response into an *apiError.
// :generateContent returns the google.rpc envelope
// ({"error":{"code":400,"message":...,"status":"INVALID_ARGUMENT"}}); the
// Interactions API returns {"error":{"code":"invalid_request","message":...}}
// with a string code and no status. Both decode here; the raw body is the
// fallback message when neither shape parses.
func parseAPIError(httpStatus int, body []byte) *apiError {
	var env struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Status  string          `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	status := env.Error.Status
	if status == "" {
		var code string
		if json.Unmarshal(env.Error.Code, &code) == nil {
			status = code
		}
	}
	msg := env.Error.Message
	if msg == "" {
		msg = string(body)
	}
	return &apiError{HTTPStatus: httpStatus, Status: status, Message: msg}
}

// callGeminiInteractions synthesizes via POST /v1beta/interactions, the
// surface Gemini 3.8 TTS is documented on. The text goes in verbatim and
// the style prompt rides in a speech_metadata annotation so it steers the
// delivery without being spoken. The narrator SystemInstruction is not
// sent: 3.8 never rewrites or answers the transcript, and system
// instructions are rejected ("Developer instruction is not enabled"). The
// Gemini API does not accept safety_settings on this surface either.
// Headerless 24 kHz LE16 PCM is requested explicitly because the unary
// default is a RIFF WAV.
func (s *Service) callGeminiInteractions(ctx context.Context, model, apiKey, voice, style, text string) ([]byte, error) {
	type annotation struct {
		Type  string `json:"type"`
		Style string `json:"style,omitempty"`
	}
	type textPart struct {
		Type        string       `json:"type"`
		Text        string       `json:"text"`
		Annotations []annotation `json:"annotations,omitempty"`
	}
	type turn struct {
		Type    string     `json:"type"`
		Content []textPart `json:"content"`
	}
	type respFormat struct {
		Type       string `json:"type"`
		MimeType   string `json:"mime_type"`
		SampleRate int    `json:"sample_rate"`
	}
	type speechCfg struct {
		Voice string `json:"voice"`
	}
	type genCfg struct {
		SpeechConfig []speechCfg `json:"speech_config"`
	}
	type body struct {
		Model            string     `json:"model"`
		Input            []turn     `json:"input"`
		ResponseFormat   respFormat `json:"response_format"`
		GenerationConfig genCfg     `json:"generation_config"`
	}
	part := textPart{Type: "text", Text: text}
	if style != "" {
		part.Annotations = []annotation{{Type: "speech_metadata", Style: style}}
	}
	payload := body{
		Model:            model,
		Input:            []turn{{Type: "user_input", Content: []textPart{part}}},
		ResponseFormat:   respFormat{Type: "audio", MimeType: "audio/l16", SampleRate: 24000},
		GenerationConfig: genCfg{SpeechConfig: []speechCfg{{Voice: voice}}},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiAPIBase+"/v1beta/interactions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MiB cap
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, respBody)
	}

	var parsed struct {
		Status string `json:"status"`
		Steps  []struct {
			Content []struct {
				Type       string `json:"type"`
				MimeType   string `json:"mime_type"`
				SampleRate int    `json:"sample_rate"`
				Data       string `json:"data"`
			} `json:"content"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("gemini decode: %w (body=%s)", err, truncate(string(respBody), 200))
	}
	// Reject anything but a completed interaction up front: an
	// "incomplete" response can still carry truncated audio, which must
	// not be served or cached as a success.
	if parsed.Status != "" && parsed.Status != "completed" {
		return nil, fmt.Errorf("gemini interaction status=%s", parsed.Status)
	}
	// The convenience output_audio is the *last* audio block; mirror that.
	for i := len(parsed.Steps) - 1; i >= 0; i-- {
		content := parsed.Steps[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			c := content[j]
			if c.Type != "audio" || c.Data == "" {
				continue
			}
			if !strings.HasPrefix(c.MimeType, "audio/l16") {
				return nil, fmt.Errorf("gemini returned unexpected audio mime %q", c.MimeType)
			}
			if c.SampleRate != 0 && c.SampleRate != 24000 {
				return nil, fmt.Errorf("gemini returned unexpected sample rate %d", c.SampleRate)
			}
			pcm, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				return nil, fmt.Errorf("base64 decode: %w", err)
			}
			return pcm, nil
		}
	}
	return nil, errors.New("gemini returned no audio data")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
