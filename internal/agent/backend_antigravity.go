package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/proctree"
)

// AntigravityBackend drives Google's Antigravity CLI (`agy`) headlessly.
//
// EXPERIMENTAL: kept on a branch that is not merged into main.
//
// Invocation (agy 1.4.x):
//
//	agy --input-format stream-json --output-format stream-json \
//	    --dangerously-skip-permissions --disable-slash-commands \
//	    [--conversation <id>] [--model <id>] --print=
//
// with one {"event":"user","message":{...}} line on stdin. Sending the
// prompt over stdin keeps it out of argv: there is no ARG_MAX limit, and a
// message that starts with "-" cannot be read as a flag. agy exits when
// stdin reaches EOF and the turn is done. `--print=` must be the last
// argument, because a bare `-p` followed by another flag takes that flag
// as the prompt.
//
// System prompt: agy has no flag to override the system prompt. It does
// re-read workspace rules (<cwd>/.agents/rules/*.md with
// `trigger: always_on`) on every turn, including resumed ones. A custom
// agent (.agents/agents/<name>/agent.md) is different: its prompt is
// frozen when the conversation is created. So kojo writes the system
// prompt into the rules directory before each turn. Each rule file is
// capped at 24,000 bytes, which is why the prompt is split into chunks.
// All rules share a 20k-token budget. Chunks over that budget are not
// dropped: agy replaces them with a file-path pointer that the model can
// read when it needs it.
//
// Session continuity: the conversation id from the init / result events
// is stored per ChatOptions.SessionKey, using the same layout as grok:
// <agentDir>/.antigravity/session_id and .antigravity/threads/key-<hash>.
// The id is replayed with `--conversation <id>`. If agy no longer has that
// conversation, it prints a warning and quietly starts a new one, so the
// id it returns always overwrites the stored one.
//
// MCP: not wired. The first version of this backend ignores
// opts.MCPServers, so extension-package MCP servers are unavailable to
// antigravity agents.
type AntigravityBackend struct {
	logger *slog.Logger
}

func NewAntigravityBackend(logger *slog.Logger) *AntigravityBackend {
	return &AntigravityBackend{logger: logger}
}

func (b *AntigravityBackend) Name() string { return ToolAntigravity }

func (b *AntigravityBackend) Available() bool {
	_, err := exec.LookPath(antigravityBinary)
	return err == nil
}

const antigravityBinary = "agy"

// AntigravityDefaultModel is the base model used when an effort level is
// set but no model is chosen. agy only accepts an effort by way of the
// model id (`<base>-<effort>`), so it needs a concrete base.
const AntigravityDefaultModel = "gemini-4-argon"

// antigravityEffortModels lists the base model ids (as shown by
// `agy models`, minus the effort suffix) and the effort levels each one
// advertises. Keep in sync with web/src/lib/toolModels.ts.
var antigravityEffortModels = map[string][]string{
	"gemini-4-argon":    {"low", "medium", "high"},
	"gemini-3.8-flash":  {"low", "medium", "high"},
	"claude-opus-5-5":   {"low", "medium", "high"},
	"claude-sonnet-5-5": {"low", "medium", "high"},
	"gpt-oss-120b":      {"medium"},
}

// antigravityValidModelEffort reports whether effort can be applied to
// model. An empty model means AntigravityDefaultModel. A model id that
// already carries an effort suffix (e.g. "gemini-4-argon-high") is
// accepted only when effort is empty.
func antigravityValidModelEffort(model, effort string) bool {
	if effort == "" {
		return true
	}
	if model == "" {
		model = AntigravityDefaultModel
	}
	for _, e := range antigravityEffortModels[model] {
		if e == effort {
			return true
		}
	}
	return false
}

// antigravityModelArg maps kojo's (model, effort) pair to the id passed to
// `agy --model`. "" means "do not pass --model". A base id without an
// effort is passed through, and agy uses that model's default effort. The
// one exception is gpt-oss-120b, which exists only as -medium.
func antigravityModelArg(model, effort string) string {
	if model == "" && effort == "" {
		return ""
	}
	if model == "" {
		model = AntigravityDefaultModel
	}
	if levels, ok := antigravityEffortModels[model]; ok {
		if effort == "" && len(levels) == 1 {
			effort = levels[0]
		}
		if effort != "" {
			return model + "-" + effort
		}
	}
	return model
}

// antigravityConversationIDPattern matches agy's conversation ids
// (lowercase UUIDs). Every id read from the agent workspace or from the
// stream is checked against it before it reaches argv or filepath.Join.
// The agent can write to its own workspace, so an unchecked value could
// inject a flag or escape the state directory during cleanup.
var antigravityConversationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isAntigravityConversationID(s string) bool {
	return antigravityConversationIDPattern.MatchString(s)
}

// antigravityHome is agy's global state directory. It is shared by every
// agy process on the host; conversations are told apart by their id.
func antigravityHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

func antigravityStateDir(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".antigravity")
}

func antigravitySessionIDFile(agentDirPath string) string {
	return filepath.Join(antigravityStateDir(agentDirPath), "session_id")
}

func antigravityThreadRefDir(agentDirPath string) string {
	return filepath.Join(antigravityStateDir(agentDirPath), "threads")
}

// antigravityKnownFile records every conversation id this agent has
// created, OneShot ones included. Reset can then remove all of them from
// agy's shared store, not only those a ref still points to.
func antigravityKnownFile(agentDirPath string) string {
	return filepath.Join(antigravityStateDir(agentDirPath), "conversations")
}

// antigravityRefuseStateSymlinks refuses a symlinked .antigravity or
// threads directory. The agent can write its own workspace, so a link
// there would otherwise redirect kojo's ref writes and cleanup to a path
// outside the agent dir. Same guard as the Gemini/OpenCode state dirs.
func antigravityRefuseStateSymlinks(agentDirPath string) error {
	return geminiRefuseSymlinks(antigravityStateDir(agentDirPath), antigravityThreadRefDir(agentDirPath))
}

// antigravityRefuseFileSymlink refuses a ref or ledger file that is a
// symlink, so a write or append cannot follow it out of the agent dir.
func antigravityRefuseFileSymlink(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func antigravitySessionRefPath(agentDirPath, sessionKey string) string {
	if sessionKey == "" {
		return antigravitySessionIDFile(agentDirPath)
	}
	return filepath.Join(antigravityThreadRefDir(agentDirPath), "key-"+agentIDToUUID(sessionKey))
}

func readAntigravitySessionIDFor(agentDirPath, sessionKey string) string {
	path := antigravitySessionRefPath(agentDirPath, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isAntigravityConversationID(id) {
		_ = os.Remove(path)
		return ""
	}
	return id
}

func writeAntigravitySessionIDFor(agentDirPath, sessionKey, id string, logger *slog.Logger) {
	if !isAntigravityConversationID(id) {
		logger.Warn("antigravity: refusing to persist non-UUID conversation id", "value", id)
		return
	}
	if err := antigravityRefuseStateSymlinks(agentDirPath); err != nil {
		logger.Warn("antigravity: refusing to write session ref", "err", err)
		return
	}
	path := antigravitySessionRefPath(agentDirPath, sessionKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("antigravity: mkdir for session ref failed", "path", path, "err", err)
		return
	}
	if err := antigravityRefuseFileSymlink(path); err != nil {
		logger.Warn("antigravity: refusing to write session ref", "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		logger.Warn("antigravity: write session ref failed", "path", path, "err", err)
	}
}

// antigravityKnownMu serialises appends to and the reset of the per-agent
// known-conversations ledger.
var antigravityKnownMu sync.Mutex

func recordAntigravityConversation(agentDirPath, id string, logger *slog.Logger) {
	if !isAntigravityConversationID(id) {
		return
	}
	if err := antigravityRefuseStateSymlinks(agentDirPath); err != nil {
		logger.Warn("antigravity: refusing to update conversation ledger", "err", err)
		return
	}
	antigravityKnownMu.Lock()
	defer antigravityKnownMu.Unlock()
	path := antigravityKnownFile(agentDirPath)
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == id {
				return
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("antigravity: mkdir for conversation ledger failed", "err", err)
		return
	}
	if err := antigravityRefuseFileSymlink(path); err != nil {
		logger.Warn("antigravity: refusing to update conversation ledger", "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warn("antigravity: open conversation ledger failed", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		logger.Warn("antigravity: append conversation ledger failed", "err", err)
	}
}

// antigravitySessionIDPersisted reports whether id is still the resume
// target of any ref for this agent. A disposable (OneShot) conversation is
// only removed when no ref points at it.
func antigravitySessionIDPersisted(agentDirPath, id string) bool {
	if id == "" {
		return false
	}
	if readAntigravitySessionIDFor(agentDirPath, "") == id {
		return true
	}
	entries, err := os.ReadDir(antigravityThreadRefDir(agentDirPath))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(antigravityThreadRefDir(agentDirPath), e.Name()))
		if rerr == nil && strings.TrimSpace(string(body)) == id {
			return true
		}
	}
	return false
}

// antigravityConversationExists reports whether agy still has the
// conversation database for id.
func antigravityConversationExists(id string) bool {
	if !isAntigravityConversationID(id) {
		return false
	}
	home := antigravityHome()
	if home == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(home, "conversations", id+".db"))
	return err == nil
}

func antigravityCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	id := readAntigravitySessionIDFor(agentDir(agentID), sessionKey)
	return id != "" && antigravityConversationExists(id)
}

func deleteAntigravityThreadRefStrict(agentID, sessionKey string) error {
	err := os.Remove(antigravitySessionRefPath(agentDir(agentID), sessionKey))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// removeAntigravityConversation deletes agy's on-disk state for one
// conversation and reports how many paths it removed. The rows agy keeps
// in conversation_summaries.db are left alone: that database is shared
// and owned by agy.
func removeAntigravityConversation(id string) (removed int, err error) {
	if !isAntigravityConversationID(id) {
		return 0, nil
	}
	home := antigravityHome()
	if home == "" {
		return 0, nil
	}
	paths := []string{
		filepath.Join(home, "conversations", id+".db"),
		filepath.Join(home, "conversations", id+".db-wal"),
		filepath.Join(home, "conversations", id+".db-shm"),
		filepath.Join(home, "conversations", id+".db-journal"),
		filepath.Join(home, "annotations", id+".pbtxt"),
		filepath.Join(home, "presence", id+".lock"),
		filepath.Join(home, "brain", id),
	}
	for _, p := range paths {
		if _, serr := os.Lstat(p); serr != nil {
			if !os.IsNotExist(serr) && err == nil {
				err = serr
			}
			continue
		}
		if rerr := os.RemoveAll(p); rerr != nil {
			if err == nil {
				err = rerr
			}
			continue
		}
		removed++
	}
	return removed, err
}

// clearAntigravitySessionCounted drops every conversation this agent
// created, along with the refs and the ledger. It is used by the CLI
// session reset, by ResetData and by TruncateMemory.
func clearAntigravitySessionCounted(agentID string) (filesRemoved, conversationsRemoved int, err error) {
	dir := agentDir(agentID)
	if err := antigravityRefuseStateSymlinks(dir); err != nil {
		return 0, 0, err
	}
	// Any ref or ledger that exists but cannot be read may be the only
	// record of a conversation; fail before removing anything.
	ids := map[string]bool{}
	readRef := func(path string) error {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if id := strings.TrimSpace(string(body)); isAntigravityConversationID(id) {
			ids[id] = true
		}
		return nil
	}
	if err := readRef(antigravitySessionIDFile(dir)); err != nil {
		return 0, 0, err
	}
	entries, rerr := os.ReadDir(antigravityThreadRefDir(dir))
	if rerr != nil && !os.IsNotExist(rerr) {
		return 0, 0, rerr
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := readRef(filepath.Join(antigravityThreadRefDir(dir), e.Name())); err != nil {
			return 0, 0, err
		}
	}
	antigravityKnownMu.Lock()
	defer antigravityKnownMu.Unlock()
	data, rerr := os.ReadFile(antigravityKnownFile(dir))
	if rerr != nil && !os.IsNotExist(rerr) {
		return 0, 0, rerr
	}
	for _, line := range strings.Split(string(data), "\n") {
		if id := strings.TrimSpace(line); isAntigravityConversationID(id) {
			ids[id] = true
		}
	}
	var failed []string
	for id := range ids {
		n, rerr := removeAntigravityConversation(id)
		filesRemoved += n
		if n > 0 {
			conversationsRemoved++
		}
		if rerr != nil {
			failed = append(failed, id)
			if err == nil {
				err = rerr
			}
		}
	}
	for _, p := range []string{antigravitySessionIDFile(dir), antigravityThreadRefDir(dir), antigravityKnownFile(dir)} {
		if rerr := os.RemoveAll(p); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	}
	// Keep the ids whose state could not be removed in the ledger so the
	// next reset retries them; the refs are still dropped so no turn
	// resumes a half-deleted conversation.
	if len(failed) > 0 {
		sort.Strings(failed)
		if merr := os.MkdirAll(antigravityStateDir(dir), 0o755); merr == nil {
			if werr := os.WriteFile(antigravityKnownFile(dir), []byte(strings.Join(failed, "\n")+"\n"), 0o644); werr != nil && err == nil {
				err = werr
			}
		}
	}
	return filesRemoved, conversationsRemoved, err
}

func clearAntigravitySession(agentID string) {
	_, _, _ = clearAntigravitySessionCounted(agentID)
}

// --- system prompt as workspace rules -------------------------------------

const (
	antigravityRulesFilePrefix  = "kojo-system-"
	antigravityRulesFrontmatter = "---\ntrigger: always_on\n---\n\n"
	// antigravityRuleChunkBytes keeps each file, frontmatter included,
	// well below agy's 24,000-byte per-file cap.
	antigravityRuleChunkBytes = 23000
)

func antigravityRulesDir(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".agents", "rules")
}

// chunkAntigravitySystemPrompt splits prompt into pieces of at most
// limit bytes. It cuts on line boundaries where it can, and otherwise on a
// UTF-8 rune boundary, so no chunk ever holds a broken character.
func chunkAntigravitySystemPrompt(prompt string, limit int) []string {
	if prompt == "" {
		return nil
	}
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(prompt, "\n") {
		if line == "" {
			continue
		}
		if cur.Len()+len(line) <= limit {
			cur.WriteString(line)
			continue
		}
		flush()
		for len(line) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			if cut == 0 {
				cut = limit
			}
			chunks = append(chunks, line[:cut])
			line = line[cut:]
		}
		cur.WriteString(line)
	}
	flush()
	return chunks
}

// renderAntigravityRules returns the rule files (by name) that carry
// prompt. Each file is the always_on frontmatter followed by one chunk.
func renderAntigravityRules(prompt string) map[string]string {
	header := func(i, n int) string {
		return fmt.Sprintf("<!-- kojo system prompt, part %d of %d. Managed by kojo and rewritten every turn; do not edit. -->\n", i, n)
	}
	// Leave room for the frontmatter and the part header.
	budget := antigravityRuleChunkBytes - len(antigravityRulesFrontmatter) - len(header(99, 99))
	chunks := chunkAntigravitySystemPrompt(prompt, budget)
	out := make(map[string]string, len(chunks))
	for i, c := range chunks {
		name := fmt.Sprintf("%s%02d.md", antigravityRulesFilePrefix, i+1)
		out[name] = antigravityRulesFrontmatter + header(i+1, len(chunks)) + c
	}
	return out
}

// writeAntigravityRules makes the kojo-managed rule files in dir match
// files exactly. Stale kojo-system-*.md files are removed; other rule
// files are left alone.
func writeAntigravityRules(dir string, files map[string]string) error {
	// dir sits in the agent's own workspace; refuse a symlinked .agents or
	// rules dir so the writes and stale-file removal below cannot be
	// redirected outside it.
	if err := geminiRefuseSymlinks(filepath.Dir(dir), dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := geminiRefuseSymlinks(filepath.Dir(dir), dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, antigravityRulesFilePrefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		if _, keep := files[name]; keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if cur, rerr := os.ReadFile(path); rerr == nil && string(cur) == files[name] {
			continue
		}
		tmp, err := os.CreateTemp(dir, ".kojo-rule-*")
		if err != nil {
			return err
		}
		if _, err := tmp.WriteString(files[name]); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	return nil
}

type antigravityRulesHold struct {
	content string
	users   int
	freed   chan struct{}
}

var (
	antigravityRulesMu    sync.Mutex
	antigravityRulesHolds = map[string]*antigravityRulesHold{}
)

// holdAntigravityRules writes the system prompt into the agent's rules
// directory and keeps it that way until release is called. Turns of the
// same agent that use the same prompt can run at the same time. A turn
// with a different prompt (for example another Slack thread) waits until
// the running ones finish, because agy reads the rules from the shared
// workspace. This mirrors holdGrokMCPConfig.
func holdAntigravityRules(ctx context.Context, agentDirPath, systemPrompt string) (release func(), err error) {
	dir := antigravityRulesDir(agentDirPath)
	files := renderAntigravityRules(systemPrompt)
	var key strings.Builder
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		key.WriteString(n)
		key.WriteByte(0)
		key.WriteString(files[n])
		key.WriteByte(0)
	}
	content := key.String()
	for {
		antigravityRulesMu.Lock()
		hold := antigravityRulesHolds[dir]
		if hold != nil && hold.content != content {
			freed := hold.freed
			antigravityRulesMu.Unlock()
			select {
			case <-freed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// Write even under a hold with the same content: the files may
		// have been edited since, and restoring them costs the running
		// turns nothing.
		if err := writeAntigravityRules(dir, files); err != nil {
			antigravityRulesMu.Unlock()
			return nil, err
		}
		if hold == nil {
			hold = &antigravityRulesHold{content: content, freed: make(chan struct{})}
			antigravityRulesHolds[dir] = hold
		}
		hold.users++
		antigravityRulesMu.Unlock()

		var once sync.Once
		return func() {
			once.Do(func() {
				antigravityRulesMu.Lock()
				defer antigravityRulesMu.Unlock()
				if hold.users--; hold.users == 0 {
					delete(antigravityRulesHolds, dir)
					close(hold.freed)
				}
			})
		}, nil
	}
}

// --- invocation -------------------------------------------------------------

func buildAntigravityArgs(resumeID string, agent *Agent) []string {
	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		// Headless: nobody can answer a permission prompt, and agy
		// auto-denies the tool otherwise.
		"--dangerously-skip-permissions",
		// A user message that starts with "/" is chat text, not a
		// command for agy.
		"--disable-slash-commands",
	}
	if resumeID != "" {
		args = append(args, "--conversation", resumeID)
	}
	if m := antigravityModelArg(agent.Model, agent.Effort); m != "" {
		args = append(args, "--model", m)
	}
	// Must come last (see the type comment).
	return append(args, "--print=")
}

func antigravityStdinPayload(userMessage string) ([]byte, error) {
	payload := map[string]any{
		"event": "user",
		"message": map[string]any{
			"role":    "user",
			"content": userMessage,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (b *AntigravityBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	agyPath, err := exec.LookPath(antigravityBinary)
	if err != nil {
		return nil, fmt.Errorf("%s not found in PATH", antigravityBinary)
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	if len(opts.MCPServers) > 0 {
		b.logger.Debug("antigravity: MCP servers are not supported yet; ignoring",
			"agent", agent.ID, "count", len(opts.MCPServers))
	}

	persistKey := opts.SessionKey
	var resumeID string
	if !opts.OneShot {
		resumeID = readAntigravitySessionIDFor(dir, persistKey)
		if resumeID != "" && !antigravityConversationExists(resumeID) {
			_ = os.Remove(antigravitySessionRefPath(dir, persistKey))
			b.logger.Info("antigravity: dropped conversation ref with no state on disk",
				"agent", agent.ID, "staleId", resumeID)
			resumeID = ""
		}
	}

	userMessage = injectSessionContexts(userMessage, opts, resumeID != "")
	if !opts.OneShot && resumeID == "" && opts.FreshSessionContext == "" && opts.RecentMessagesContext != "" {
		userMessage = injectRecentMessagesContext(userMessage, opts.RecentMessagesContext)
	}

	stdinPayload, err := antigravityStdinPayload(userMessage)
	if err != nil {
		return nil, fmt.Errorf("encode prompt: %w", err)
	}

	releaseRules, err := holdAntigravityRules(ctx, dir, systemPrompt)
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", antigravityRulesDir(dir), err)
	}

	cmd := exec.CommandContext(ctx, agyPath, buildAntigravityArgs(resumeID, agent)...)
	cmd.Env = filterEnv([]string{"AGENT_BROWSER_SESSION", "AGENT_BROWSER_COOKIE_DIR", "ANTIGRAVITY_CONVERSATION_ID"}, agent.ID, dir)
	cmd.Env = appendKojoTurnEnv(cmd.Env, opts)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdinPayload)
	// agy runs run_command children in their own session (a pty), so
	// neither a SIGTERM to agy nor one to its process group reaches them:
	// a cancelled `sleep 60` would outlive the turn. Kill the whole
	// descendant tree instead, identified at start so a reused pid is
	// never signalled.
	// The ctx watcher can fire before Identify runs below; a zero ident
	// then falls back to a plain KillTree of the just-started pid.
	// The kill runs off the Cancel callback (exec holds Wait on it) and
	// closes agyKilled when done; the cancel path waits for that, as
	// OpenCode's awaitKill does, so no descendant outlives the turn.
	var (
		agyIdentMu sync.Mutex
		agyIdent   proctree.Ident
		agyKilled  chan struct{}
	)
	cmd.Cancel = func() error {
		agyIdentMu.Lock()
		id := agyIdent
		killed := make(chan struct{})
		agyKilled = killed
		agyIdentMu.Unlock()
		pid := cmd.Process.Pid
		go func() {
			defer close(killed)
			proctree.KillTreeIdent(pid, id, 5*time.Second)
		}()
		return nil
	}
	awaitKill := func() {
		agyIdentMu.Lock()
		killed := agyKilled
		agyIdentMu.Unlock()
		if killed != nil {
			<-killed
		}
	}
	cmd.WaitDelay = 10 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		releaseRules()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		releaseRules()
		return nil, fmt.Errorf("start %s: %w", antigravityBinary, err)
	}
	ident := proctree.Identify(cmd.Process.Pid)
	agyIdentMu.Lock()
	agyIdent = ident
	agyIdentMu.Unlock()

	ch := make(chan ChatEvent, 64)
	go func() {
		defer close(ch)
		defer releaseRules()

		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }
		result := parseAntigravityStream(stdout, b.logger, send)
		// A cancel that lands while agy is quiet (e.g. inside a long tool
		// call) shows up as EOF after SIGTERM, not as a failed send.
		if !result.cancelled && ctx.Err() != nil {
			result.cancelled = true
		}

		if result.conversationID != "" {
			recordAntigravityConversation(dir, result.conversationID, b.logger)
		}
		cleanupOneShot := func() {
			if opts.OneShot && result.conversationID != "" && !antigravitySessionIDPersisted(dir, result.conversationID) {
				if _, err := removeAntigravityConversation(result.conversationID); err != nil {
					b.logger.Warn("antigravity: OneShot cleanup failed", "agent", agent.ID, "err", err)
				}
			}
		}

		if result.cancelled {
			cmd.Wait()
			awaitKill()
			cleanupOneShot()
			emitCancelDone(ctx, ch, cleanAntigravityContent(result.text), "", result.toolUses, result.usage)
			return
		}

		waitErr := cmd.Wait()
		awaitKill() // a cancel can still land between the check above and Wait
		stderr := stderrBuf.String()
		processError := classifyAntigravityProcessError(waitErr, stderr, result)
		if waitErr != nil {
			b.logger.Warn("antigravity process exited with error",
				"agent", agent.ID, "err", waitErr, "stderr", stderr, "streamError", result.streamError)
		} else if processError != "" {
			b.logger.Warn("antigravity turn reported an error",
				"agent", agent.ID, "error", processError, "stderr", stderr)
		}
		if resumeID != "" && result.conversationID != "" && result.conversationID != resumeID {
			b.logger.Info("antigravity: resume target was gone; agy started a new conversation",
				"agent", agent.ID, "staleId", resumeID, "newId", result.conversationID)
		}

		if processError != "" && result.text == "" {
			cleanupOneShot()
			send(ChatEvent{Type: "error", ErrorMessage: processError})
			return
		}

		// agy can replace a missing --conversation target with a new
		// conversation, so persist whenever the id changed and not only
		// on the first turn.
		if !opts.OneShot && result.conversationID != "" && result.conversationID != resumeID {
			writeAntigravitySessionIDFor(dir, persistKey, result.conversationID, b.logger)
		}
		cleanupOneShot()

		msg := assembleAssistantMessage(cleanAntigravityContent(result.text), "", result.toolUses, result.usage)
		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()

	return ch, nil
}

// --- stream parsing -----------------------------------------------------------

type antigravityUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

type antigravityToolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
	Output     json.RawMessage `json:"output"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type antigravityStepUpdate struct {
	ConversationID string               `json:"conversation_id"`
	StepIndex      int                  `json:"step_index"`
	State          string               `json:"state"`
	StepType       string               `json:"step_type"`
	TextDelta      string               `json:"text_delta"`
	ToolName       string               `json:"tool_name"`
	ToolInfo       *antigravityToolInfo `json:"tool_info"`
}

type antigravityResultPayload struct {
	ConversationID string            `json:"conversation_id"`
	Status         string            `json:"status"`
	Response       string            `json:"response"`
	Error          string            `json:"error"`
	Usage          *antigravityUsage `json:"usage"`
	DeniedActions  []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
}

type antigravityStreamEvent struct {
	Event          string                    `json:"event"`
	ConversationID string                    `json:"conversation_id"`
	StepUpdate     *antigravityStepUpdate    `json:"step_update"`
	Result         *antigravityResultPayload `json:"result"`
	// A fatal error before the stream starts (e.g. authentication) is
	// printed as a bare result object without the event wrapper.
	Status string `json:"status"`
	Error  string `json:"error"`
}

type antigravityStreamResult struct {
	text           string
	toolUses       []ToolUse
	usage          *Usage
	conversationID string
	gotResult      bool
	resultStatus   string
	streamError    string // result.error, or a fatal non-JSON line
	deniedActions  []string
	cancelled      bool
}

func antigravityToolID(stepIndex int) string {
	return "agy-" + strconv.Itoa(stepIndex)
}

// antigravityRawText renders a JSON value for display: a JSON string is
// unquoted, and anything else is returned as raw JSON.
func antigravityRawText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

func parseAntigravityStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool) *antigravityStreamResult {
	res := &antigravityStreamResult{}
	scanner := newSkippingLineScanner(r)
	loggedSkips := 0

	var textBuf strings.Builder
	var toolUses []ToolUse
	toolStarted := map[int]bool{}
	toolDone := map[int]bool{}
	lastTextStep := -1
	var lastFatalLine string

	finish := func() *antigravityStreamResult {
		res.text = textBuf.String()
		res.toolUses = toolUses
		if res.streamError == "" && !res.gotResult && lastFatalLine != "" {
			res.streamError = lastFatalLine
		}
		return res
	}
	setConv := func(id string) {
		if isAntigravityConversationID(id) {
			res.conversationID = id
		}
	}
	startTool := func(su *antigravityStepUpdate) bool {
		if toolStarted[su.StepIndex] {
			return true
		}
		toolStarted[su.StepIndex] = true
		name := su.ToolName
		input := ""
		if su.ToolInfo != nil {
			if name == "" {
				name = su.ToolInfo.Name
			}
			input = antigravityRawText(su.ToolInfo.Parameters)
		}
		id := antigravityToolID(su.StepIndex)
		toolUses = append(toolUses, ToolUse{ID: id, Name: name, Input: input})
		return send(ChatEvent{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: input})
	}

	for scanner.Scan() {
		if n := scanner.Skipped(); n > loggedSkips {
			logger.Warn("antigravity stream: dropped oversized JSONL line(s)",
				"dropped", n-loggedSkips, "totalDropped", n)
			loggedSkips = n
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			// agy prints fatal errors as plain lines ("error: ...",
			// "AGY_ERROR: {...}"). Keep the last one in case the
			// stream ends without a result event.
			s := string(line)
			if strings.HasPrefix(s, "error:") || strings.HasPrefix(s, "AGY_ERROR:") {
				lastFatalLine = s
			}
			continue
		}
		var ev antigravityStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			logger.Debug("antigravity parse line", "err", err, "line", string(line))
			continue
		}
		switch ev.Event {
		case "init":
			setConv(ev.ConversationID)
		case "step_update":
			su := ev.StepUpdate
			if su == nil {
				continue
			}
			setConv(su.ConversationID)
			switch su.StepType {
			case "agent_response":
				if su.TextDelta == "" {
					continue
				}
				delta := su.TextDelta
				if lastTextStep != su.StepIndex && textBuf.Len() > 0 {
					// A new response step after a tool call (or after
					// another response) is a new paragraph.
					cur := textBuf.String()
					switch {
					case strings.HasSuffix(cur, "\n\n"):
					case strings.HasSuffix(cur, "\n"):
						delta = "\n" + delta
					default:
						delta = "\n\n" + delta
					}
				}
				lastTextStep = su.StepIndex
				textBuf.WriteString(delta)
				if !send(ChatEvent{Type: "text", Delta: delta}) {
					res.cancelled = true
					return finish()
				}
			case "tool":
				if !startTool(su) {
					res.cancelled = true
					return finish()
				}
				if su.State != "DONE" && su.State != "ERROR" {
					continue
				}
				if toolDone[su.StepIndex] {
					continue
				}
				toolDone[su.StepIndex] = true
				output := ""
				if su.ToolInfo != nil {
					output = antigravityRawText(su.ToolInfo.Output)
					if su.ToolInfo.Error != nil && su.ToolInfo.Error.Message != "" {
						if output != "" {
							output += "\n"
						}
						output += "Error: " + su.ToolInfo.Error.Message
					}
				}
				if su.State == "ERROR" && output == "" {
					output = "Error"
				}
				id := antigravityToolID(su.StepIndex)
				name := su.ToolName
				if name == "" && su.ToolInfo != nil {
					name = su.ToolInfo.Name
				}
				matchToolOutput(toolUses, id, name, output)
				if !send(ChatEvent{Type: "tool_result", ToolUseID: id, ToolName: name, ToolOutput: output}) {
					res.cancelled = true
					return finish()
				}
			}
		case "result":
			if ev.Result != nil {
				applyAntigravityResult(res, ev.Result)
			}
		case "":
			if ev.Status != "" {
				applyAntigravityResult(res, &antigravityResultPayload{
					ConversationID: ev.ConversationID, Status: ev.Status, Error: ev.Error,
				})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("antigravity stream read error", "err", err)
	}
	return finish()
}

func applyAntigravityResult(res *antigravityStreamResult, r *antigravityResultPayload) {
	res.gotResult = true
	res.resultStatus = r.Status
	if isAntigravityConversationID(r.ConversationID) {
		res.conversationID = r.ConversationID
	}
	if r.Usage != nil {
		res.usage = &Usage{
			InputTokens:          r.Usage.InputTokens,
			OutputTokens:         r.Usage.OutputTokens + r.Usage.ThinkingTokens,
			CacheReadInputTokens: r.Usage.CacheReadTokens,
		}
	}
	if r.Status != "" && !strings.EqualFold(r.Status, "SUCCESS") {
		res.streamError = r.Error
		if res.streamError == "" {
			res.streamError = "antigravity turn failed (status " + r.Status + ")"
		}
	}
	for _, d := range r.DeniedActions {
		name := d.DisplayName
		if name == "" {
			name = d.Action
		}
		res.deniedActions = append(res.deniedActions, name)
	}
}

// classifyAntigravityProcessError returns the most useful error text for
// a finished agy process, or "" when the turn succeeded.
func classifyAntigravityProcessError(waitErr error, stderr string, res *antigravityStreamResult) string {
	if res.streamError != "" {
		return res.streamError
	}
	if waitErr != nil {
		if s := antigravityStderrSummary(stderr); s != "" {
			return waitErr.Error() + ": " + s
		}
		return waitErr.Error()
	}
	if !res.gotResult && res.text == "" {
		msg := "antigravity exited without a result"
		if s := antigravityStderrSummary(stderr); s != "" {
			msg += ": " + s
		}
		return msg
	}
	if len(res.deniedActions) > 0 && res.text == "" {
		return "antigravity denied tool permission: " + strings.Join(res.deniedActions, ", ")
	}
	return ""
}

// antigravityStderrSummary drops agy's routine warnings, such as the
// "conversation not found" notice on a stale resume, which agy recovers
// from by itself.
func antigravityStderrSummary(stderr string) string {
	var keep []string
	sc := bufio.NewScanner(strings.NewReader(stderr))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "warning:") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// antigravityFileLinkPattern matches the markdown links agy adds around
// file names in its replies ("[main.go](file:///abs/main.go)"). Outside an
// IDE they are noise; the link label is kept.
var antigravityFileLinkPattern = regexp.MustCompile(`\[([^\]\n]+)\]\(file://[^)\s]*\)`)

func cleanAntigravityContent(s string) string {
	return antigravityFileLinkPattern.ReplaceAllString(s, "$1")
}
