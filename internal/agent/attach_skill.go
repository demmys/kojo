package agent

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

// attachStagingSubpath is the path (relative to agentDir) where an agent
// writes files it wants Kojo to attach to a WebUI response. The scanner and
// the channel-aware system-prompt contract share this constant.
const attachStagingSubpath = ".kojo/attach"

const attachSkillDirName = "kojo-attach"

// attachSkillLocks serializes legacy cleanup against concurrent prepareChat
// calls for the same agent. Entries intentionally live for the process
// lifetime so the mutex identity remains stable.
var attachSkillLocks keyedMutex

// RemoveLegacyAttachSkills removes kojo-attach project skills installed by
// older Kojo versions. Project skill files are visible to every session in an
// agent directory, which makes them impossible to filter safely per delivery
// transport: removing one for a Slack turn could race a concurrent WebUI turn.
//
// The attachment contract now comes exclusively from the channel-aware system
// prompt. Remove both backend layouts so switching tools cannot resurrect a
// stale copy. Sibling skills and the surrounding project configuration remain
// untouched.
func RemoveLegacyAttachSkills(agentID string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	unlock := attachSkillLocks.Lock(agentID)
	defer unlock()

	for _, root := range []string{".claude", ".codex"} {
		skillDir := filepath.Join(agentDir(agentID), root, "skills", attachSkillDirName)
		if err := os.RemoveAll(skillDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove legacy attach skill",
				"agent", agentID, "path", skillDir, "err", err)
		}
	}
}
