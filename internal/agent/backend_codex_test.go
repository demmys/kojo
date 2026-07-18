package agent

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// rpcLine builds a JSON-RPC notification line for testing.
func rpcLine(method string, params any) string {
	raw, _ := json.Marshal(params)
	rawMsg := json.RawMessage(raw)
	msg := rpcMessage{
		Method: method,
		Params: &rawMsg,
	}
	data, _ := json.Marshal(msg)
	return string(data)
}

// rpcResponseLine builds a JSON-RPC response line with an ID.
func rpcResponseLine(id int64, result any, rpcErr *rpcError) string {
	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	msg := rpcMessage{ID: &rawID}
	if result != nil {
		raw, _ := json.Marshal(result)
		rawMsg := json.RawMessage(raw)
		msg.Result = &rawMsg
	}
	if rpcErr != nil {
		msg.Error = rpcErr
	}
	data, _ := json.Marshal(msg)
	return string(data)
}

// rpcServerRequestLine builds a server-initiated JSON-RPC request. Codex may
// use either a numeric or string request ID, so keep id polymorphic here.
func rpcServerRequestLine(id any, method string, params any) string {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	return string(data)
}

// collectCodexEvents runs parseCodexStream on the given lines and collects all emitted events.
func collectCodexEvents(t *testing.T, turnStartID int64, lines ...string) ([]ChatEvent, *codexStreamResult) {
	t.Helper()
	input := strings.Join(lines, "\n") + "\n"
	scanner := newCodexLineScanner(strings.NewReader(input))

	var events []ChatEvent
	send := func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}

	result := parseCodexStream(scanner, turnStartID, nil, nil, testLogger(), send)
	return events, result
}

func TestCodexLineScanner(t *testing.T) {
	collect := func(input string) ([]string, error) {
		s := newCodexLineScanner(strings.NewReader(input))
		var lines []string
		for s.Scan() {
			lines = append(lines, s.Text())
		}
		return lines, s.Err()
	}

	t.Run("terminated lines", func(t *testing.T) {
		lines, err := collect("a\nbb\nccc\n")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if want := []string{"a", "bb", "ccc"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("final line without trailing newline", func(t *testing.T) {
		lines, err := collect("a\nlast-no-nl")
		if err != nil {
			t.Fatalf("Err = %v, want nil (EOF must not surface as error)", err)
		}
		if want := []string{"a", "last-no-nl"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("blank lines preserved as empty", func(t *testing.T) {
		lines, _ := collect("a\n\nb\n")
		if want := []string{"a", "", "b"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("CRLF trimmed", func(t *testing.T) {
		lines, _ := collect("a\r\nb\r\n")
		if want := []string{"a", "b"}; !equalStrings(lines, want) {
			t.Errorf("lines = %v, want %v", lines, want)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		lines, err := collect("")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if len(lines) != 0 {
			t.Errorf("lines = %v, want empty", lines)
		}
	})

	t.Run("large line under cap", func(t *testing.T) {
		big := strings.Repeat("z", 5*1024*1024)
		lines, err := collect(big + "\n")
		if err != nil {
			t.Fatalf("Err = %v, want nil", err)
		}
		if len(lines) != 1 || len(lines[0]) != len(big) {
			t.Fatalf("got %d lines (first len %d), want 1 line of len %d", len(lines), func() int {
				if len(lines) > 0 {
					return len(lines[0])
				}
				return -1
			}(), len(big))
		}
	})

	t.Run("line over cap", func(t *testing.T) {
		tooBig := strings.Repeat("z", chathistory.MaxJSONLLineBytes+1)
		lines, err := collect(tooBig + "\n")
		if !errors.Is(err, chathistory.ErrLineTooLarge) {
			t.Fatalf("Err = %v, want ErrLineTooLarge", err)
		}
		if len(lines) != 0 {
			t.Fatalf("lines = %d, want 0 when line exceeds cap", len(lines))
		}
	})
}

// TestCodexLineScanner_NonEOFErrorYieldsPartialLine verifies that a partial
// line followed by a non-EOF read error is yielded once (matching
// bufio.Scanner) and the error is then reported via Err().
func TestCodexLineScanner_NonEOFErrorYieldsPartialLine(t *testing.T) {
	wantErr := io.ErrUnexpectedEOF
	r := io.MultiReader(strings.NewReader("partial-no-newline"), &errReader{err: wantErr})
	s := newCodexLineScanner(r)

	if !s.Scan() {
		t.Fatal("Scan() = false, want true (partial line must be yielded)")
	}
	if got := s.Text(); got != "partial-no-newline" {
		t.Errorf("Text() = %q, want %q", got, "partial-no-newline")
	}
	if s.Scan() {
		t.Error("second Scan() = true, want false")
	}
	if s.Err() != wantErr {
		t.Errorf("Err() = %v, want %v", s.Err(), wantErr)
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseCodexStream_OversizedLine is a regression test for the
// "bufio.Scanner: token too long" hang: a single item/completed notification
// carrying a multi-MB aggregatedOutput (e.g. a command that printed a huge
// blob) must be read intact instead of killing the stream. bufio.Scanner with
// a 1MB max token would have failed here; jsonlLineScanner must read it while
// still enforcing its larger MaxJSONLLineBytes safety cap.
func TestParseCodexStream_OversizedLine(t *testing.T) {
	bigOutput := strings.Repeat("x", 3*1024*1024) // 3MB, well over the old 1MB cap
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id":               "i1",
				"type":             "commandExecution",
				"command":          "cat huge.log",
				"aggregatedOutput": bigOutput,
				"exitCode":         0,
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if !result.turnCompleted {
		t.Fatal("expected turnCompleted = true (stream must survive the oversized line)")
	}
	var gotOutput string
	for _, e := range events {
		if e.Type == "tool_result" {
			gotOutput = e.ToolOutput
		}
	}
	if len(gotOutput) != len(bigOutput) {
		t.Errorf("tool_result output length = %d, want %d (output truncated/lost)", len(gotOutput), len(bigOutput))
	}
}

func TestParseCodexStream_TextDelta(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "Hello ",
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "world",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "Hello world" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "Hello world")
	}
	if !result.turnCompleted {
		t.Error("expected turnCompleted = true")
	}

	textEvents := 0
	for _, e := range events {
		if e.Type == "text" {
			textEvents++
		}
	}
	if textEvents != 2 {
		t.Errorf("expected 2 text events, got %d", textEvents)
	}
}

func TestParseCodexStream_ThinkingDelta(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i1", "type": "agentMessage", "phase": "commentary"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "thinking...",
		}),
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "i2", "type": "agentMessage", "phase": "final_answer"},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i2", "delta": "Answer",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "thinking..." {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "thinking...")
	}
	if result.fullText.String() != "Answer" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "Answer")
	}

	foundThinking := false
	for _, e := range events {
		if e.Type == "thinking" && e.Delta == "thinking..." {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Error("expected thinking event")
	}
}

func TestParseCodexStream_CommandExecution(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "command": "ls -la"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "aggregatedOutput": "file1\nfile2"},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.Name != "shell" {
		t.Errorf("tool name = %q, want %q", tu.Name, "shell")
	}
	if tu.Input != "ls -la" {
		t.Errorf("tool input = %q, want %q", tu.Input, "ls -la")
	}
	if tu.Output != "file1\nfile2" {
		t.Errorf("tool output = %q, want %q", tu.Output, "file1\nfile2")
	}

	foundToolUse := false
	foundToolResult := false
	for _, e := range events {
		if e.Type == "tool_use" && e.ToolName == "shell" {
			foundToolUse = true
		}
		if e.Type == "tool_result" && e.ToolName == "shell" && e.ToolOutput == "file1\nfile2" {
			foundToolResult = true
		}
	}
	if !foundToolUse {
		t.Error("expected tool_use event")
	}
	if !foundToolResult {
		t.Error("expected tool_result event")
	}
}

func TestParseCodexStream_CommandExitCode(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "command": "false"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{"id": "cmd1", "type": "commandExecution", "exitCode": 1},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "exit code: 1" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "exit code: 1")
	}
}

func TestParseCodexStream_MCPToolCall(t *testing.T) {
	events, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall",
				"tool": "read_file", "server": "filesystem",
				"arguments": json.RawMessage(`{"path":"foo.txt"}`),
			},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall",
				"tool": "read_file", "server": "filesystem",
				"result": json.RawMessage(`"file contents here"`),
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	tu := result.toolUses[0]
	if tu.Name != "filesystem/read_file" {
		t.Errorf("tool name = %q, want %q", tu.Name, "filesystem/read_file")
	}

	foundResult := false
	for _, e := range events {
		if e.Type == "tool_result" && e.ToolName == "filesystem/read_file" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("expected tool_result event for MCP tool")
	}
}

func TestParseCodexStream_MCPToolError(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "mcp1", "type": "mcpToolCall", "tool": "broken"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "mcp1", "type": "mcpToolCall", "tool": "broken",
				"error": map[string]string{"message": "tool failed"},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "error: tool failed" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "error: tool failed")
	}
}

func TestParseCodexStream_DynamicToolCall(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "dyn1", "type": "dynamicToolCall", "tool": "search"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "dyn1", "type": "dynamicToolCall", "tool": "search",
				"contentItems": json.RawMessage(`[{"text":"result"}]`),
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != `[{"text":"result"}]` {
		t.Errorf("output = %q", result.toolUses[0].Output)
	}
}

func TestParseCodexStream_DynamicToolFailed(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/started", map[string]any{
			"item": map[string]any{"id": "dyn1", "type": "dynamicToolCall", "tool": "broken"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "dyn1", "type": "dynamicToolCall", "tool": "broken",
				"success": false,
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if len(result.toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(result.toolUses))
	}
	if result.toolUses[0].Output != "failed" {
		t.Errorf("output = %q, want %q", result.toolUses[0].Output, "failed")
	}
}

func TestParseCodexStream_TokenUsage(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("thread/tokenUsage/updated", map[string]any{
			"tokenUsage": map[string]any{
				"last": map[string]int{"inputTokens": 100, "outputTokens": 50},
			},
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.usage == nil {
		t.Fatal("expected usage")
	}
	if result.usage.InputTokens != 100 || result.usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want {100, 50}", result.usage)
	}
}

func TestParseCodexStream_TurnFailed(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{
				"status": "failed",
				"error":  map[string]any{"code": -1, "message": "something broke"},
			},
		}),
	)

	if !result.turnCompleted {
		t.Error("expected turnCompleted = true")
	}
	if result.processError != "something broke" {
		t.Errorf("processError = %q, want %q", result.processError, "something broke")
	}
}

func TestParseCodexStream_TurnInterrupted(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "interrupted"},
		}),
	)

	if result.processError != "codex turn interrupted" {
		t.Errorf("processError = %q, want %q", result.processError, "codex turn interrupted")
	}
}

func TestParseCodexStream_TurnStartError(t *testing.T) {
	var turnID int64 = 5
	events, result := collectCodexEvents(t, turnID,
		rpcResponseLine(turnID, nil, &rpcError{Code: -32600, Message: "invalid request"}),
	)

	if !result.cancelled {
		t.Error("expected cancelled = true on turn/start error")
	}

	foundError := false
	for _, e := range events {
		if e.Type == "error" && strings.Contains(e.ErrorMessage, "invalid request") {
			foundError = true
		}
	}
	if !foundError {
		t.Error("expected error event for turn/start failure")
	}
}

func TestParseCodexStream_ReasoningDelta(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/reasoning/summaryTextDelta", map[string]any{
			"delta": "reasoning step",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.thinking.String() != "reasoning step" {
		t.Errorf("thinking = %q, want %q", result.thinking.String(), "reasoning step")
	}
}

func TestParseCodexStream_EmptyLines(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		"",
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "ok",
		}),
		"",
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "ok" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "ok")
	}
}

func TestParseCodexStream_InvalidJSON(t *testing.T) {
	_, result := collectCodexEvents(t, 1,
		"not json",
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "valid",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	)

	if result.fullText.String() != "valid" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "valid")
	}
}

func TestParseCodexStream_FailsPluginInstallAndContinues(t *testing.T) {
	input := strings.Join([]string{
		rpcLine("item/started", map[string]any{
			"item": map[string]any{
				"id": "plugin-tool-1", "type": "dynamicToolCall",
				"tool": "request_plugin_install",
			},
		}),
		rpcServerRequestLine("plugin-install-1", "item/tool/call", map[string]any{
			"tool":      "request_plugin_install",
			"arguments": map[string]string{"plugin_id": "google-drive@openai-curated-remote"},
		}),
		rpcLine("item/completed", map[string]any{
			"item": map[string]any{
				"id": "plugin-tool-1", "type": "dynamicToolCall",
				"tool": "request_plugin_install", "success": false,
			},
		}),
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "fallback answer",
		}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	}, "\n") + "\n"

	var wire strings.Builder
	respond := newCodexServerRequestResponder(func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wire.Write(data)
		wire.WriteByte('\n')
		return nil
	})
	result := parseCodexStream(
		newCodexLineScanner(strings.NewReader(input)),
		1,
		nil,
		respond,
		testLogger(),
		func(ChatEvent) bool { return true },
	)

	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Success      bool `json:"success"`
			ContentItems []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"contentItems"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(wire.String())), &response); err != nil {
		t.Fatalf("decode tool failure response: %v; wire=%q", err, wire.String())
	}
	if got := string(response.ID); got != `"plugin-install-1"` {
		t.Errorf("tool failure response ID = %s, want quoted string ID", got)
	}
	if response.JSONRPC != "2.0" || response.Result.Success {
		t.Errorf("tool failure response = %+v, want unsuccessful JSON-RPC result", response)
	}
	if len(response.Result.ContentItems) != 1 || !strings.Contains(response.Result.ContentItems[0].Text, "normal chat") {
		t.Errorf("tool failure contentItems = %+v, want fallback guidance", response.Result.ContentItems)
	}
	if !result.turnCompleted {
		t.Fatal("turnCompleted = false; server request must not stop the stream")
	}
	if got := result.fullText.String(); got != "fallback answer" {
		t.Errorf("fullText = %q, want fallback answer", got)
	}
	if len(result.toolUses) != 1 || result.toolUses[0].Output != "failed" {
		t.Errorf("tool uses = %+v, want completed failed request_plugin_install call", result.toolUses)
	}
}

func TestWaitCodexRPCResponse_RejectsServerRequestAndContinues(t *testing.T) {
	input := strings.Join([]string{
		rpcServerRequestLine(91, "mcpServer/elicitation/request", map[string]any{}),
		rpcResponseLine(7, map[string]any{"ok": true}, nil),
	}, "\n") + "\n"

	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	msg, ok, err := waitCodexRPCResponse(
		newCodexLineScanner(strings.NewReader(input)),
		7,
		respond,
		testLogger(),
	)
	if err != nil {
		t.Fatalf("waitCodexRPCResponse error: %v", err)
	}
	if !ok || msg == nil {
		t.Fatal("waitCodexRPCResponse did not return the target response")
	}
	var response struct {
		ID     json.RawMessage   `json:"id"`
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode elicitation response: %v", err)
	}
	if got := string(response.ID); got != "91" || response.Result["action"] != "decline" {
		t.Errorf("elicitation response = %+v, want numeric ID 91 and decline", response)
	}
	if id, ok := msg.numericID(); !ok || id != 7 {
		t.Errorf("response ID = (%d, %v), want (7, true)", id, ok)
	}
}

func TestParseCodexStream_ServerRequestRejectionWriteFailureStops(t *testing.T) {
	input := strings.Join([]string{
		rpcServerRequestLine(91, "item/tool/call", map[string]any{"tool": "request_plugin_install"}),
		rpcLine("turn/completed", map[string]any{
			"turn": map[string]any{"status": "completed"},
		}),
	}, "\n") + "\n"

	var events []ChatEvent
	result := parseCodexStream(
		newCodexLineScanner(strings.NewReader(input)),
		1,
		nil,
		newCodexServerRequestResponder(func(any) error { return errors.New("broken stdin") }),
		testLogger(),
		func(event ChatEvent) bool {
			events = append(events, event)
			return true
		},
	)

	if !result.cancelled || result.turnCompleted {
		t.Fatalf("result = %+v, want cancelled before turn completion", result)
	}
	if len(events) != 1 || events[0].Type != "error" || !strings.Contains(events[0].ErrorMessage, "broken stdin") {
		t.Fatalf("events = %+v, want rejection write error", events)
	}
}

func TestCodexServerRequestResponder_HandlesKnownInteractiveMethods(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantResult map[string]any
	}{
		{name: "command approval", method: "item/commandExecution/requestApproval", wantResult: map[string]any{"decision": "decline"}},
		{name: "file approval", method: "item/fileChange/requestApproval", wantResult: map[string]any{"decision": "decline"}},
		{name: "legacy command approval", method: "execCommandApproval", wantResult: map[string]any{"decision": "denied"}},
		{name: "legacy patch approval", method: "applyPatchApproval", wantResult: map[string]any{"decision": "denied"}},
		{name: "MCP elicitation", method: "mcpServer/elicitation/request", wantResult: map[string]any{"action": "decline"}},
		{name: "user input", method: "item/tool/requestUserInput", wantResult: map[string]any{"answers": map[string]any{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wire []byte
			respond := newCodexServerRequestResponder(func(v any) error {
				var err error
				wire, err = json.Marshal(v)
				return err
			})
			id := json.RawMessage(`42`)
			params := json.RawMessage(`{}`)
			if _, err := respond(&rpcMessage{ID: &id, Method: tt.method, Params: &params}); err != nil {
				t.Fatalf("respond: %v", err)
			}

			var response struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(wire, &response); err != nil {
				t.Fatalf("decode result response: %v", err)
			}
			for key, want := range tt.wantResult {
				if got := response.Result[key]; !reflect.DeepEqual(got, want) {
					t.Errorf("result[%q] = %#v, want %#v", key, got, want)
				}
			}
		})
	}
}

func TestCodexServerRequestResponder_DeclinesAdditionalPermissions(t *testing.T) {
	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	id := json.RawMessage(`43`)
	params := json.RawMessage(`{}`)
	if _, err := respond(&rpcMessage{ID: &id, Method: "item/permissions/requestApproval", Params: &params}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	var response struct {
		Result struct {
			Permissions struct {
				FileSystem any `json:"fileSystem"`
				Network    any `json:"network"`
			} `json:"permissions"`
			Scope string `json:"scope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode permissions response: %v", err)
	}
	if response.Result.Scope != "turn" || response.Result.Permissions.FileSystem != nil || response.Result.Permissions.Network != nil {
		t.Errorf("permissions response = %+v, want no additional permissions for turn", response.Result)
	}
}

func TestCodexServerRequestResponder_ReturnsCurrentTime(t *testing.T) {
	var wire []byte
	respond := newCodexServerRequestResponder(func(v any) error {
		var err error
		wire, err = json.Marshal(v)
		return err
	})
	id := json.RawMessage(`44`)
	params := json.RawMessage(`{}`)
	before := time.Now().Unix()
	if _, err := respond(&rpcMessage{ID: &id, Method: "currentTime/read", Params: &params}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	after := time.Now().Unix()
	var response struct {
		Result struct {
			CurrentTimeAt int64 `json:"currentTimeAt"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatalf("decode current time response: %v", err)
	}
	if got := response.Result.CurrentTimeAt; got < before || got > after {
		t.Errorf("currentTimeAt = %d, want between %d and %d", got, before, after)
	}
}

func TestParseCodexStream_CriticalOrUnknownServerRequestStops(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
		want   string
	}{
		{name: "auth refresh", method: "account/chatgptAuthTokens/refresh", params: map[string]any{}, want: "infrastructure request"},
		{name: "attestation", method: "attestation/generate", params: map[string]any{}, want: "infrastructure request"},
		{name: "unknown method", method: "future/newRequest", params: map[string]any{}, want: "unknown Codex server request"},
		{name: "unknown dynamic tool", method: "item/tool/call", params: map[string]any{"tool": "future_tool"}, want: `dynamic client tool "future_tool"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Join([]string{
				rpcServerRequestLine("server-1", tt.method, tt.params),
				rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
			}, "\n") + "\n"
			var wireCalls int
			var events []ChatEvent
			result := parseCodexStream(
				newCodexLineScanner(strings.NewReader(input)),
				1,
				nil,
				newCodexServerRequestResponder(func(any) error { wireCalls++; return nil }),
				testLogger(),
				func(event ChatEvent) bool { events = append(events, event); return true },
			)
			if !result.cancelled || result.turnCompleted {
				t.Fatalf("result = %+v, want immediate stop", result)
			}
			if wireCalls != 0 {
				t.Fatalf("wire calls = %d, want no generic response for critical request", wireCalls)
			}
			if len(events) != 1 || events[0].Type != "error" || !strings.Contains(events[0].ErrorMessage, tt.want) {
				t.Fatalf("events = %+v, want explicit compatibility error containing %q", events, tt.want)
			}
		})
	}
}

func TestCodexServerRequestResponder_PreservesRequestID(t *testing.T) {
	for _, rawID := range []string{`91`, `"plugin-install-1"`} {
		t.Run(rawID, func(t *testing.T) {
			var wire []byte
			respond := newCodexServerRequestResponder(func(v any) error {
				var err error
				wire, err = json.Marshal(v)
				return err
			})

			id := json.RawMessage(rawID)
			params := json.RawMessage(`{"tool":"request_plugin_install","arguments":{}}`)
			if _, err := respond(&rpcMessage{ID: &id, Method: "item/tool/call", Params: &params}); err != nil {
				t.Fatalf("respond: %v", err)
			}
			var response rpcResultResponse
			if err := json.Unmarshal(wire, &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got := string(response.ID); got != rawID {
				t.Errorf("response ID = %s, want %s", got, rawID)
			}
		})
	}
}

func TestParseCodexStream_Cancelled(t *testing.T) {
	input := rpcLine("item/agentMessage/delta", map[string]any{
		"itemId": "i1", "delta": "a",
	}) + "\n" + rpcLine("item/agentMessage/delta", map[string]any{
		"itemId": "i1", "delta": "b",
	}) + "\n"

	scanner := newCodexLineScanner(strings.NewReader(input))

	callCount := 0
	send := func(e ChatEvent) bool {
		callCount++
		return callCount < 2
	}

	result := parseCodexStream(scanner, 1, nil, nil, testLogger(), send)
	if !result.cancelled {
		t.Error("expected cancelled = true")
	}
}

func TestParseCodexStream_NoTurnCompleted(t *testing.T) {
	// Scanner ends without turn/completed
	_, result := collectCodexEvents(t, 1,
		rpcLine("item/agentMessage/delta", map[string]any{
			"itemId": "i1", "delta": "partial",
		}),
	)

	if result.turnCompleted {
		t.Error("expected turnCompleted = false")
	}
	if !result.hasOutput() {
		t.Error("expected hasOutput = true")
	}
	if result.fullText.String() != "partial" {
		t.Errorf("fullText = %q, want %q", result.fullText.String(), "partial")
	}
}

func TestCodexStreamResult_BuildMessage(t *testing.T) {
	res := &codexStreamResult{
		usage: &Usage{InputTokens: 10, OutputTokens: 5},
	}
	res.fullText.WriteString("hello")
	res.thinking.WriteString("thought")
	res.toolUses = []ToolUse{{Name: "shell"}}

	msg := res.buildMessage()
	if msg.Content != "hello" {
		t.Errorf("Content = %q, want %q", msg.Content, "hello")
	}
	if msg.Thinking != "thought" {
		t.Errorf("Thinking = %q, want %q", msg.Thinking, "thought")
	}
	if len(msg.ToolUses) != 1 {
		t.Errorf("ToolUses len = %d, want 1", len(msg.ToolUses))
	}
	if msg.Usage != res.usage {
		t.Error("Usage should match")
	}
}

func TestCodexStreamResult_HasOutput(t *testing.T) {
	res := &codexStreamResult{}
	if res.hasOutput() {
		t.Error("empty result should not have output")
	}

	res.fullText.WriteString("text")
	if !res.hasOutput() {
		t.Error("result with text should have output")
	}

	res2 := &codexStreamResult{}
	res2.toolUses = []ToolUse{{Name: "shell"}}
	if !res2.hasOutput() {
		t.Error("result with tool uses should have output")
	}
}

func TestCodexEffortForProtocol_MaxGating(t *testing.T) {
	// gpt-5.6 family: max passes through (codex CLI 0.144.1).
	for _, m := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if got := codexEffortForProtocol(m, "max"); got != "max" {
			t.Errorf("codexEffortForProtocol(%q, max) = %q, want max", m, got)
		}
	}
	// Older codex models and the empty (CLI-default) model drop max so
	// the CLI falls back to its default effort instead of erroring.
	for _, m := range []string{"gpt-5.5", ""} {
		if got := codexEffortForProtocol(m, "max"); got != "" {
			t.Errorf("codexEffortForProtocol(%q, max) = %q, want empty", m, got)
		}
	}
	// Non-max levels are model-independent.
	if got := codexEffortForProtocol("", "xhigh"); got != "xhigh" {
		t.Errorf("codexEffortForProtocol(\"\", xhigh) = %q, want xhigh", got)
	}
	if got := codexEffortForProtocol("gpt-5.6-sol", "bogus"); got != "" {
		t.Errorf("expected bogus effort to map to empty, got %q", got)
	}
}
