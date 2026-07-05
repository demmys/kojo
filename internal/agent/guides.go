package agent

import (
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
	"github.com/loppo-llc/kojo/internal/configdir"
)

// kojo guides — verbose how-to docs externalized from the system prompt.
//
// The full API / etiquette docs (group DM curl commands, todo API,
// credential retrieval, memory hygiene rules, attachment staging
// contract) used to be inlined into every agent's system prompt,
// costing ~2100 tokens per prompt. They now live as shared markdown
// files under <configdir>/guide/ that the agent Reads on demand; the
// system prompt carries only a one-line pointer per capability (see
// the "## kojo Guides" section in buildSystemPrompt).
//
// The files are generic — shared by all agents — and use
// {AGENT_ID} / {API_BASE} / {CURL_FLAGS} / {DATA_DIR} placeholders
// whose concrete values are already present in each agent's system
// prompt. This keeps the on-disk files byte-identical across agents
// (one copy, no per-agent churn) and keeps the prompt cache-stable.

//go:embed guides/*.md
var guidesFS embed.FS

// GuideDir returns the shared guide directory under the kojo config
// root. Not per-agent: every agent reads the same files.
func GuideDir() string {
	return filepath.Join(configdir.Path(), "guide")
}

// Guides are re-synced lazily from prepareChat, throttled to once per
// guidesSyncInterval per process. A time window (not sync.Once) so a
// failed first sync retries, and so local tampering with the shared
// files is healed within the window instead of persisting until the
// next daemon restart.
var (
	guidesSyncMu   sync.Mutex
	guidesSyncedAt time.Time
)

const guidesSyncInterval = 10 * time.Minute

// syncGuidesThrottled runs SyncGuides at most once per
// guidesSyncInterval. Called from prepareChat on every turn.
//
// The sync runs UNDER guidesSyncMu and the timestamp advances only
// after a fully successful sync: a concurrent caller can never observe
// the guides as "fresh" while files are still being written, and a
// failed sync leaves the timestamp untouched so the next call retries
// immediately instead of waiting out the window. Holding the mutex
// across the sync is fine — five small file compares/writes, run at
// most once per window on the happy path.
func syncGuidesThrottled(logger *slog.Logger) {
	guidesSyncMu.Lock()
	defer guidesSyncMu.Unlock()
	if time.Since(guidesSyncedAt) < guidesSyncInterval {
		return
	}
	if SyncGuides(logger) {
		guidesSyncedAt = time.Now()
	}
}

// SyncGuides writes the embedded guide files to GuideDir(), overwriting
// a file only when its content differs from the embedded copy (so an
// upgraded binary refreshes stale guides while steady-state boots are
// write-free). Failures are logged and non-fatal: the agent can still
// run, it just gets a dangling pointer until the next sync succeeds.
// Returns true only when every guide file was verified/written — the
// throttle uses this to decide whether to retry on the next call.
func SyncGuides(logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	dir := GuideDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("guides: create guide dir failed", "dir", dir, "err", err)
		return false
	}
	entries, err := fs.ReadDir(guidesFS, "guides")
	if err != nil {
		logger.Warn("guides: read embedded guides failed", "err", err)
		return false
	}
	ok := true
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := fs.ReadFile(guidesFS, "guides/"+e.Name())
		if err != nil {
			logger.Warn("guides: read embedded guide failed", "name", e.Name(), "err", err)
			ok = false
			continue
		}
		dst := filepath.Join(dir, e.Name())
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(body) {
			continue // already current
		}
		if err := atomicfile.WriteBytes(dst, body, 0o644); err != nil {
			logger.Warn("guides: write guide failed", "path", dst, "err", err)
			ok = false
			continue
		}
		logger.Debug("guides: synced", "path", dst)
	}
	return ok
}
