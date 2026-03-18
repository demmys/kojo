package agent

import (
	"os"
	"path/filepath"
	"time"
)

// GroupDM represents a group conversation between agents.
// GroupDMStyle controls the communication style for a group conversation.
// "efficient" (default): concise, token-saving, no pleasantries.
// "expressive": human-like chat with greetings and conversational filler.
type GroupDMStyle string

const (
	GroupDMStyleEfficient  GroupDMStyle = "efficient"
	GroupDMStyleExpressive GroupDMStyle = "expressive"
)

// ValidGroupDMStyles is the set of accepted style values.
var ValidGroupDMStyles = map[GroupDMStyle]bool{
	GroupDMStyleEfficient:  true,
	GroupDMStyleExpressive: true,
}

type GroupDM struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Members   []GroupMember `json:"members"`
	Cooldown  int           `json:"cooldown"` // notification cooldown in seconds (0 = use default)
	Style     GroupDMStyle  `json:"style"`    // communication style: "efficient" or "expressive"
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
	return generatePrefixedID("gd_")
}

func generateGroupMessageID() string {
	return generatePrefixedID("gm_")
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
	return jsonlAppend(filepath.Join(dir, messagesFile), msg)
}

// loadGroupMessages reads messages from a group's JSONL transcript with pagination.
func loadGroupMessages(groupID string, limit int, before string) ([]*GroupMessage, bool, error) {
	path := filepath.Join(groupDir(groupID), messagesFile)
	msgs, hasMore, err := jsonlLoadPaginated(path, limit, before, func(m *GroupMessage) string { return m.ID })
	for _, m := range msgs {
		m.Timestamp = normalizeTimestamp(m.Timestamp)
	}
	return msgs, hasMore, err
}

func newGroupMessage(agentID, agentName, content string) *GroupMessage {
	return &GroupMessage{
		ID:        generateGroupMessageID(),
		AgentID:   agentID,
		AgentName: agentName,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}
