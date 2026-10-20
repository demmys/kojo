package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"
)

// geminiStreamResult collects one `opencode run --format json` invocation's
// streamed output.
type geminiStreamResult struct {
	text        string
	thinking    string
	toolUses    []ToolUse
	usage       *Usage
	sessionID   string
	streamError string // last "error" event's message
	anyEvent    bool   // at least one JSON event was read
	finished    bool   // a step_finish with a terminal reason was seen
	cancelled   bool
}

type openCodeTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type openCodeStreamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      *struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  *struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Reason string          `json:"reason"`
		Tokens *openCodeTokens `json:"tokens"`
	} `json:"part"`
	Error *struct {
		Name string `json:"name"`
		Data *struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	Delta string `json:"delta"` // kojo_delta only
}

// openCodeLivePart is one text or reasoning part as it streams.
type openCodeLivePart struct {
	streamed  string // concatenated deltas
	started   bool   // the part has been sent (from here on deltas go out as-is)
	completed bool
	text      string // the completed part's text
}

// openCodePartStream merges a kind's (text or reasoning) live deltas with
// its completed parts: each part is sent once, deltas first and then any
// suffix the completed part adds; parts are separated by a blank line and
// whitespace-only parts are skipped, as in the completed-only output.
type openCodePartStream struct {
	order []*openCodeLivePart
	byID  map[string]*openCodeLivePart
	sent  bool
}

func (s *openCodePartStream) part(id string) *openCodeLivePart {
	if p := s.byID[id]; id != "" && p != nil {
		return p
	}
	p := &openCodeLivePart{}
	s.order = append(s.order, p)
	if id != "" {
		if s.byID == nil {
			s.byID = map[string]*openCodeLivePart{}
		}
		s.byID[id] = p
	}
	return p
}

func (s *openCodePartStream) lead() string {
	if s.sent {
		return "\n\n"
	}
	s.sent = true
	return ""
}

// delta records a live delta and returns the text to send ("" = none).
func (s *openCodePartStream) delta(id, d string) string {
	p := s.part(id)
	if p.completed || d == "" {
		return ""
	}
	p.streamed += d
	if p.started {
		return d
	}
	if strings.TrimSpace(p.streamed) == "" {
		return ""
	}
	p.started = true
	return s.lead() + p.streamed
}

// complete records a completed part and returns what is left to send. A
// completed text that does not extend what was streamed (never seen in
// practice) sends nothing more; String() still uses the completed text,
// and the done message carries it.
func (s *openCodePartStream) complete(id, text string) string {
	p := s.part(id)
	if p.completed {
		return ""
	}
	p.completed = true
	p.text = text
	if strings.TrimSpace(text) == "" {
		return ""
	}
	if !p.started {
		p.started = true
		return s.lead() + text
	}
	if strings.HasPrefix(text, p.streamed) {
		return text[len(p.streamed):]
	}
	return ""
}

// String is the kind's final text: completed parts as completed, parts
// cut off before completing (cancel, crash) as streamed so far.
func (s *openCodePartStream) String() string {
	var b strings.Builder
	for _, p := range s.order {
		t := p.streamed
		if p.completed {
			t = p.text
		}
		if strings.TrimSpace(t) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t)
	}
	return b.String()
}

// geminiNow is the clock used to pick the price step; tests override it.
var geminiNow = time.Now

// parseGeminiStream reads `opencode run --format json --thinking` NDJSON
// from r. Each line is {type, timestamp, sessionID, part | error}; the
// types used here (opencode 1.19.3):
//
//	step_start   a model request starts
//	text         a completed assistant text part (part.text)
//	reasoning    a completed reasoning part (only with --thinking)
//	tool_use     a tool call that finished: part.tool, part.callID,
//	             part.state{status:"completed"|"error", input, output, error}
//	step_finish  a model request finished: part.reason ("tool-calls",
//	             "stop", ...), part.tokens{input (uncached), output,
//	             reasoning, cache{read, write}} for that request only
//	error        session or prompt error: error{name, data{message}}
//
// OpenCode emits only the root session's parts (subagent parts are
// filtered out) and only completed text/reasoning parts. kojo's plugin
// (openCodeEnvPlugin) adds kojo_delta lines (live text/reasoning deltas,
// sent as they arrive; the completed part then only sends what the
// deltas missed), kojo_tool_start (a tool call starts running: tool_use
// is sent then, tool_result when it finishes) and kojo_child_step
// (subagent usage, added to the turn's usage). Consecutive parts are
// separated by a blank line. Non-JSON lines are ignored; unknown types
// are skipped.
func parseGeminiStream(r io.Reader, logger *slog.Logger, send func(ChatEvent) bool) *geminiStreamResult {
	res := &geminiStreamResult{}
	scanner := newSkippingLineScanner(r)
	loggedSkips := 0
	var text, thinking openCodePartStream
	seenParts := map[string]bool{}
	startedTools := map[string]bool{}
	var tokens openCodeTokens
	steps := 0
	finish := func(cancelled bool) *geminiStreamResult {
		res.text = text.String()
		res.thinking = thinking.String()
		res.cancelled = cancelled
		if steps > 0 {
			res.usage = &Usage{
				// cache.write is billed at the input rate; OutputTokens
				// includes thinking, which bills at the output rate.
				InputTokens:          tokens.Input + tokens.Cache.Write,
				OutputTokens:         tokens.Output + tokens.Reasoning,
				CacheReadInputTokens: tokens.Cache.Read,
			}
		}
		return res
	}
	addTokens := func(t *openCodeTokens) {
		if t == nil {
			return
		}
		steps++
		tokens.Input += t.Input
		tokens.Output += t.Output
		tokens.Reasoning += t.Reasoning
		tokens.Cache.Read += t.Cache.Read
		tokens.Cache.Write += t.Cache.Write
	}

	for scanner.Scan() {
		if n := scanner.Skipped(); n > loggedSkips {
			logger.Warn("opencode stream: dropped oversized JSONL line(s)", "dropped", n-loggedSkips)
			loggedSkips = n
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev openCodeStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			logger.Debug("opencode parse line", "err", err)
			continue
		}
		fromPlugin := strings.HasPrefix(ev.Type, "kojo_")
		if !fromPlugin {
			res.anyEvent = true
			if res.sessionID == "" && isGeminiSessionID(ev.SessionID) {
				res.sessionID = ev.SessionID
			}
		}
		if ev.Type == "error" {
			if msg := openCodeErrorMessage(ev.Error); msg != "" {
				res.streamError = msg
			}
			continue
		}
		p := ev.Part
		if p == nil {
			continue
		}
		if (ev.Type == "kojo_delta" || ev.Type == "kojo_tool_start") && res.sessionID != "" && ev.SessionID != res.sessionID {
			continue // the plugin filters subagents already; belt and braces
		}
		if p.ID != "" && ev.Type != "kojo_delta" {
			key := ev.Type + "/" + p.ID
			if seenParts[key] {
				continue
			}
			seenParts[key] = true
		}
		switch ev.Type {
		case "kojo_delta":
			var d string
			var typ string
			switch p.Type {
			case "text":
				d, typ = text.delta(p.ID, ev.Delta), "text"
			case "reasoning":
				d, typ = thinking.delta(p.ID, ev.Delta), "thinking"
			}
			if d != "" && !send(ChatEvent{Type: typ, Delta: d}) {
				return finish(true)
			}
		case "text":
			if d := text.complete(p.ID, p.Text); d != "" && !send(ChatEvent{Type: "text", Delta: d}) {
				return finish(true)
			}
		case "reasoning":
			if d := thinking.complete(p.ID, p.Text); d != "" && !send(ChatEvent{Type: "thinking", Delta: d}) {
				return finish(true)
			}
		case "kojo_tool_start", "tool_use":
			if p.State == nil {
				continue
			}
			id := p.CallID
			if id == "" {
				id = p.ID
			}
			input := ""
			if len(p.State.Input) > 0 && string(p.State.Input) != "null" {
				var compact bytes.Buffer
				if json.Compact(&compact, p.State.Input) == nil {
					input = compact.String()
				} else {
					input = string(p.State.Input)
				}
			}
			if !startedTools[id] {
				startedTools[id] = true
				res.toolUses = append(res.toolUses, ToolUse{ID: id, Name: p.Tool, Input: input})
				if !send(ChatEvent{Type: "tool_use", ToolUseID: id, ToolName: p.Tool, ToolInput: input}) {
					return finish(true)
				}
			} else if input != "" {
				for i := range res.toolUses {
					if res.toolUses[i].ID == id {
						res.toolUses[i].Input = input
					}
				}
			}
			if ev.Type == "kojo_tool_start" {
				continue
			}
			output := geminiJSONText(p.State.Output)
			if p.State.Status == "error" && p.State.Error != "" {
				if output != "" {
					output += "\n"
				}
				output += "Error: " + p.State.Error
			}
			matchToolOutput(res.toolUses, id, p.Tool, output)
			if !send(ChatEvent{Type: "tool_result", ToolUseID: id, ToolName: p.Tool, ToolOutput: output}) {
				return finish(true)
			}
		case "kojo_child_step":
			addTokens(p.Tokens) // a subagent's model request (task tool)
		case "step_finish":
			addTokens(p.Tokens)
			if p.Reason != "tool-calls" {
				res.finished = true
			}
		}
	}
	return finish(false)
}

func openCodeErrorMessage(e *struct {
	Name string `json:"name"`
	Data *struct {
		Message string `json:"message"`
	} `json:"data"`
}) string {
	if e == nil {
		return ""
	}
	if e.Data != nil && strings.TrimSpace(e.Data.Message) != "" {
		if e.Name != "" && e.Name != "UnknownError" {
			return e.Name + ": " + strings.TrimSpace(e.Data.Message)
		}
		return strings.TrimSpace(e.Data.Message)
	}
	return e.Name
}

// geminiJSONText renders a tool output: strings verbatim, anything else
// as compact JSON.
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
// Preference: the stream's own error event, then a failed exit (with
// stderr), then a run that never finished a model step — OpenCode prints
// fatal startup errors (bad model, missing session) as plain text.
func classifyGeminiProcessError(waitErr error, stderr string, res *geminiStreamResult) string {
	if res.streamError != "" {
		return res.streamError
	}
	trimmed := strings.TrimSpace(stripANSI(stderr))
	if waitErr != nil {
		if trimmed != "" {
			return "opencode: " + waitErr.Error() + ": " + trimmed
		}
		return "opencode: " + waitErr.Error()
	}
	if !res.finished {
		if trimmed != "" {
			return "opencode exited without a result: " + trimmed
		}
		return "opencode exited without a result"
	}
	return ""
}

// isGeminiStaleSessionError matches OpenCode's failed `-s <id>` lookup
// (stderr "Error: Session not found", exit 1, no events).
func isGeminiStaleSessionError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "session not found")
}

// stripANSI removes terminal escape sequences OpenCode's UI.error adds.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
