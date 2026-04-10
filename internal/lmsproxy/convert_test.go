package lmsproxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractSystem_String(t *testing.T) {
	raw := json.RawMessage(`"You are helpful"`)
	got := extractSystem(raw)
	if got != "You are helpful" {
		t.Errorf("got %q, want %q", got, "You are helpful")
	}
}

func TestExtractSystem_Array(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]`)
	got := extractSystem(raw)
	if got != "Part 1\n\nPart 2" {
		t.Errorf("got %q, want %q", got, "Part 1\n\nPart 2")
	}
}

func TestExtractSystem_Empty(t *testing.T) {
	got := extractSystem(nil)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseContent_String(t *testing.T) {
	raw := json.RawMessage(`"hello"`)
	blocks, err := parseContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Text != "hello" {
		t.Errorf("unexpected blocks: %+v", blocks)
	}
}

func TestParseContent_Array(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hi"},{"type":"tool_use","id":"tu_1","name":"read","input":{"path":"f.txt"}}]`)
	blocks, err := parseContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "hi" {
		t.Errorf("block 0: %+v", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].Name != "read" || blocks[1].ID != "tu_1" {
		t.Errorf("block 1: %+v", blocks[1])
	}
}

func TestConvertMessages_UserText(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}
	items, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Type != "message" || items[0].Role != "user" {
		t.Errorf("item: %+v", items[0])
	}
	if items[0].Content[0].Type != "input_text" || items[0].Content[0].Text != "hello" {
		t.Errorf("content: %+v", items[0].Content[0])
	}
}

func TestConvertMessages_AssistantText(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`"hi there"`)},
	}
	items, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Content[0].Type != "output_text" {
		t.Errorf("expected output_text, got %s", items[0].Content[0].Type)
	}
}

func TestConvertMessages_ToolResult(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu_1","content":"file contents"}]`)},
	}
	items, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Type != "function_call_output" || items[0].CallID != "tu_1" {
		t.Errorf("item: %+v", items[0])
	}
}

func TestConvertMessages_ToolUse(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu_1","name":"read","input":{"path":"f.txt"}}]`)},
	}
	items, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Type != "function_call" || items[0].Name != "read" || items[0].CallID != "tu_1" {
		t.Errorf("item: %+v", items[0])
	}
}

func TestBuildOAIRequest_FirstMessage(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "test-model",
		MaxTokens: 1024,
		System:    json.RawMessage(`"Be helpful"`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Tools: []AnthropicTool{
			{Name: "read", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Stream: true,
	}
	newMsgs := req.Messages

	oai, err := BuildOAIRequest(req, "", newMsgs, nil)
	if err != nil {
		t.Fatal(err)
	}

	if oai.Model != "test-model" {
		t.Errorf("model: %s", oai.Model)
	}
	if oai.Instructions != "Be helpful" {
		t.Errorf("instructions: %s", oai.Instructions)
	}
	if oai.PreviousResponseID != "" {
		t.Errorf("unexpected prevID: %s", oai.PreviousResponseID)
	}
	if oai.MaxOutputTokens != 1024 {
		t.Errorf("max_output_tokens: %d", oai.MaxOutputTokens)
	}
	if len(oai.Tools) != 1 || oai.Tools[0].Name != "read" {
		t.Errorf("tools: %+v", oai.Tools)
	}
}

func TestBuildOAIRequest_WithPrevID(t *testing.T) {
	req := &AnthropicRequest{
		Model:  "test-model",
		System: json.RawMessage(`"Be helpful"`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`"hi"`)},
			{Role: "user", Content: json.RawMessage(`"how are you"`)},
		},
	}
	// Only the new message.
	newMsgs := req.Messages[2:]

	oai, err := BuildOAIRequest(req, "resp_123", newMsgs, nil)
	if err != nil {
		t.Fatal(err)
	}

	if oai.PreviousResponseID != "resp_123" {
		t.Errorf("prevID: %s", oai.PreviousResponseID)
	}
	if oai.Instructions != "" {
		t.Errorf("instructions should be empty with prevID (LMS restores from session), got %q", oai.Instructions)
	}

	// Should only contain the new message.
	var items []OAIInputItem
	json.Unmarshal(oai.Input, &items)
	if len(items) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(items))
	}
}

// --- StreamConverter tests ---

func TestStreamConverter_TextStream(t *testing.T) {
	sse := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"test"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":" world"}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"test","usage":{"input_tokens":10,"output_tokens":2}}}

`
	w := httptest.NewRecorder()
	conv := NewStreamConverter(w, "test-model")
	err := conv.Process(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()

	// Check key events are present.
	if !strings.Contains(body, "event: message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(body, "event: content_block_start") {
		t.Error("missing content_block_start")
	}
	if !strings.Contains(body, `"text_delta"`) {
		t.Error("missing text_delta")
	}
	if !strings.Contains(body, `"Hello"`) {
		t.Error("missing Hello delta")
	}
	if !strings.Contains(body, "event: content_block_stop") {
		t.Error("missing content_block_stop")
	}
	if !strings.Contains(body, "event: message_delta") {
		t.Error("missing message_delta")
	}
	if !strings.Contains(body, `"end_turn"`) {
		t.Error("missing end_turn stop_reason")
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Error("missing message_stop")
	}

	if conv.ResponseID() != "resp_1" {
		t.Errorf("responseID: %s", conv.ResponseID())
	}
}

func TestStreamConverter_ToolUseStream(t *testing.T) {
	sse := `event: response.created
data: {"type":"response.created","response":{"id":"resp_2","model":"test"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"read_file","call_id":"call_1"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"path\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"\"test.txt\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","name":"read_file","call_id":"call_1","arguments":"{\"path\":\"test.txt\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","model":"test","usage":{"input_tokens":5,"output_tokens":10}}}

`
	w := httptest.NewRecorder()
	conv := NewStreamConverter(w, "test-model")
	err := conv.Process(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()

	if !strings.Contains(body, `"tool_use"`) {
		t.Error("missing tool_use content block")
	}
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Error("missing input_json_delta")
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Error("missing tool_use stop_reason")
	}

	if conv.ResponseID() != "resp_2" {
		t.Errorf("responseID: %s", conv.ResponseID())
	}
}

func TestStreamConverter_Error(t *testing.T) {
	sse := `event: error
data: {"message":"model not found"}

`
	w := httptest.NewRecorder()
	conv := NewStreamConverter(w, "test-model")
	_ = conv.Process(strings.NewReader(sse))

	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Error("missing error event")
	}
	if !strings.Contains(body, "model not found") {
		t.Error("missing error message")
	}
}

// Ensure the recorder implements http.Flusher for the test.
var _ io.Writer = httptest.NewRecorder()
