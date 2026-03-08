package agent

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// GroupDM represents a group conversation between agents.
type GroupDM struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Members   []GroupMember `json:"members"`
	CreatedAt string        `json:"createdAt"`
	UpdatedAt string        `json:"updatedAt"`
}

// GroupMember is a participant in a group DM.
type GroupMember struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
}

// GroupMessage is a single message in a group DM transcript.
type GroupMessage struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func generateGroupID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "gd_" + hex.EncodeToString(b)
}

func generateGroupMessageID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "gm_" + hex.EncodeToString(b)
}

// groupdmsDir returns the base directory for group DM data.
func groupdmsDir() string {
	return filepath.Join(agentsDir(), "groupdms")
}

// groupDir returns the directory for a specific group.
func groupDir(groupID string) string {
	return filepath.Join(groupdmsDir(), groupID)
}

// appendGroupMessage appends a message to a group's JSONL transcript.
func appendGroupMessage(groupID string, msg *GroupMessage) error {
	dir := groupDir(groupID)
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

// loadGroupMessages reads messages from a group's JSONL transcript with pagination.
func loadGroupMessages(groupID string, limit int, before string) ([]*GroupMessage, bool, error) {
	path := filepath.Join(groupDir(groupID), messagesFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	var all []*GroupMessage
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var msg GroupMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		all = append(all, &msg)
	}
	if err := scanner.Err(); err != nil {
		return all, false, err
	}

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

func newGroupMessage(agentID, agentName, content string) *GroupMessage {
	return &GroupMessage{
		ID:        generateGroupMessageID(),
		AgentID:   agentID,
		AgentName: agentName,
		Content:   content,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}
