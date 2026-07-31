package agent

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/thumbnail"
)

// Sentinel errors for ValidateTempAvatarPath, allowing callers to map to
// appropriate HTTP status codes.
var (
	ErrAvatarInternal         = errors.New("cannot resolve temp dir")
	ErrAvatarNotFound         = errors.New("file not found")
	ErrAvatarUnsupportedImage = errors.New("unsupported image format")
)

// AvatarGenerationError classifies provider failures so the HTTP layer can
// distinguish configuration, throttling, moderation, and timeout failures.
type AvatarGenerationError struct {
	Code       string
	HTTPStatus int
	Err        error
}

func (e *AvatarGenerationError) Error() string { return e.Err.Error() }
func (e *AvatarGenerationError) Unwrap() error { return e.Err }

// allowedImageExts is the set of image extensions accepted for avatars.
var allowedImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".svg": true,
}

// avatarExtProbe is the set of extensions probed (and cleaned) when
// resolving an agent's stored avatar. Order is the same as the legacy
// disk path: an agent that has both `avatar.png` and `avatar.svg`
// (legitimately disallowed by SaveAvatar but possible from a
// hand-edited blob tree) presents the .png first to keep behaviour
// stable across the cutover. The list is also used by SaveAvatar to
// know what to delete after publishing the new extension.
var avatarExtProbe = []string{".png", ".jpg", ".jpeg", ".webp", ".svg"}

// avatarMu serializes avatar operations per agent.
//
// Writers (SaveAvatar / DeleteAvatar) take Lock() so the
// "Put replacement, then delete other extensions" sequence is
// atomically by concurrent callers — without this two parallel
// uploads at different extensions could interleave their delete and
// put calls and end up with multiple avatar rows surviving, with
// resolveAvatarBlob's probe order silently picking one and shadowing
// the other.
//
// Readers (ServeAvatar) take RLock() so they observe a consistent
// pair of (file body, blob_refs ETag/ModTime). blob.Store.Put
// internally rename's the temp file before updating the blob_refs
// row, so without this serialization a concurrent ServeAvatar could
// open the new body but read the old refs row's digest — sending
// the client a Content-Length / SHA mismatch. RLock allows multiple
// concurrent reads (the common case is many tabs hitting GET
// /agents/<id>/avatar) while a writer is exclusive.
//
// Lock entries are process-local and intentionally NEVER reclaimed:
// removing an entry while a goroutine waits on the old mutex would
// silently let a new entry be created and break the serialization
// invariant. The leak is bounded by total agent ids ever created
// (one *sync.RWMutex per id, ~24 bytes), matching the same pattern
// used by Manager.patchMus.
var avatarLocks keyedRWMutex

func acquireAvatarLock(agentID string) func() {
	return avatarLocks.Lock(agentID)
}

func acquireAvatarRLock(agentID string) func() {
	return avatarLocks.RLock(agentID)
}

// IsAllowedImageExt returns true if ext (case-insensitive) is an accepted avatar image extension.
func IsAllowedImageExt(ext string) bool {
	return allowedImageExts[strings.ToLower(ext)]
}

// avatarBlobPath returns the logical path under blob.ScopeGlobal for
// an agent's avatar at the given extension. Centralised so the URI
// scheme (`agents/<id>/avatar.<ext>`) is defined in exactly one place
// — the migration importer (internal/migrate/importers/blobs.go) and
// the runtime read/write paths must agree byte-for-byte or
// post-migration installs would silently miss their avatars.
func avatarBlobPath(agentID, ext string) string {
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return "agents/" + agentID + "/avatar" + ext
}

// resolveAvatarBlob probes blob.ScopeGlobal for the first existing
// avatar.<ext> file and returns (ext, object, ok). Returns ok=false
// if no avatar is published or the blob store is nil. The probe runs
// blob.Head per extension (Lstat + cache lookup, no body read), so
// cost is the same as the legacy os.Stat loop but with
// blob_refs-backed ETag included in the returned object.
//
// On success ext includes the leading dot ("." + e.g. "png"). The
// object's ETag is the strong "sha256:<hex>" form when blob_refs is
// wired; tests with a bare blob.Store get ETag="" which callers
// fall back to in avatarMeta.
func resolveAvatarBlob(bs *blob.Store, agentID string) (string, *blob.Object, bool) {
	if bs == nil {
		return "", nil, false
	}
	for _, ext := range avatarExtProbe {
		obj, err := bs.Head(blob.ScopeGlobal, avatarBlobPath(agentID, ext))
		if err == nil && obj != nil {
			return ext, obj, true
		}
	}
	return "", nil, false
}

// avatarMeta returns whether an avatar blob exists and a content-
// derived hash for ETag use. Cache key is the blob_refs sha256 (via
// resolveAvatarBlob) so a re-upload that produces an identical body
// also produces an identical hash — matching the design contract
// for a content-addressed blob store. When the blob store is wired
// without refs (slice-1 path / unit tests), or the row hasn't been
// backfilled, we fall back to a ModTime-derived hash so freshly
// published avatars still defeat HTTP caches.
func (m *Manager) avatarMeta(agentID string) (exists bool, hash string) {
	// Hold the read side of the avatar lock so a concurrent
	// SaveAvatar's rename → blob_refs.Put gap can't surface a
	// hash derived from the new body's ETag-from-old-row state.
	// Brief: resolveAvatarBlob calls blob.Head which reads both
	// the on-disk file (for size/modtime) and the blob_refs row
	// (for ETag); without the lock those two reads can straddle a
	// concurrent Put.
	runlock := acquireAvatarRLock(agentID)
	defer runlock()
	_, obj, ok := resolveAvatarBlob(m.blobStore, agentID)
	if !ok {
		return false, ""
	}
	if obj.ETag != "" {
		// Strip the "sha256:" prefix; the public AvatarHash field
		// has historically been a bare hex string and the Web UI
		// embeds it in the avatar URL's cache-bust query param
		// (?t=<hash> in AgentAvatar.tsx) — the bare-hex form
		// preserves backward compatibility with v0 consumers.
		return true, strings.TrimPrefix(obj.ETag, "sha256:")
	}
	return true, fmt.Sprintf("%x", obj.ModTime)
}

// applyAvatarMeta sets HasAvatar and AvatarHash on the agent from pre-fetched values.
// Falls back to UpdatedAt as hash when no avatar exists.
// Call avatarMeta(id) outside any lock to get has/hash, then apply under lock.
func applyAvatarMeta(a *Agent, has bool, hash string) {
	a.HasAvatar = has
	a.AvatarHash = hash
	if !has {
		a.AvatarHash = a.UpdatedAt
	}
}

// ServeAvatar serves the agent's avatar image, falling back to a
// generated SVG when no avatar is published or the blob store
// hasn't been wired (test fixture). Uses http.ServeContent so
// conditional GET (If-Modified-Since / If-None-Match) and Range
// requests work the same way they did in the v0 http.ServeFile path.
//
// Content-Type is inferred from the file extension; svg is special-
// cased because Go's mime package returns "image/svg+xml" but only
// when the system mime database has been initialized, which can't
// be relied on across all deploy targets.
func ServeAvatar(bs *blob.Store, w http.ResponseWriter, r *http.Request, a *Agent) {
	// ?size=<n>: when present AND the avatar is a raster image the
	// thumbnail package can decode, serve a cached JPEG thumbnail
	// instead of the full blob. SVG avatars (both uploaded and the
	// generated-fallback) skip this path — they're resolution-
	// independent and tiny.
	thumbSize := 0
	if s := r.URL.Query().Get("size"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			thumbSize = n
		}
	}

	// Hold the per-agent avatar read lock JUST across the resolve →
	// Open → header-snapshot window so a concurrent SaveAvatar's
	// rename → blob_refs.Put gap can't surface a fresh body with a
	// stale ETag. The lock is released BEFORE http.ServeContent
	// streams the body — once we have an open *os.File the kernel
	// holds the inode, so a subsequent SaveAvatar that rename's a
	// different file into the same path doesn't disturb our stream.
	// Holding the read lock across the body write would let a slow
	// HTTP client (mobile uplink, attacker-throttled) starve
	// SaveAvatar / DeleteAvatar.
	f, ext, etag, fsPath, modTime, ok := openAvatarForServe(bs, a.ID)
	if ok {
		defer f.Close()

		// Thumbnail path: raster image + size requested. fsPath was
		// resolved under the avatar read lock by openAvatarForServe
		// so it points at the same inode as f.
		if thumbSize > 0 && fsPath != "" && thumbnail.IsSupportedExt(ext) {
			if err := thumbnail.ServeHTTP(w, r, fsPath, thumbSize); err == nil {
				return
			}
			// Generation failed — fall through to full body.
		}

		// Full-size path. Override the middleware's no-store with
		// no-cache so the browser stores the body but revalidates
		// via ETag on every request → 304 when unchanged.
		ctype := contentTypeForAvatarExt(ext)
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		if etag != "" {
			w.Header().Set("ETag", `"`+etag+`"`)
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "avatar"+ext, modTime, f)
		return
	}
	// Fall through to SVG fallback on either no-avatar OR Open
	// failure — a missing blob between Head and Open means a
	// concurrent SaveAvatar/DeleteAvatar dropped the row, and
	// serving the legacy initials avatar is preferable to a 500.
	//
	// Cache-Control is left to apiNoStoreDefaultMiddleware's
	// `no-store` seed: the SVG is generated from a.Name, so a
	// rename or first avatar upload would otherwise leave the
	// previous initials cached for up to an hour on the same
	// /api/v1/agents/{id}/avatar URL. The SVG body is a few
	// hundred bytes — re-fetching on every request is cheaper
	// than getting wrong content stuck in a browser cache.
	svg := generateSVGAvatar(a.Name)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(svg))
}

// openAvatarForServe is ServeAvatar's lock-scoped helper: it takes
// the per-agent avatar read lock, resolves the published extension,
// opens the blob, snapshots the Object's ETag/ModTime, and returns
// the open file handle to the caller. The read lock is released on
// return so the caller can stream the body without blocking writers.
//
// Returning an open *os.File past the lock release is safe: blob.
// Store.Put never truncates in place — it writes a temp file and
// rename's it onto the target — so an inode our caller is reading
// stays valid even after a concurrent re-Put removes the directory
// entry. Closing the returned handle is the caller's responsibility.
func openAvatarForServe(bs *blob.Store, agentID string) (f *os.File, ext, etag, fsPath string, modTime time.Time, ok bool) {
	runlock := acquireAvatarRLock(agentID)
	defer runlock()
	ext, _, found := resolveAvatarBlob(bs, agentID)
	if !found {
		return nil, "", "", "", time.Time{}, false
	}
	blobPath := avatarBlobPath(agentID, ext)
	f, obj, err := bs.Open(blob.ScopeGlobal, blobPath)
	if err != nil {
		return nil, "", "", "", time.Time{}, false
	}
	// Resolve the on-disk path while still holding the read lock so
	// thumbnail.Generate operates on the same inode that we opened.
	fp, _ := bs.FSPath(blob.ScopeGlobal, blobPath)
	return f, ext, obj.ETag, fp, time.UnixMilli(obj.ModTime), true
}

// contentTypeForAvatarExt maps an avatar extension to its MIME type.
// Defined here so ServeAvatar doesn't depend on the host's
// mime.types database (Go's mime.TypeByExtension reads /etc/mime.types
// on Linux, which a minimal container image may not ship).
func contentTypeForAvatarExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}

// SaveAvatar publishes an uploaded avatar to the blob store. ext is
// the leading-dot extension ("." + e.g. "png"); callers are
// responsible for validating it via IsAllowedImageExt before calling.
//
// Publishes the replacement first, then removes any pre-existing avatar at a
// different extension so a failed Put never destroys the current avatar. The
// agent presents exactly one avatar at a time — without this a user
// who first uploads avatar.png and then avatar.svg would have BOTH
// surface, with resolveAvatarBlob's probe order picking .png and
// silently discarding the new svg. The final single-avatar state matches v0,
// while the failure-safe operation order deliberately differs.
//
// Per-agent serialization (acquireAvatarLock) ensures the
// "put-then-delete" sequence is serialized — without it,
// two concurrent uploads at different extensions could interleave
// their delete/put calls and leave multiple rows in place.
//
// Failure posture: Put failures leave every previous extension untouched.
// Cleanup failures keep the durable replacement: blob.Delete may report an
// error after already removing the old file, so rolling the replacement back
// could otherwise lose both versions. A later upload/reset retries cleanup.
func SaveAvatar(bs *blob.Store, agentID string, src io.Reader, ext string) error {
	if bs == nil {
		return errors.New("avatar: blob store not configured")
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	unlock := acquireAvatarLock(agentID)
	defer unlock()

	if _, err := bs.Put(blob.ScopeGlobal, avatarBlobPath(agentID, ext), src, blob.PutOptions{}); err != nil {
		return fmt.Errorf("avatar: put: %w", err)
	}

	// Delete every other extension only after the replacement is durable.
	cleanupFailed := false
	for _, e := range avatarExtProbe {
		if e == ext {
			continue
		}
		if err := bs.Delete(blob.ScopeGlobal, avatarBlobPath(agentID, e), blob.DeleteOptions{}); err != nil && !errors.Is(err, blob.ErrNotFound) {
			cleanupFailed = true
			slog.Default().Warn("avatar: replacement published but old extension cleanup failed",
				"agent", agentID, "old_ext", e, "new_ext", ext, "err", err)
		}
	}
	if cleanupFailed {
		resolvedExt, _, ok := resolveAvatarBlob(bs, agentID)
		if !ok || resolvedExt != ext {
			return fmt.Errorf("avatar: replacement stored as %s but old extension still shadows it", ext)
		}
	}

	return nil
}

// DeleteAvatar removes every published avatar.* blob for an agent.
// Used by the reset and delete paths so a re-created agent sharing
// the same id starts with no avatar. ErrNotFound on individual
// extensions is folded into success — the post-condition is "no
// avatar blobs for this agent", which is already true for any
// extension that wasn't published.
//
// Acquires the per-agent avatar lock so a concurrent SaveAvatar
// can't observe a half-deleted state (some extensions cleaned, the
// new Put landed). See SaveAvatar for the rationale.
func DeleteAvatar(bs *blob.Store, agentID string) error {
	if bs == nil {
		return nil
	}
	unlock := acquireAvatarLock(agentID)
	defer unlock()
	for _, e := range avatarExtProbe {
		if err := bs.Delete(blob.ScopeGlobal, avatarBlobPath(agentID, e), blob.DeleteOptions{}); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("avatar: delete %s: %w", e, err)
		}
	}
	return nil
}

// ValidateTempAvatarPath validates that a path points to an image file inside
// a kojo-avatar-* temp directory. Returns the resolved absolute path or an error.
// Used by handlers that accept user-supplied avatar paths.
func ValidateTempAvatarPath(avatarPath string) (string, error) {
	absPath, err := filepath.EvalSymlinks(avatarPath)
	if err != nil {
		return "", fmt.Errorf("invalid avatar path")
	}
	tempDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return "", ErrAvatarInternal
	}
	if !strings.HasPrefix(absPath, tempDir+string(filepath.Separator)) {
		return "", fmt.Errorf("avatar path must be in temp directory")
	}
	rel, _ := filepath.Rel(tempDir, absPath)
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "kojo-avatar-") {
		return "", fmt.Errorf("invalid avatar path")
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	if !IsAllowedImageExt(ext) {
		return "", ErrAvatarUnsupportedImage
	}
	fi, err := os.Stat(absPath)
	if err != nil || !fi.Mode().IsRegular() {
		return "", ErrAvatarNotFound
	}
	return absPath, nil
}

// AvatarProvider identifies an upstream image-generation service.
type AvatarProvider string

const (
	AvatarProviderGemini AvatarProvider = "gemini"
	AvatarProviderOpenAI AvatarProvider = "openai"

	geminiImageModel        = "gemini-3.1-flash-image"
	geminiImagesEndpoint    = "https://generativelanguage.googleapis.com/v1beta/interactions"
	openAIImageModel        = "gpt-image-2"
	openAIImagesEndpoint    = "https://api.openai.com/v1/images/generations"
	avatarGenerationTimeout = 180 * time.Second
	maxAvatarResponseBytes  = 32 << 20
)

func extFromMimeType(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// GenerateAvatarWithAI chooses an available provider and generates an avatar.
// When both providers are configured, requestedProvider is required so a
// caller cannot silently change the billing provider.
func GenerateAvatarWithAI(
	ctx context.Context,
	creds *CredentialStore,
	requestedProvider string,
	persona string,
	name string,
	prompt string,
	logger *slog.Logger,
) (string, AvatarProvider, error) {
	provider, apiKey, err := resolveAvatarProvider(creds, requestedProvider)
	if err != nil {
		return "", "", err
	}
	imagePrompt := buildAvatarPrompt(name, persona, prompt)

	switch provider {
	case AvatarProviderOpenAI:
		path, err := generateAvatarWithOpenAI(ctx, apiKey, imagePrompt, http.DefaultClient, openAIImagesEndpoint, logger)
		return path, provider, err
	case AvatarProviderGemini:
		model := geminiImageModel
		if m := strings.TrimSpace(os.Getenv("KOJO_GEMINI_IMAGE_MODEL")); m != "" {
			model = m
		}
		path, err := generateAvatarWithGemini(ctx, apiKey, model, imagePrompt, http.DefaultClient, geminiImagesEndpoint, logger)
		return path, provider, err
	default:
		return "", "", fmt.Errorf("unsupported avatar provider %q", provider)
	}
}

func resolveAvatarProvider(creds *CredentialStore, requested string) (AvatarProvider, string, error) {
	geminiKey, geminiErr := LoadGeminiAPIKey(creds)
	openAIKey, openAIErr := LoadOpenAIAPIKey(creds)

	switch AvatarProvider(strings.ToLower(strings.TrimSpace(requested))) {
	case AvatarProviderGemini:
		if geminiErr != nil {
			return "", "", geminiErr
		}
		return AvatarProviderGemini, geminiKey, nil
	case AvatarProviderOpenAI:
		if openAIErr != nil {
			return "", "", openAIErr
		}
		return AvatarProviderOpenAI, openAIKey, nil
	case "":
		if geminiErr == nil && openAIErr == nil {
			return "", "", fmt.Errorf("provider is required when both Gemini and OpenAI API keys are configured")
		}
		if geminiErr == nil {
			return AvatarProviderGemini, geminiKey, nil
		}
		if openAIErr == nil {
			return AvatarProviderOpenAI, openAIKey, nil
		}
		return "", "", fmt.Errorf("no image generation API key configured (configure Gemini or OpenAI in Global Settings)")
	default:
		return "", "", fmt.Errorf("unsupported avatar provider %q", requested)
	}
}

func buildAvatarPrompt(name, persona, userPrompt string) string {
	const maxPersonaRunes = 4000
	const maxUserPromptRunes = 8000
	persona = truncateRunes(strings.TrimSpace(persona), maxPersonaRunes)
	userPrompt = truncateRunes(strings.TrimSpace(userPrompt), maxUserPromptRunes)

	var b strings.Builder
	b.WriteString("Create a square character portrait avatar.\n\nName:\n")
	b.WriteString(strings.TrimSpace(name))
	if persona != "" {
		b.WriteString("\n\nCharacter description:\n")
		b.WriteString(persona)
	}
	if userPrompt != "" {
		b.WriteString("\n\nAdditional art direction from the user:\n")
		b.WriteString(userPrompt)
	}
	b.WriteString("\n\nRequirements:\n- centered composition\n- readable at small icon sizes\n- clean opaque background\n- no text or logo\n- square format")
	return b.String()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func generateAvatarWithGemini(
	ctx context.Context,
	apiKey string,
	model string,
	imagePrompt string,
	client *http.Client,
	endpoint string,
	logger *slog.Logger,
) (string, error) {
	reqBody := map[string]any{
		"model": model,
		"input": []any{
			map[string]any{"type": "text", "text": imagePrompt},
		},
		"response_format": map[string]any{
			"type":         "image",
			"mime_type":    "image/png",
			"aspect_ratio": "1:1",
			"image_size":   "1K",
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, avatarGenerationTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", apiKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return "", &AvatarGenerationError{
				Code: "avatar_timeout", HTTPStatus: http.StatusGatewayTimeout,
				Err: fmt.Errorf("Gemini image generation timed out: %w", reqCtx.Err()),
			}
		}
		return "", fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := readAvatarResponse(resp.Body)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return "", &AvatarGenerationError{
				Code: "avatar_timeout", HTTPStatus: http.StatusGatewayTimeout,
				Err: fmt.Errorf("Gemini image generation timed out: %w", reqCtx.Err()),
			}
		}
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		msg := apiErr.Error.Message
		if msg == "" {
			msg = string(body)
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		if logger != nil {
			logger.Debug("gemini API error", "status", resp.StatusCode, "body", string(body))
		}
		kind := "avatar_provider_error"
		status := http.StatusBadGateway
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = "avatar_auth_failed"
			status = resp.StatusCode
		case http.StatusTooManyRequests:
			kind = "avatar_rate_limited"
			status = http.StatusTooManyRequests
		}
		return "", &AvatarGenerationError{
			Code: kind, HTTPStatus: status,
			Err: fmt.Errorf("Gemini API HTTP %d: %s", resp.StatusCode, msg),
		}
	}

	var parsed struct {
		Steps []struct {
			Type    string `json:"type"`
			Content []struct {
				Type     string `json:"type"`
				MimeType string `json:"mime_type"`
				Data     string `json:"data"`
			} `json:"content"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	var mimeType, b64 string
	for _, step := range parsed.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, content := range step.Content {
			if content.Type == "image" && content.Data != "" {
				mimeType = content.MimeType
				b64 = content.Data
			}
		}
	}
	if b64 == "" {
		return "", fmt.Errorf("no image data in response")
	}
	ext := extFromMimeType(mimeType)
	if ext == "" {
		return "", fmt.Errorf("unsupported mime type: %s", mimeType)
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	return writeTempAvatar(raw, ext)
}

func generateAvatarWithOpenAI(
	ctx context.Context,
	apiKey string,
	imagePrompt string,
	client *http.Client,
	endpoint string,
	logger *slog.Logger,
) (string, error) {
	reqBody := map[string]any{
		"model":         openAIImageModel,
		"prompt":        imagePrompt,
		"size":          "1024x1024",
		"quality":       "low",
		"output_format": "png",
		"background":    "opaque",
		"n":             1,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, avatarGenerationTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return "", &AvatarGenerationError{
				Code: "avatar_timeout", HTTPStatus: http.StatusGatewayTimeout,
				Err: fmt.Errorf("OpenAI image generation timed out: %w", err),
			}
		}
		return "", fmt.Errorf("OpenAI request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := readAvatarResponse(resp.Body)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return "", &AvatarGenerationError{
				Code: "avatar_timeout", HTTPStatus: http.StatusGatewayTimeout,
				Err: fmt.Errorf("OpenAI image generation timed out: %w", reqCtx.Err()),
			}
		}
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		msg := strings.TrimSpace(apiErr.Error.Message)
		if msg == "" {
			msg = truncateRunes(string(body), 300)
		}
		if logger != nil {
			logger.Debug("OpenAI API error", "status", resp.StatusCode, "code", apiErr.Error.Code, "body", string(body))
		}
		codeText := strings.ToLower(fmt.Sprint(apiErr.Error.Code))
		messageText := strings.ToLower(msg)
		kind := "avatar_provider_error"
		status := http.StatusBadGateway
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			kind = "avatar_auth_failed"
		case resp.StatusCode == http.StatusTooManyRequests:
			kind = "avatar_rate_limited"
			status = http.StatusTooManyRequests
		case strings.Contains(codeText, "moderation"), strings.Contains(codeText, "safety"),
			strings.Contains(codeText, "content_policy"), strings.Contains(messageText, "moderation"),
			strings.Contains(messageText, "safety"), strings.Contains(messageText, "content policy"):
			kind = "avatar_moderation_blocked"
			status = http.StatusUnprocessableEntity
		}
		return "", &AvatarGenerationError{
			Code: kind, HTTPStatus: status,
			Err: fmt.Errorf("OpenAI API HTTP %d: %s", resp.StatusCode, msg),
		}
	}

	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Data) == 0 || parsed.Data[0].B64JSON == "" {
		return "", fmt.Errorf("no image data in OpenAI response")
	}
	raw, err := base64.StdEncoding.DecodeString(parsed.Data[0].B64JSON)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	return writeTempAvatar(raw, ".png")
}

func readAvatarResponse(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxAvatarResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAvatarResponseBytes {
		return nil, fmt.Errorf("image response exceeds %d bytes", maxAvatarResponseBytes)
	}
	return body, nil
}

func writeTempAvatar(raw []byte, ext string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}
	outPath := filepath.Join(tmpDir, "avatar"+ext)
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write image: %w", err)
	}
	return outPath, nil
}

// GenerateSVGAvatarFile creates an SVG avatar file in a temp directory and returns its path.
// Used as fallback when AI avatar generation is unavailable.
func GenerateSVGAvatarFile(name string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "kojo-avatar-*")
	if err != nil {
		return "", err
	}
	svg := generateSVGAvatar(name)
	p := filepath.Join(tmpDir, "avatar.svg")
	if err := os.WriteFile(p, []byte(svg), 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", err
	}
	return p, nil
}

// generateSVGAvatar creates a fallback SVG avatar using name-derived gradient and initials.
func generateSVGAvatar(name string) string {
	hash := md5.Sum([]byte(name))

	// Generate two colors from hash for gradient
	h1 := int(hash[0]) % 360
	h2 := (h1 + 60 + int(hash[1])%120) % 360

	// Get initials (first letter of first two words, or first two letters)
	initials := "?"
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		initials = strings.ToUpper(string([]rune(parts[0])[0:1]) + string([]rune(parts[1])[0:1]))
	} else if len(name) > 0 {
		runes := []rune(name)
		if len(runes) >= 2 {
			initials = strings.ToUpper(string(runes[0:2]))
		} else {
			initials = strings.ToUpper(string(runes[0:1]))
		}
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">
  <defs>
    <linearGradient id="g" x1="0%%" y1="0%%" x2="100%%" y2="100%%">
      <stop offset="0%%" style="stop-color:hsl(%d,70%%,50%%)" />
      <stop offset="100%%" style="stop-color:hsl(%d,70%%,40%%)" />
    </linearGradient>
  </defs>
  <rect width="100" height="100" rx="20" fill="url(#g)" />
  <text x="50" y="50" text-anchor="middle" dominant-baseline="central"
    font-family="system-ui,-apple-system,sans-serif" font-size="36" font-weight="600" fill="white">%s</text>
</svg>`, h1, h2, initials)
}
