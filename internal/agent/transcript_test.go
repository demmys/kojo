package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMessagesPaginated(t *testing.T) {
	// Create a temp agents dir
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	agentID := "ag_test_pagination"
	dir := filepath.Join(tmpDir, ".config", "kojo", "agents", agentID)
	os.MkdirAll(dir, 0o755)

	// Write test messages
	for i, content := range []string{"msg1", "msg2", "msg3", "msg4", "msg5"} {
		msg := &Message{
			ID:        "m_" + string(rune('a'+i)),
			Role:      "user",
			Content:   content,
			Timestamp: "2024-01-01T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := appendMessage(agentID, msg); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("load all", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 5 {
			t.Errorf("expected 5 messages, got %d", len(msgs))
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})

	t.Run("load with limit", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 3, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Errorf("expected 3 messages, got %d", len(msgs))
		}
		if !hasMore {
			t.Error("expected hasMore=true")
		}
		// Should be the last 3 messages
		if msgs[0].Content != "msg3" {
			t.Errorf("expected msg3, got %s", msgs[0].Content)
		}
	})

	t.Run("load with before cursor", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated(agentID, 2, "m_c")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Errorf("expected 2 messages, got %d", len(msgs))
		}
		if msgs[0].Content != "msg1" {
			t.Errorf("expected msg1, got %s", msgs[0].Content)
		}
		if msgs[1].Content != "msg2" {
			t.Errorf("expected msg2, got %s", msgs[1].Content)
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})

	t.Run("nonexistent agent", func(t *testing.T) {
		msgs, hasMore, err := loadMessagesPaginated("ag_nonexistent", 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if msgs != nil {
			t.Errorf("expected nil, got %v", msgs)
		}
		if hasMore {
			t.Error("expected hasMore=false")
		}
	})
}
