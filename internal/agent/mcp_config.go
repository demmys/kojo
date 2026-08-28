package agent

import (
	"encoding/json"
	"fmt"
)

// mcpServerEntry represents one MCP server configuration.
// For HTTP transport, only URL (and optionally Type) are set.
// Headers are required when the MCP target sits behind kojo's auth
// listener — every /api/v1/* request needs the per-agent token.
// For stdio transport (extension-contributed servers) Command/Args/Env
// are set instead and URL stays empty. The two shapes are mutually
// exclusive; nothing constructs both halves at once.
type mcpServerEntry struct {
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"` // "http" for Codex
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// isStdio reports whether the entry describes a locally spawned server
// rather than an HTTP endpoint. The two transports need different
// backend flags, so every consumer branches on this.
func (e mcpServerEntry) isStdio() bool { return e.Command != "" }

// ExternalMCPServer is a stdio MCP server contributed by an installed
// extension package. Declared here rather than imported from
// internal/extpkg so the backend layer keeps no dependency on the
// package registry — the server wires the two together with
// SetExternalMCPLookup.
type ExternalMCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// externalMCPLookup, when set, returns the MCP servers contributed to
// an agent by enabled extension packages bound to it.
var externalMCPLookup func(agentID string) []ExternalMCPServer

// SetExternalMCPLookup wires the extension MCP lookup. May be nil.
func SetExternalMCPLookup(fn func(string) []ExternalMCPServer) { externalMCPLookup = fn }

// BuildMCPServers returns the set of MCP servers that should be available
// to the given agent. apiBase is the kojo server URL (e.g. "http://127.0.0.1:8080").
//
// When the agent has its own kojo auth token (i.e. agentTokenLookup is
// wired up by the server) the per-agent /mcp endpoint requires the
// X-Kojo-Token header. Without it the call lands as a Guest principal
// and is denied by the auth middleware.
func BuildMCPServers(agentID, apiBase string, hasSlackBot bool) map[string]mcpServerEntry {
	// No API base means kojo has not finished booting its listener.
	// Extension stdio servers are held back too: they are handed
	// KOJO_API_BASE and would come up unable to call back.
	if apiBase == "" {
		return nil
	}

	servers := make(map[string]mcpServerEntry)

	if hasSlackBot {
		entry := mcpServerEntry{
			URL:  fmt.Sprintf("%s/api/v1/agents/%s/mcp", apiBase, agentID),
			Type: "http",
		}
		if agentTokenLookup != nil {
			if tok, ok := agentTokenLookup(agentID); ok && tok != "" {
				entry.Headers = map[string]string{"X-Kojo-Token": tok}
			}
		}
		servers["slack"] = entry
	}

	// Extension-contributed stdio servers. The names are already
	// namespaced by package ID (extpkg.qualifyMCPName), so a package
	// cannot shadow "slack" or another package's server; the explicit
	// skip below keeps that true even if the namespacing ever changes.
	if externalMCPLookup != nil {
		for _, srv := range externalMCPLookup(agentID) {
			if srv.Name == "" || srv.Command == "" {
				continue
			}
			if _, taken := servers[srv.Name]; taken {
				continue
			}
			servers[srv.Name] = mcpServerEntry{
				Command: srv.Command,
				Args:    srv.Args,
				Env:     srv.Env,
				Type:    "stdio",
			}
		}
	}

	return servers
}

// mcpConfigJSON returns inline JSON for Claude's --mcp-config flag.
// Claude Code uses {"mcpServers": {...}} format and expects mcpServers to be
// an object (never null), so a nil input map is normalized to an empty map.
func mcpConfigJSON(servers map[string]mcpServerEntry) (string, error) {
	if servers == nil {
		servers = map[string]mcpServerEntry{}
	}
	cfg := struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}{MCPServers: servers}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
