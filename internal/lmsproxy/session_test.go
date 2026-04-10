package lmsproxy

import (
	"encoding/json"
	"testing"
)

func TestSessionStore_FirstLookup(t *testing.T) {
	s := NewSessionStore()
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}

	prevID, newMsgs := s.Lookup("s1", "m", msgs)
	if prevID != "" {
		t.Errorf("expected empty prevID, got %q", prevID)
	}
	if len(newMsgs) != 1 {
		t.Errorf("expected 1 new message, got %d", len(newMsgs))
	}
}

func TestSessionStore_ContinuedConversation(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "m", "resp_1")

	// Second turn: previous messages + new user message.
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
		{Role: "assistant", Content: json.RawMessage(`"hi"`)},
		{Role: "user", Content: json.RawMessage(`"how are you"`)},
	}

	prevID, newMsgs := s.Lookup("s1", "m", msgs)
	if prevID != "resp_1" {
		t.Errorf("expected resp_1, got %q", prevID)
	}
	// Should only send the new user message (assistant skipped).
	if len(newMsgs) != 1 {
		t.Errorf("expected 1 new message, got %d", len(newMsgs))
	}
	if len(newMsgs) > 0 && newMsgs[0].Role != "user" {
		t.Errorf("expected user message, got %s", newMsgs[0].Role)
	}
}

func TestSessionStore_DifferentModel(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "model-a", "resp_a")

	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}
	prevID, _ := s.Lookup("s1", "model-b", msgs)
	if prevID != "" {
		t.Errorf("expected empty prevID for different model, got %q", prevID)
	}
}

func TestSessionStore_ToolResultDelta(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "m", "resp_1")

	// Tool use flow: user → assistant(tool_use) → user(tool_result)
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"read file"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu1","name":"read","input":{}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":"file contents"}]`)},
	}

	prevID, newMsgs := s.Lookup("s1", "m", msgs)
	if prevID != "resp_1" {
		t.Errorf("expected resp_1, got %q", prevID)
	}
	// Should send the tool_result user message (assistant skipped).
	if len(newMsgs) != 1 {
		t.Errorf("expected 1 new message, got %d", len(newMsgs))
	}
}

func TestSessionStore_Reset(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "m", "resp_1")

	s.Reset()

	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}
	prevID, _ := s.Lookup("s1", "m", msgs)
	if prevID != "" {
		t.Errorf("expected empty prevID after reset, got %q", prevID)
	}
}

func TestSessionStore_MultiTurnChain(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "m", "r1")

	// Second response overwrites.
	s.Store("s1", "m", "r2")

	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`"b"`)},
		{Role: "user", Content: json.RawMessage(`"c"`)},
		{Role: "assistant", Content: json.RawMessage(`"d"`)},
		{Role: "user", Content: json.RawMessage(`"e"`)},
	}
	prevID, newMsgs := s.Lookup("s1", "m", msgs)
	if prevID != "r2" {
		t.Errorf("expected r2, got %q", prevID)
	}
	// Only the last user message.
	if len(newMsgs) != 1 {
		t.Errorf("expected 1 new message, got %d", len(newMsgs))
	}
}

func TestSessionStore_SingleUserMessage(t *testing.T) {
	s := NewSessionStore()
	s.Store("s1", "m", "resp_1")

	// Only one user message — no assistant to skip.
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}
	prevID, newMsgs := s.Lookup("s1", "m", msgs)
	if prevID != "resp_1" {
		t.Errorf("expected resp_1, got %q", prevID)
	}
	if len(newMsgs) != 1 {
		t.Errorf("expected 1, got %d", len(newMsgs))
	}
}
