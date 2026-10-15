package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// GeminiDefaultModel is the model kojo passes when the agent has none set.
// It is always passed explicitly (never left to the CLI default) so the
// thinkingLevel override in settings.json has a concrete model to match.
const GeminiDefaultModel = "gemini-4-argon"

// geminiEffortModels maps the Gemini models whose thinking level kojo can
// drive through agent.Effort (low/medium/high → generationConfig
// .thinkingConfig.thinkingLevel) to the concrete id the settings.json
// modelConfigs override must match. gemini-cli resolves aliases before
// matching overrides, so an override keyed on an alias never applies
// (verified against gemini-cli 0.64.0: `-m flash` with an override on
// "flash" is ignored, on "gemini-3.8-flash" it applies). Only "flash" is
// mapped: "pro" and "auto" resolve to models outside this list (auto even
// routes per request), so effort stays rejected for them.
var geminiEffortModels = map[string]string{
	"gemini-4-argon":   "gemini-4-argon",
	"gemini-3.8-flash": "gemini-3.8-flash",
	"flash":            "gemini-3.8-flash",
}

// geminiSessionIDPattern matches the Gemini CLI's session UUIDs. Every
// value read back from a ref file or the stream is validated before it
// reaches argv (`--resume`) or a filesystem glob: the agent can write to
// its own directory, so an unvalidated ref could inject flags or paths.
var geminiSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isGeminiSessionID(s string) bool { return geminiSessionIDPattern.MatchString(s) }

// geminiStripEnvPrefixes are removed from the inherited environment before
// the per-turn values are appended. GEMINI_* covers a parent GEMINI_API_KEY /
// GEMINI_CLI_HOME / GEMINI_SYSTEM_MD / GEMINI_MODEL; the GOOGLE_* entries
// would otherwise switch the CLI to another auth method (Vertex, ADC) or a
// different key than the one kojo resolved.
var geminiStripEnvPrefixes = []string{
	"GEMINI_",
	"GOOGLE_API_KEY=",
	"GOOGLE_GEMINI_", // e.g. GOOGLE_GEMINI_BASE_URL would redirect the injected key
	"GOOGLE_GENAI_USE_VERTEXAI=",
	"GOOGLE_CLOUD_PROJECT=",
	"GOOGLE_CLOUD_LOCATION=",
	"GOOGLE_APPLICATION_CREDENTIALS=",
	"AGENT_BROWSER_SESSION",
	"AGENT_BROWSER_COOKIE_DIR",
}

// GeminiBackend implements ChatBackend for the Gemini CLI
// (@google/gemini-cli). It runs `gemini -o stream-json` headlessly with
// the prompt on stdin and parses the NDJSON event stream.
//
// Isolation: every agent gets its own GEMINI_CLI_HOME
// (<agentDir>/.gemini-cli-home). Without it the CLI would pick up the
// operator's ~/.gemini OAuth login (rejected for consumer accounts) and
// share session/memory state across agents. kojo regenerates
// $GEMINI_CLI_HOME/.gemini/settings.json on every turn (auth type, MCP
// servers, thinking level), and passes the system prompt through
// GEMINI_SYSTEM_MD, which fully replaces the CLI's built-in prompt.
//
// Sessions: the CLI reports its session UUID in the `init` event; kojo
// stores it per ChatOptions.SessionKey (same layout as the grok backend)
// and replays it with `--resume <uuid>` on the next turn.
type GeminiBackend struct {
	logger *slog.Logger
	creds  *CredentialStore
}

func NewGeminiBackend(logger *slog.Logger, creds *CredentialStore) *GeminiBackend {
	return &GeminiBackend{logger: logger, creds: creds}
}

func (b *GeminiBackend) Name() string { return ToolGemini }

func (b *GeminiBackend) Available() bool {
	_, err := exec.LookPath("gemini")
	return err == nil
}

// geminiCLIHome is the per-agent GEMINI_CLI_HOME. Living inside the agent
// dir means ResetData's RemoveAll wipes it with everything else.
func geminiCLIHome(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".gemini-cli-home")
}

// geminiKojoDir holds kojo-owned state inside the CLI home (session refs,
// per-turn system prompt files); the CLI itself only touches .gemini/.
func geminiKojoDir(agentDirPath string) string {
	return filepath.Join(geminiCLIHome(agentDirPath), "kojo")
}

func geminiSettingsPath(agentDirPath string) string {
	return filepath.Join(geminiCLIHome(agentDirPath), ".gemini", "settings.json")
}

func geminiSessionIDFile(agentDirPath string) string {
	return filepath.Join(geminiKojoDir(agentDirPath), "session_id")
}

func geminiThreadRefDir(agentDirPath string) string {
	return filepath.Join(geminiKojoDir(agentDirPath), "threads")
}

// geminiSessionRefPath maps a SessionKey to its ref file. The key is
// hashed so a caller-supplied value can't steer the path.
func geminiSessionRefPath(agentDirPath, sessionKey string) string {
	if sessionKey == "" {
		return geminiSessionIDFile(agentDirPath)
	}
	return filepath.Join(geminiThreadRefDir(agentDirPath), "key-"+agentIDToUUID(sessionKey))
}

func readGeminiSessionIDFor(agentDirPath, sessionKey string) string {
	path := geminiSessionRefPath(agentDirPath, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !isGeminiSessionID(id) {
		_ = os.Remove(path)
		return ""
	}
	return id
}

func writeGeminiSessionIDFor(agentDirPath, sessionKey, sessionID string, logger *slog.Logger) {
	if !isGeminiSessionID(sessionID) {
		logger.Warn("gemini: refusing to persist malformed session id", "sessionId", sessionID)
		return
	}
	path := geminiSessionRefPath(agentDirPath, sessionKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Warn("gemini: mkdir for session ref failed", "path", path, "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0o644); err != nil {
		logger.Warn("gemini: write session ref failed", "path", path, "err", err)
	}
}

// geminiSessionIDPersisted reports whether any stored key resumes
// sessionID, so a OneShot cleanup never deletes a session in use.
func geminiSessionIDPersisted(agentDirPath, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if readGeminiSessionIDFor(agentDirPath, "") == sessionID {
		return true
	}
	entries, err := os.ReadDir(geminiThreadRefDir(agentDirPath))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(geminiThreadRefDir(agentDirPath), e.Name()))
		if rerr == nil && strings.TrimSpace(string(body)) == sessionID {
			return true
		}
	}
	return false
}

// geminiSessionFiles returns the CLI's on-disk files for sessionID:
// $GEMINI_CLI_HOME/.gemini/tmp/<project>/chats/session-<ts>-<id[:8]>.jsonl.
// The filename only carries the first 8 hex digits, so each candidate's
// header line is checked for the full sessionId.
func geminiSessionFiles(agentDirPath, sessionID string) []string {
	if !isGeminiSessionID(sessionID) {
		return nil
	}
	pattern := filepath.Join(geminiCLIHome(agentDirPath), ".gemini", "tmp", "*", "chats", "session-*-"+sessionID[:8]+".jsonl")
	matches, _ := filepath.Glob(pattern)
	var out []string
	for _, p := range matches {
		if geminiSessionFileID(p) == sessionID {
			out = append(out, p)
		}
	}
	return out
}

// geminiSessionFileID reads the sessionId from a session file's first line.
func geminiSessionFileID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	line := buf[:n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var hdr struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &hdr) != nil {
		return ""
	}
	return hdr.SessionID
}

func geminiSessionExists(agentDirPath, sessionID string) bool {
	return len(geminiSessionFiles(agentDirPath, sessionID)) > 0
}

// geminiCanResumeSession mirrors grokCanResumeSession for Manager.CanResumeSession.
func geminiCanResumeSession(agentID, sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	dir := agentDir(agentID)
	id := readGeminiSessionIDFor(dir, sessionKey)
	return id != "" && geminiSessionExists(dir, id)
}

func removeGeminiSession(agentDirPath, sessionID string) {
	for _, p := range geminiSessionFiles(agentDirPath, sessionID) {
		_ = os.Remove(p)
	}
}

// deleteGeminiThreadRefStrict makes the next keyed turn start fresh.
func deleteGeminiThreadRefStrict(agentID, sessionKey string) error {
	err := os.Remove(geminiSessionRefPath(agentDir(agentID), sessionKey))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// clearGeminiSessionCounted removes every CLI session file and every kojo
// session ref for the agent. Settings are left alone (rewritten per turn).
func clearGeminiSessionCounted(agentID string) (filesRemoved int, err error) {
	dir := agentDir(agentID)
	tmpRoot := filepath.Join(geminiCLIHome(dir), ".gemini", "tmp")
	_ = filepath.Walk(tmpRoot, func(_ string, info os.FileInfo, werr error) error {
		if werr == nil && info != nil && !info.IsDir() {
			filesRemoved++
		}
		return nil
	})
	if rerr := os.RemoveAll(tmpRoot); rerr != nil && !os.IsNotExist(rerr) {
		err = rerr
		filesRemoved = 0
	}
	for _, p := range []string{geminiSessionIDFile(dir), geminiThreadRefDir(dir)} {
		if rerr := os.RemoveAll(p); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	}
	return filesRemoved, err
}

// geminiModel returns the model id kojo passes with -m.
func geminiModel(agent *Agent) string {
	if agent.Model != "" {
		return agent.Model
	}
	return GeminiDefaultModel
}

// geminiThinkingLevel maps kojo's effort onto the API's thinkingLevel
// enum, or "" when the CLI default should apply.
func geminiThinkingLevel(model, effort string) string {
	if geminiEffortModels[model] == "" {
		return ""
	}
	switch effort {
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	case "high":
		return "HIGH"
	}
	return ""
}

// buildGeminiSettings assembles the settings.json kojo writes into the
// agent's GEMINI_CLI_HOME before every turn.
//
//   - security.auth.selectedType pins API-key auth (no OAuth fallback).
//   - Auto-update / usage statistics are off: a headless child must not
//     self-update or phone home on kojo's behalf.
//   - save_memory is excluded: kojo owns agent memory (MEMORY.md and the
//     memory DB); the CLI's own GEMINI.md memory would bypass kojo's
//     truncate/reset and re-inject stale facts.
//   - mcpServers: HTTP entries use `httpUrl` (streamable HTTP; plain `url`
//     means SSE to the CLI), stdio entries pass command/args/env through.
//     trust=true skips per-tool confirmation, matching --yolo.
//   - modelConfigs.customOverrides carries the thinking level, matched on
//     the concrete model id (verified against gemini-cli 0.64.0: a bogus
//     value there surfaces as a 400 on thinking_config.thinking_level).
func buildGeminiSettings(model, effort string, mcpServers map[string]mcpServerEntry) map[string]any {
	settings := map[string]any{
		"security": map[string]any{
			"auth": map[string]any{"selectedType": "gemini-api-key"},
		},
		"general": map[string]any{
			"enableAutoUpdate":             false,
			"enableAutoUpdateNotification": false,
		},
		"privacy": map[string]any{"usageStatisticsEnabled": false},
		"tools":   map[string]any{"exclude": []string{"save_memory"}},
	}
	if len(mcpServers) > 0 {
		servers := make(map[string]any, len(mcpServers))
		for name, srv := range mcpServers {
			entry := map[string]any{"trust": true}
			if srv.isStdio() {
				entry["command"] = srv.Command
				if len(srv.Args) > 0 {
					entry["args"] = srv.Args
				}
				if len(srv.Env) > 0 {
					entry["env"] = srv.Env
				}
			} else {
				entry["httpUrl"] = srv.URL
				if len(srv.Headers) > 0 {
					entry["headers"] = srv.Headers
				}
			}
			servers[name] = entry
		}
		settings["mcpServers"] = servers
	}
	if level := geminiThinkingLevel(model, effort); level != "" {
		settings["modelConfigs"] = map[string]any{
			"customOverrides": []any{
				map[string]any{
					"match": map[string]any{"model": geminiEffortModels[model]},
					"modelConfig": map[string]any{
						"generateContentConfig": map[string]any{
							// The override replaces the CLI's default
							// thinkingConfig, which carries includeThoughts;
							// without it the session file's "thoughts" come
							// back empty (verified live on gemini-4-argon)
							// and kojo has no thinking to show. The stream
							// content stays thought-free either way.
							"thinkingConfig": map[string]any{"thinkingLevel": level, "includeThoughts": true},
						},
					},
				},
			},
		}
	}
	return settings
}

func writeGeminiSettings(agentDirPath, model, effort string, mcpServers map[string]mcpServerEntry) error {
	body, err := json.MarshalIndent(buildGeminiSettings(model, effort, mcpServers), "", "  ")
	if err != nil {
		return err
	}
	path := geminiSettingsPath(agentDirPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Atomic so a concurrent keyed turn starting up never reads a torn file.
	return atomicfile.WriteBytes(path, body, 0o600)
}

// buildGeminiArgs builds the argv. The prompt itself goes on stdin
// (`-p ""` makes the CLI headless and appends stdin), so long prompts
// never hit ARG_MAX and a prompt starting with "-" is never a flag.
func buildGeminiArgs(model, resumeID string, includeDirs []string) []string {
	args := []string{
		// The agent dir is the agent's own workspace; without this the
		// headless CLI refuses to start in an untrusted folder.
		"--skip-trust",
		// Headless: auto-approve every tool call.
		"--yolo",
		"-o", "stream-json",
		"-m", model,
	}
	for _, d := range includeDirs {
		args = append(args, "--include-directories", d)
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	return append(args, "-p", "")
}

// geminiCommandEnv builds the child env: inherited env minus every
// Gemini/Google auth knob, plus the per-agent home, key and system prompt.
func geminiCommandEnv(agentID, dir, apiKey, systemMDPath string) []string {
	env := filterEnv(geminiStripEnvPrefixes, agentID, dir)
	env = append(env,
		"GEMINI_CLI_HOME="+geminiCLIHome(dir),
		"GEMINI_API_KEY="+apiKey,
	)
	if systemMDPath != "" {
		env = append(env, "GEMINI_SYSTEM_MD="+systemMDPath)
	}
	return env
}

func (b *GeminiBackend) Chat(ctx context.Context, agent *Agent, userMessage string, systemPrompt string, opts ChatOptions) (<-chan ChatEvent, error) {
	geminiPath, err := exec.LookPath("gemini")
	if err != nil {
		return nil, fmt.Errorf("gemini not found in PATH")
	}
	apiKey, err := loadGeminiAPIKey(b.creds)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("gemini: no Gemini API key configured (set it in Settings > API keys, or GEMINI_API_KEY)")
	}

	dir := agentDir(agent.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	// Resume strategy matches the grok backend: !OneShot resumes the
	// ref stored for opts.SessionKey ("" = the agent's own session);
	// OneShot always starts fresh and never touches a stored ref.
	persistKey := opts.SessionKey
	var resumeID string
	if !opts.OneShot {
		resumeID = readGeminiSessionIDFor(dir, persistKey)
		if resumeID != "" && !geminiSessionExists(dir, resumeID) {
			_ = os.Remove(geminiSessionRefPath(dir, persistKey))
			b.logger.Info("gemini: dropped session ref with no session on disk",
				"agent", agent.ID, "staleId", resumeID)
			resumeID = ""
		}
	}

	userMessage = injectSessionHistoryContext(userMessage, opts.FreshSessionContext, opts.ResumeSessionContext, resumeID != "")
	if !opts.OneShot && resumeID == "" && opts.FreshSessionContext == "" && opts.RecentMessagesContext != "" {
		userMessage = injectRecentMessagesContext(userMessage, opts.RecentMessagesContext)
	}

	model := geminiModel(agent)
	if err := writeGeminiSettings(dir, model, agent.Effort, opts.MCPServers); err != nil {
		return nil, fmt.Errorf("write %s: %w", geminiSettingsPath(dir), err)
	}

	// The system prompt varies per turn (keyed surfaces append their own
	// context), so each turn gets its own file; concurrent turns for the
	// same agent never overwrite each other's prompt.
	var systemMDPath string
	if systemPrompt != "" {
		if err := os.MkdirAll(geminiKojoDir(dir), 0o700); err != nil {
			return nil, fmt.Errorf("create gemini kojo dir: %w", err)
		}
		var rnd [8]byte
		_, _ = rand.Read(rnd[:])
		systemMDPath = filepath.Join(geminiKojoDir(dir), "system-"+hex.EncodeToString(rnd[:])+".md")
		if err := os.WriteFile(systemMDPath, []byte(geminiSystemMD(systemPrompt)), 0o600); err != nil {
			return nil, fmt.Errorf("write system prompt: %w", err)
		}
	}
	removeSystemMD := func() {
		if systemMDPath != "" {
			_ = os.Remove(systemMDPath)
		}
	}

	cmd := exec.CommandContext(ctx, geminiPath, buildGeminiArgs(model, resumeID, geminiIncludeDirs())...)
	cmd.Env = appendKojoTurnEnv(geminiCommandEnv(agent.ID, dir, apiKey, systemMDPath), opts)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(userMessage)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderrBuf, limit: 4096}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		removeSystemMD()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	thoughts := newGeminiThoughtTail(dir, resumeID, time.Now())
	if err := cmd.Start(); err != nil {
		removeSystemMD()
		return nil, fmt.Errorf("start gemini: %w", err)
	}

	ch := make(chan ChatEvent, 64)
	go func() {
		defer close(ch)
		defer removeSystemMD()

		send := func(e ChatEvent) bool { return ctxSend(ctx, ch, e) }

		// Thought summaries come from the session file, not stdout; tail
		// it alongside the stream. stopTail must run before any terminal
		// event so no "thinking" event can trail it.
		tailStop, tailDone := make(chan struct{}), make(chan struct{})
		go thoughts.run(tailStop, tailDone, send)
		stopTail := func() {
			select {
			case <-tailStop:
			default:
				close(tailStop)
			}
			<-tailDone
		}
		defer stopTail()

		result := parseGeminiStream(stdout, b.logger, send, thoughts.setSession)

		cleanupOneShot := func() {
			if opts.OneShot && result.sessionID != "" && !geminiSessionIDPersisted(dir, result.sessionID) {
				removeGeminiSession(dir, result.sessionID)
			}
		}

		if result.cancelled {
			stopTail()
			cmd.Wait()
			// Collect (without sending: the consumer is gone) whatever the
			// last step recorded, so the cancelled message keeps it.
			thoughts.poll(func(ChatEvent) bool { return false })
			emitCancelDone(ctx, ch, result.text, thoughts.thinking(), result.toolUses, result.usage)
			cleanupOneShot()
			return
		}

		waitErr := cmd.Wait()
		// Final drain: the last step's record is written when the step
		// completes, typically right before the process exits.
		stopTail()
		thoughts.poll(send)
		processError := classifyGeminiProcessError(waitErr, stderrBuf.String(), result)
		if processError != "" {
			b.logger.Warn("gemini turn failed",
				"agent", agent.ID, "err", waitErr, "stderr", stderrBuf.String(), "streamError", result.streamError)
		}

		if resumeID != "" && result.text == "" && isGeminiStaleSessionError(processError) {
			_ = os.Remove(geminiSessionRefPath(dir, persistKey))
			b.logger.Info("gemini: dropped stale session ref after resume failure",
				"agent", agent.ID, "staleId", resumeID)
		}

		if processError != "" && result.text == "" {
			cleanupOneShot()
			send(ChatEvent{Type: "error", ErrorMessage: processError})
			return
		}

		if !opts.OneShot && resumeID == "" && result.sessionID != "" {
			writeGeminiSessionIDFor(dir, persistKey, result.sessionID, b.logger)
		}
		cleanupOneShot()

		msg := assembleAssistantMessage(result.text, thoughts.thinking(), result.toolUses, result.usage)
		send(ChatEvent{Type: "done", Message: msg, Usage: result.usage, ErrorMessage: processError})
	}()

	return ch, nil
}

// geminiStreamResult collects one gemini invocation's streamed output.
type geminiStreamResult struct {
	text         string
	toolUses     []ToolUse
	usage        *Usage
	sessionID    string
	resultStatus string // "success" / "error" / "" (no result event)
	streamError  string // result.error.message or last severity=error event
	cancelled    bool
}

type geminiStreamEvent struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	Delta      bool            `json:"delta"`
	ToolName   string          `json:"tool_name"`
	ToolID     string          `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
	Output     json.RawMessage `json:"output"`
	Severity   string          `json:"severity"`
	Message    string          `json:"message"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Stats *struct {
		InputTokens  int  `json:"input_tokens"`
		OutputTokens int  `json:"output_tokens"`
		Cached       int  `json:"cached"`
		Input        *int `json:"input"`
	} `json:"stats"`
}

// parseGeminiStream reads `gemini -o stream-json` NDJSON from r. Event types:
//
//	init         {session_id, model}
//	message      {role:"user"|"assistant", content, delta}
//	tool_use     {tool_name, tool_id, parameters}
//	tool_result  {tool_id, status, output, error?}
//	error        {severity:"warning"|"error", message}
//	result       {status, error?, stats}
//
// Non-JSON lines (startup warnings, stack traces) are ignored; unknown
// event types are skipped.
//
// onSession, when non-nil, is called with the init event's session id.
func parseGeminiStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool, onSession func(string)) *geminiStreamResult {
	res := &geminiStreamResult{}
	scanner := newSkippingLineScanner(r)
	loggedSkips := 0
	var textBuf strings.Builder
	finish := func(cancelled bool) *geminiStreamResult {
		res.text = textBuf.String()
		res.cancelled = cancelled
		return res
	}

	for scanner.Scan() {
		if n := scanner.Skipped(); n > loggedSkips {
			logger.Warn("gemini stream: dropped oversized JSONL line(s)", "dropped", n-loggedSkips)
			loggedSkips = n
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev geminiStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			logger.Debug("gemini parse line", "err", err)
			continue
		}
		switch ev.Type {
		case "init":
			if isGeminiSessionID(ev.SessionID) {
				res.sessionID = ev.SessionID
				if onSession != nil {
					onSession(ev.SessionID)
				}
			}
		case "message":
			if ev.Role != "assistant" || ev.Content == "" {
				continue
			}
			textBuf.WriteString(ev.Content)
			if !send(ChatEvent{Type: "text", Delta: ev.Content}) {
				return finish(true)
			}
		case "tool_use":
			input := ""
			if len(ev.Parameters) > 0 && string(ev.Parameters) != "null" {
				input = string(ev.Parameters)
			}
			res.toolUses = append(res.toolUses, ToolUse{ID: ev.ToolID, Name: ev.ToolName, Input: input})
			if !send(ChatEvent{Type: "tool_use", ToolUseID: ev.ToolID, ToolName: ev.ToolName, ToolInput: input}) {
				return finish(true)
			}
		case "tool_result":
			output := geminiJSONText(ev.Output)
			if ev.Status == "error" && ev.Error != nil && ev.Error.Message != "" {
				if output != "" {
					output += "\n"
				}
				output += "Error: " + ev.Error.Message
			}
			name := ""
			for i := len(res.toolUses) - 1; i >= 0; i-- {
				if res.toolUses[i].ID == ev.ToolID {
					name = res.toolUses[i].Name
					break
				}
			}
			matchToolOutput(res.toolUses, ev.ToolID, name, output)
			if !send(ChatEvent{Type: "tool_result", ToolUseID: ev.ToolID, ToolName: name, ToolOutput: output}) {
				return finish(true)
			}
		case "error":
			if ev.Severity == "error" && ev.Message != "" {
				res.streamError = ev.Message
			} else if ev.Message != "" {
				logger.Debug("gemini warning", "message", ev.Message)
			}
		case "result":
			res.resultStatus = ev.Status
			if ev.Error != nil && ev.Error.Message != "" {
				res.streamError = ev.Error.Message
			}
			if s := ev.Stats; s != nil && (s.InputTokens > 0 || s.OutputTokens > 0) {
				input := s.InputTokens - s.Cached
				if s.Input != nil {
					input = *s.Input
				}
				if input < 0 {
					input = 0
				}
				res.usage = &Usage{
					InputTokens:          input,
					OutputTokens:         s.OutputTokens,
					CacheReadInputTokens: s.Cached,
				}
			}
		}
	}
	return finish(false)
}

// geminiJSONText renders a tool_result output: strings verbatim,
// anything else as compact JSON.
func geminiJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// classifyGeminiProcessError returns the turn's error string, or "".
// Preference: the stream's own error (result.error / severity=error
// event), then a result with status=error, then a failed exit (with
// stderr), then a run that never produced a result event at all — the
// CLI prints fatal startup errors (bad auth, bad --resume) as plain text
// and may still exit 0.
func classifyGeminiProcessError(waitErr error, stderr string, res *geminiStreamResult) string {
	if res.streamError != "" && res.resultStatus != "success" {
		return res.streamError
	}
	if res.resultStatus == "error" {
		return "gemini: turn failed"
	}
	trimmed := strings.TrimSpace(stderr)
	if waitErr != nil {
		if trimmed != "" {
			return waitErr.Error() + ": " + trimmed
		}
		return waitErr.Error()
	}
	if res.resultStatus == "" {
		if trimmed != "" {
			return "gemini exited without a result: " + trimmed
		}
		return "gemini exited without a result"
	}
	return ""
}

// isGeminiStaleSessionError matches the CLI's failed-resume message
// ("Error resuming session: Invalid session identifier ...", 0.64.0).
func isGeminiStaleSessionError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "error resuming session") ||
		strings.Contains(lower, "invalid session identifier")
}

// geminiSystemMD builds the GEMINI_SYSTEM_MD body. GEMINI_SYSTEM_MD replaces
// gemini-cli's built-in prompt entirely, so append the CLI's own tool and
// skill sections (substituted by the CLI) so the model still learns the
// real Gemini tool names alongside kojo's backend-neutral prompt.
func geminiSystemMD(systemPrompt string) string {
	return systemPrompt + "\n\n# Gemini CLI tools\n\nThe following tools are available to you: ${AvailableTools}\n\nThe shell tool rejects commands that use command or process substitution ($(...), backticks, <(...), >(...)), including inside double quotes. When you need those, write the commands to a script file in your working directory (e.g. `task.sh`) and run it with `bash task.sh`.\n\n${AgentSkills}\n\n${SubAgents}\n"
}

// geminiGuideDir returns GuideDir as an absolute path: the CLI resolves
// relative include dirs against its cwd (the agent dir), which would
// break when kojo runs with a relative --config-dir.
// geminiIncludeDirs returns the extra workspace directories to pass via
// --include-directories. The shared guide dir lives outside the agent
// workspace and the CLI's file tools refuse paths outside included
// directories, but gemini-cli aborts at startup ("Directory does not
// exist") on a missing one, so it is only included when present.
func geminiIncludeDirs() []string {
	if fi, err := os.Stat(geminiGuideDir()); err == nil && fi.IsDir() {
		return []string{geminiGuideDir()}
	}
	return nil
}

func geminiGuideDir() string {
	if abs, err := filepath.Abs(GuideDir()); err == nil {
		return abs
	}
	return GuideDir()
}
