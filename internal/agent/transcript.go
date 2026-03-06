package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

const messagesFile = "messages.jsonl"

// appendMessage appends a message to the agent's JSONL transcript.
func appendMessage(agentID string, msg *Message) error {
	dir := agentDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	path := filepath.Join(dir, messagesFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

// loadMessages reads the last N messages from the agent's JSONL transcript.
// If limit <= 0, all messages are returned.
func loadMessages(agentID string, limit int) ([]*Message, error) {
	msgs, _, err := loadMessagesPaginated(agentID, limit, "")
	return msgs, err
}

// loadMessagesPaginated reads messages with cursor-based pagination.
// If before is non-empty, returns the last `limit` messages before that ID.
// Returns the messages and whether there are more older messages.
func loadMessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	path := filepath.Join(agentDir(agentID), messagesFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	var all []*Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1MB per line
	for scanner.Scan() {
		var msg Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue // skip malformed lines
		}
		all = append(all, &msg)
	}
	if err := scanner.Err(); err != nil {
		return all, false, err
	}

	// If before cursor is set, slice up to that message
	if before != "" {
		idx := -1
		for i, m := range all {
			if m.ID == before {
				idx = i
				break
			}
		}
		if idx >= 0 {
			all = all[:idx]
		}
	}

	hasMore := false
	if limit > 0 && len(all) > limit {
		hasMore = true
		all = all[len(all)-limit:]
	}
	return all, hasMore, nil
}
