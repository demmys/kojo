// Package extpkg implements kojo extension packages: third-party
// bundles installed from a git repository that contribute skills, MCP
// servers, a supervised service process, and a declarative settings
// form to a running kojo instance.
//
// kojo ships as a single Go binary with an embedded web bundle, so an
// extension can never be loaded *into* the process. A package is
// therefore a manifest plus assets on disk, and — when it needs to run
// code — an out-of-process service that talks to kojo over the normal
// HTTP API with a scoped token. This file owns the manifest schema and
// its validation only; installation lives in install.go and the
// registry in manager.go.
package extpkg

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/loppo-llc/kojo/internal/selfupdate"
)

// ManifestFilename is the fixed path, relative to the repository root,
// that every extension package must provide.
const ManifestFilename = "kojo-package.json"

// ScopeSet enumerates every capability an extension may request. The
// list is closed: an unknown scope fails validation rather than being
// ignored, so a package built against a newer kojo cannot silently run
// with fewer privileges than it expects.
var ScopeSet = map[string]string{
	"chat:send":        "エージェントにメッセージを送る",
	"chat:read":        "エージェントの会話履歴を読む",
	"events:subscribe": "イベントストリームを購読する",
	"agents:read":      "エージェントの一覧と設定を読む",
	"agents:write":     "エージェントの設定を変更する",
	"kv:own":           "自分の名前空間の KV を読み書きする",
	"files:agent":      "エージェントのファイルを読む",
	"blob:read":        "添付ファイル (blob) を読む",
}

// idRe constrains package IDs to a filesystem- and URL-safe shape. The
// ID doubles as the on-disk directory name and the credential-store
// source ID, so anything that could traverse or collide is rejected.
var idRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

// agentIDRe mirrors internal/auth's agent-ID alphabet. A binding's
// agent ID becomes a directory name under the extension data root, so
// anything that could traverse is rejected before it is stored.
var agentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validateAgentID rejects agent IDs that are unsafe as path segments.
func validateAgentID(id string) error {
	if !agentIDRe.MatchString(id) {
		return fmt.Errorf("invalid agent id %q", id)
	}
	return nil
}

// execKeyRe matches the GOOS/GOARCH keys of Service.Exec.
var execKeyRe = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)

// mcpNameRe constrains a contributed MCP server name. The name is
// interpolated into Codex's `-c mcp_servers.<name>.command=...` config
// keys and used as an object key in Claude's --mcp-config JSON, so a
// name containing a dot, quote or space would let a package write
// config it was never given.
var mcpNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// envNameRe constrains a manifest-declared environment variable name.
// The check is what makes the reserved-name comparison meaningful: a
// "name" containing "=" would be spliced into the child's environment
// as a DIFFERENT variable than the one compared against the reserved
// list, so a package could ship `"KOJO_EXT_TOKEN=x": "y"` and end up
// overriding its own token — or PATH.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnv checks the shape of a manifest-declared environment
// block. Values are arbitrary; names are not.
func validateEnv(env map[string]string, where string) error {
	for name := range env {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("%s: invalid environment variable name %q", where, name)
		}
	}
	return nil
}

// Manifest is the parsed kojo-package.json.
type Manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// KojoVersion is a space-separated constraint list, e.g.
	// ">=0.127.0" or ">=0.127.0 <1.0.0". Empty means "any".
	KojoVersion string      `json:"kojoVersion,omitempty"`
	Scopes      []string    `json:"scopes,omitempty"`
	Contributes Contributes `json:"contributes"`
}

// Contributes lists what the package adds to kojo. Every field is
// optional; an assets-only package declares just Skills.
type Contributes struct {
	// Skills are repository-relative directories, each containing a
	// SKILL.md, that get installed into the data directory of every
	// agent the extension is enabled for.
	Skills     []string    `json:"skills,omitempty"`
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
	Service    *Service    `json:"service,omitempty"`
	Settings   *Settings   `json:"settings,omitempty"`
}

// MCPServer is an stdio MCP server definition contributed to agents.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Service is a long-lived process kojo supervises on the package's
// behalf — the shape the Slack integration takes once it is extracted
// from the core.
type Service struct {
	// Scope is "global" (one process for the whole instance) or
	// "per-agent" (one process per agent the extension is enabled
	// for; the agent ID arrives as KOJO_EXT_AGENT_ID).
	Scope string `json:"scope"`
	// Exec maps "GOOS/GOARCH" to a repository-relative executable.
	// Installation fails when the running platform has no entry.
	Exec map[string]string `json:"exec"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

// Settings points at a JSON Schema the web UI renders as a form. There
// is deliberately no way for a package to ship React: the bundle is
// built ahead of time, so configuration is declarative or nothing.
type Settings struct {
	// Scope is "global" or "per-agent".
	Scope  string `json:"scope"`
	Schema string `json:"schema"`
}

// ScopeSummary pairs a requested scope with its human-readable
// description for the install-time consent dialog.
type ScopeSummary struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
}

// ScopeSummaries renders the manifest's scopes for the consent UI.
func (m *Manifest) ScopeSummaries() []ScopeSummary {
	out := make([]ScopeSummary, 0, len(m.Scopes))
	for _, s := range m.Scopes {
		out = append(out, ScopeSummary{Scope: s, Description: ScopeSet[s]})
	}
	return out
}

// ParseManifest decodes and validates a kojo-package.json body.
// Unknown fields are tolerated so a package targeting a newer kojo
// still installs on an older one as long as its constraints allow it.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestFilename, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate enforces every structural rule. It does not touch the
// filesystem: path fields are checked for shape (relative, no escape)
// but their existence is verified during installation.
func (m *Manifest) Validate() error {
	if !idRe.MatchString(m.ID) {
		return fmt.Errorf("invalid id %q: lowercase letters, digits and dashes only, 1-40 chars", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if _, err := selfupdate.ParseVersion(m.Version); err != nil {
		return fmt.Errorf("invalid version %q: want X.Y.Z", m.Version)
	}
	if m.KojoVersion != "" {
		if _, err := parseConstraints(m.KojoVersion); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, s := range m.Scopes {
		if _, ok := ScopeSet[s]; !ok {
			return fmt.Errorf("unknown scope %q", s)
		}
		if seen[s] {
			return fmt.Errorf("duplicate scope %q", s)
		}
		seen[s] = true
	}
	for _, dir := range m.Contributes.Skills {
		if err := checkRelPath(dir); err != nil {
			return fmt.Errorf("contributes.skills: %w", err)
		}
	}
	names := map[string]bool{}
	for i, srv := range m.Contributes.MCPServers {
		if !mcpNameRe.MatchString(srv.Name) {
			return fmt.Errorf("contributes.mcpServers[%d]: invalid name %q: letters, digits, underscore and dash only", i, srv.Name)
		}
		if names[srv.Name] {
			return fmt.Errorf("contributes.mcpServers: duplicate name %q", srv.Name)
		}
		names[srv.Name] = true
		cmd := strings.TrimSpace(srv.Command)
		if cmd == "" {
			return fmt.Errorf("contributes.mcpServers[%d]: command is required", i)
		}
		// A command is either a bare name resolved through PATH
		// ("npx", "python3") or a path inside the package. An
		// absolute path, or one that climbs out of the checkout, is
		// refused: a package may ship a binary, not point kojo at an
		// arbitrary place on the host.
		if strings.ContainsAny(cmd, `/\`) {
			if err := checkRelPath(cmd); err != nil {
				return fmt.Errorf("contributes.mcpServers[%d].command: %w", i, err)
			}
		}
		if err := validateEnv(srv.Env, fmt.Sprintf("contributes.mcpServers[%d].env", i)); err != nil {
			return err
		}
	}
	if svc := m.Contributes.Service; svc != nil {
		if !validScope(svc.Scope) {
			return fmt.Errorf(`contributes.service.scope must be "global" or "per-agent"`)
		}
		if len(svc.Exec) == 0 {
			return fmt.Errorf("contributes.service.exec: at least one platform entry is required")
		}
		for key, rel := range svc.Exec {
			if !execKeyRe.MatchString(key) {
				return fmt.Errorf("contributes.service.exec: invalid platform key %q, want GOOS/GOARCH", key)
			}
			if err := checkRelPath(rel); err != nil {
				return fmt.Errorf("contributes.service.exec[%s]: %w", key, err)
			}
		}
		if err := validateEnv(svc.Env, "contributes.service.env"); err != nil {
			return err
		}
	}
	if st := m.Contributes.Settings; st != nil {
		if !validScope(st.Scope) {
			return fmt.Errorf(`contributes.settings.scope must be "global" or "per-agent"`)
		}
		if err := checkRelPath(st.Schema); err != nil {
			return fmt.Errorf("contributes.settings.schema: %w", err)
		}
	}
	return nil
}

func validScope(s string) bool { return s == ScopeGlobal || s == ScopePerAgent }

// Scope values shared by Service and Settings.
const (
	ScopeGlobal   = "global"
	ScopePerAgent = "per-agent"
)

// checkRelPath rejects absolute paths, parent traversal, and Windows
// drive/backslash forms so a manifest can only ever name files inside
// its own checkout.
func checkRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.ContainsAny(p, "\\:") {
		return fmt.Errorf("invalid path %q", p)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q must be relative", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q escapes the package root", p)
	}
	return nil
}
