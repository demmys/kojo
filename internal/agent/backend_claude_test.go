package agent

import (
	"regexp"
	"testing"
)

func TestAgentIDToUUID(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	t.Run("valid UUID format", func(t *testing.T) {
		id := agentIDToUUID("ag_8cf247118ad856e8")
		if !uuidRe.MatchString(id) {
			t.Errorf("not a valid UUID format: %s", id)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		a := agentIDToUUID("ag_8cf247118ad856e8")
		b := agentIDToUUID("ag_8cf247118ad856e8")
		if a != b {
			t.Errorf("not deterministic: %s != %s", a, b)
		}
	})

	t.Run("different IDs produce different UUIDs", func(t *testing.T) {
		a := agentIDToUUID("ag_1111111111111111")
		b := agentIDToUUID("ag_2222222222222222")
		if a == b {
			t.Errorf("collision: %s == %s", a, b)
		}
	})

	t.Run("UUID v3 version and variant bits", func(t *testing.T) {
		id := agentIDToUUID("ag_8cf247118ad856e8")
		// Version 3: third group starts with '3'
		if id[14] != '3' {
			t.Errorf("version nibble not 3: got %c in %s", id[14], id)
		}
		// Variant RFC4122: fourth group starts with 8, 9, a, or b
		c := id[19]
		if c != '8' && c != '9' && c != 'a' && c != 'b' {
			t.Errorf("variant nibble not 8/9/a/b: got %c in %s", c, id)
		}
	})
}
