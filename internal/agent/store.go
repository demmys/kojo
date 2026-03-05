package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const agentsFile = "agents.json"

// store persists agent metadata to disk using atomic rename.
type store struct {
	mu     sync.Mutex
	dir    string // base directory (~/.config/kojo/agents)
	logger *slog.Logger
}

func newStore(logger *slog.Logger) *store {
	return &store{
		dir:    agentsDir(),
		logger: logger,
	}
}

// Save writes all agents metadata to agents.json using atomic rename.
func (st *store) Save(agents []*Agent) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		st.logger.Warn("failed to create agents dir", "err", err)
		return
	}

	data, err := json.MarshalIndent(agents, "", "  ")
	if err != nil {
		st.logger.Warn("failed to marshal agents", "err", err)
		return
	}

	path := filepath.Join(st.dir, agentsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		st.logger.Warn("failed to write tmp agents file", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		st.logger.Warn("failed to rename agents file", "err", err)
		os.Remove(tmp)
	}
}

// Load reads persisted agents from agents.json.
func (st *store) Load() ([]*Agent, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	path := filepath.Join(st.dir, agentsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var agents []*Agent
	if err := json.Unmarshal(data, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// agentsDir returns the base directory for all agent data.
func agentsDir() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "kojo", "agents")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kojo", "agents")
	}
	return filepath.Join(home, ".config", "kojo", "agents")
}

// agentDir returns the data directory for a specific agent.
func agentDir(id string) string {
	return filepath.Join(agentsDir(), id)
}
