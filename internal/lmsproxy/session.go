package lmsproxy

import (
	"sync"
)

// SessionStore tracks the last response_id per session+model so that
// subsequent requests can use previous_response_id for LM Studio KV cache reuse.
//
// Claude CLI rewrites message content between turns (injecting system-reminders
// etc.), so hash-based prefix matching doesn't work. Instead we track the
// latest response_id per session key and always send only the trailing new
// messages (user + tool_result) as delta.
// SessionConfig holds per-session settings.
type SessionConfig struct {
	ModelOverride string
	AllowedTools  map[string]bool
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]string        // "sessionID\x00model" → last responseID
	configs  map[string]SessionConfig  // sessionID → config
}

// NewSessionStore creates a new empty session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]string),
		configs:  make(map[string]SessionConfig),
	}
}

// SetConfig stores configuration for a session.
func (s *SessionStore) SetConfig(sessionID string, cfg SessionConfig) {
	s.mu.Lock()
	s.configs[sessionID] = cfg
	s.mu.Unlock()
}

// GetConfig returns configuration for a session.
func (s *SessionStore) GetConfig(sessionID string) (SessionConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.configs[sessionID]
	return cfg, ok
}

// Lookup returns the previous_response_id for the model (if any) and extracts
// only the new messages to send as delta. If no session exists, all messages
// are returned and prevID is empty.
func (s *SessionStore) Lookup(sessionID, model string, msgs []AnthropicMessage) (prevID string, newMsgs []AnthropicMessage) {
	key := sessionID + "\x00" + model
	s.mu.Lock()
	rid := s.sessions[key]
	s.mu.Unlock()

	if rid == "" || len(msgs) == 0 {
		return "", msgs
	}

	// Find the last user or tool_result messages at the tail.
	// Everything before that is context LM Studio already has.
	i := len(msgs) - 1
	for i >= 0 && msgs[i].Role == "user" {
		i--
	}
	// Skip the trailing assistant block (LMS has it via previous_response_id).
	for i >= 0 && msgs[i].Role == "assistant" {
		i--
	}
	// i now points to the last message before the new turn.
	// The new turn starts at i+1, but we only want user messages after the assistant block.
	delta := msgs[i+1:]

	// Strip leading assistant messages from delta.
	for len(delta) > 0 && delta[0].Role == "assistant" {
		delta = delta[1:]
	}

	if len(delta) == 0 {
		return "", msgs
	}

	return rid, delta
}

// Store saves a response_id for a model.
func (s *SessionStore) Store(sessionID, model, responseID string) {
	s.mu.Lock()
	key := sessionID + "\x00" + model
	s.sessions[key] = responseID
	// Simple eviction when too many sessions accumulate.
	const maxSessions = 1000
	if len(s.sessions) > maxSessions {
		i := 0
		for k := range s.sessions {
			if i >= maxSessions/2 {
				break
			}
			delete(s.sessions, k)
			i++
		}
	}
	s.mu.Unlock()
}

// Len returns the number of stored sessions.
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Reset clears all stored sessions.
func (s *SessionStore) Reset() {
	s.mu.Lock()
	s.sessions = make(map[string]string)
	s.mu.Unlock()
}
