package extpkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Per-agent contribution resolution.
//
// SkillsForAgent (manager.go) and MCPServersForAgent below answer the
// same question for two different backends: what does the extension
// registry add to THIS agent's next turn? Both apply the same gate —
// the package is enabled AND the operator bound it to this agent —
// so disabling either level takes effect on the next chat without a
// restart.

// MCPContribution is one stdio MCP server an extension contributes to
// an agent, already resolved into something exec-able.
type MCPContribution struct {
	ExtensionID string
	// Name is namespaced (see qualifyMCPName) so two packages that
	// both contribute a server called "api" do not collide in the
	// backend's server map.
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// qualifyMCPName namespaces a contributed server name with its package
// ID. Both halves are restricted to [A-Za-z0-9_-] by manifest
// validation, so the joined name stays safe to use as a Codex config
// key and a JSON object key.
func qualifyMCPName(extensionID, name string) string {
	return extensionID + "_" + name
}

// MCPServersForAgent lists the MCP servers contributed to agentID by
// every enabled package bound to it, in a stable order.
func (m *Manager) MCPServersForAgent(agentID string) []MCPContribution {
	if agentID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MCPContribution
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		if !row.Enabled {
			continue
		}
		if b, ok := row.Agents[agentID]; !ok || !b.Enabled {
			continue
		}
		for _, srv := range row.Manifest.Contributes.MCPServers {
			out = append(out, MCPContribution{
				ExtensionID: id,
				Name:        qualifyMCPName(id, srv.Name),
				Command:     resolveCommand(pkgPath(m.root, id), srv.Command),
				Args:        append([]string(nil), srv.Args...),
				Env:         m.mcpEnvLocked(row, agentID, srv.Env),
			})
		}
	}
	return out
}

// resolveCommand turns a manifest command into something exec.Command
// can run: a path inside the checkout becomes absolute, a bare name is
// left alone for PATH lookup. Manifest validation already rejected
// absolute paths and ".." escapes.
func resolveCommand(pkgDir, cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.ContainsAny(cmd, `/\`) {
		return filepath.Join(pkgDir, filepath.FromSlash(cmd))
	}
	return cmd
}

// mcpEnvLocked builds the environment for a contributed MCP server. It
// mirrors the supervised-service contract (same KOJO_EXT_* variables,
// same reserved-name rule) so a package can share one codebase between
// its service and its MCP server. Caller holds m.mu.
func (m *Manager) mcpEnvLocked(row *Installed, agentID string, extra map[string]string) map[string]string {
	cfg := row.Config
	if b, ok := row.Agents[agentID]; ok && b.Config != nil {
		cfg = b.Config
	}
	cfgJSON := "{}"
	if len(cfg) > 0 {
		if data, err := json.Marshal(cfg); err == nil {
			cfgJSON = string(data)
		}
	}
	// The server owns the data directory, not the package: an MCP
	// server is spawned by the backend CLI, which would otherwise have
	// to create it itself on first write. Best effort — a package that
	// stores nothing must still start.
	dataDir := m.DataDir(row.ID, agentID)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		m.logger.Warn("extension data dir unavailable",
			"extension", row.ID, "path", dataDir, "err", err)
	}
	tokenFile := filepath.Join(dataDir, tokenFilename)
	if err := os.WriteFile(tokenFile, []byte(row.Token), 0o600); err != nil {
		m.logger.Warn("write extension token file failed",
			"extension", row.ID, "path", tokenFile, "err", err)
	}
	env := map[string]string{
		"KOJO_API_BASE":     m.apiBase,
		"KOJO_EXT_ID":       row.ID,
		"KOJO_EXT_VERSION":  row.Manifest.Version,
		"KOJO_EXT_DIR":      pkgPath(m.root, row.ID),
		"KOJO_EXT_DATA_DIR": dataDir,
		"KOJO_EXT_AGENT_ID": agentID,
		"KOJO_EXT_CONFIG":   cfgJSON,
		// The token goes in a file, not in this map. Unlike a
		// supervised service — which kojo spawns itself — an MCP
		// server is spawned by the backend CLI, so its environment
		// travels there as command-line config (Codex `-c`, Claude
		// --mcp-config). Anything in it is visible to every process
		// on the machine in `ps`.
		"KOJO_EXT_TOKEN_FILE": tokenFile,
		// A stdio server inherits the CLI's environment, which
		// carries the agent's own KOJO_AGENT_TOKEN — full agent
		// authority, far past the scopes the operator acknowledged
		// for this package. An explicitly empty value overrides the
		// inherited one in both backends.
		"KOJO_AGENT_TOKEN": "",
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Shape first, then the reserved check: a "name" containing
		// "=" would pass the comparison and still land as a different,
		// possibly reserved, variable in the child.
		if !envNameRe.MatchString(k) {
			m.logger.Warn("extension mcp env var ignored (invalid name)",
				"extension", row.ID, "name", k)
			continue
		}
		// Reserved names stay reserved: a package must not be able to
		// redirect its own token or config. The whole KOJO_ prefix is
		// off limits, not just the names set above, so a variable
		// this version does not use yet cannot be pre-seeded either.
		if _, taken := env[k]; taken || strings.HasPrefix(k, "KOJO_") {
			m.logger.Warn("extension mcp env var ignored (reserved)",
				"extension", row.ID, "name", k)
			continue
		}
		env[k] = extra[k]
	}
	return env
}
