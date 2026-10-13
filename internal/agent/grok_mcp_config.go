package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The grok CLI has no per-invocation MCP flag. It reads MCP servers
// from ~/.grok/config.toml (shared by every agent and by the operator's
// own grok) or from <cwd>/.grok/config.toml, so kojo keeps a block of
// its own in the agent directory's file and rewrites it before each
// turn. Anything else in that file is left alone.
//
// grok only starts project-scoped servers in a folder it trusts; a
// headless run grants that with --trust (see buildGrokArgs).
const (
	grokMCPBlockStart = "# >>> kojo managed MCP servers (rewritten every turn; edits here are lost)"
	grokMCPBlockEnd   = "# <<< kojo managed MCP servers"
)

// grokMCPConfigMu guards the config files and grokMCPHolds.
var grokMCPConfigMu sync.Mutex

// grokMCPHold records what a config file says while grok processes
// that were started against it are still running. One agent can run
// several grok turns at once (its own chat plus keyed external
// sessions); they share the file, so a turn that needs it to say
// something else waits for the others to finish.
type grokMCPHold struct {
	content string
	users   int
	freed   chan struct{} // closed when users drops to zero
}

var grokMCPHolds = map[string]*grokMCPHold{}

func grokMCPConfigPath(agentDirPath string) string {
	return filepath.Join(agentDirPath, ".grok", "config.toml")
}

var (
	grokTOMLTable   = regexp.MustCompile(`^\[\[?\s*([^\]]*?)\s*\]\]?\s*(?:#.*)?$`)
	grokTOMLKeyPart = `(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))`
	grokMCPRoot     = `(?:mcp_servers|"mcp_servers"|'mcp_servers')`
	grokMCPTable    = regexp.MustCompile(`^` + grokMCPRoot + `$`)
	grokMCPPath     = regexp.MustCompile(`^` + grokMCPRoot + `\s*\.\s*` + grokTOMLKeyPart)
	grokMCPInline   = regexp.MustCompile(`^` + grokMCPRoot + `\s*=`)
	grokTOMLKey     = regexp.MustCompile(`^` + grokTOMLKeyPart + `\s*[.=]`)
)

// grokOperatorMCPNames lists the servers the operator's own part of the
// file defines, in any of the forms TOML allows: a [mcp_servers.x]
// table, a mcp_servers.x dotted key, or a key of [mcp_servers]. It
// reads line by line rather than parsing, and errs towards reporting a
// name: a server kojo leaves out is one tool missing, a server declared
// twice is a file grok cannot load.
//
// closed reports a `mcp_servers = { ... }` inline table, which TOML
// does not let anything else add to.
func grokOperatorMCPNames(own string) (names map[string]bool, closed bool) {
	names = map[string]bool{}
	add := func(m []string) {
		if m != nil {
			names[m[1]+m[2]+m[3]] = true
		}
	}
	table := ""
	for _, line := range strings.Split(own, "\n") {
		line = strings.TrimSpace(line)
		if m := grokTOMLTable.FindStringSubmatch(line); m != nil {
			table = m[1]
			add(grokMCPPath.FindStringSubmatch(table))
			continue
		}
		switch {
		case table == "":
			add(grokMCPPath.FindStringSubmatch(line))
			closed = closed || grokMCPInline.MatchString(line)
		case grokMCPTable.MatchString(table):
			add(grokTOMLKey.FindStringSubmatch(line))
		}
	}
	return names, closed
}

// renderGrokMCPBlock renders the managed block, or "" when there is
// nothing to declare. Servers named in taken are left to the operator's
// own definition: a second table of the same name is a TOML error that
// would cost the agent every server in the file.
func renderGrokMCPBlock(servers map[string]mcpServerEntry, taken map[string]bool) string {
	names := make([]string, 0, len(servers))
	for name, e := range servers {
		if name == "" || taken[name] || (e.Command == "" && e.URL == "") {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString(grokMCPBlockStart + "\n")
	for _, name := range names {
		e := servers[name]
		table := "mcp_servers." + tomlString(name)
		fmt.Fprintf(&sb, "\n[%s]\n", table)
		if e.isStdio() {
			fmt.Fprintf(&sb, "command = %s\n", tomlString(e.Command))
			fmt.Fprintf(&sb, "args = %s\n", tomlStringArray(e.Args))
			if len(e.Env) > 0 {
				fmt.Fprintf(&sb, "env = %s\n", tomlStringTable(e.Env))
			}
		} else {
			fmt.Fprintf(&sb, "url = %s\n", tomlString(e.URL))
			if len(e.Headers) > 0 {
				fmt.Fprintf(&sb, "headers = %s\n", tomlStringTable(e.Headers))
			}
		}
		sb.WriteString("enabled = true\n")
	}
	sb.WriteString("\n" + grokMCPBlockEnd + "\n")
	return sb.String()
}

// stripGrokMCPBlock removes every managed block from content (kojo
// writes one; a copied file can carry more). Markers count only as
// whole lines. A start marker with no end marker is an error: kojo
// writes the file atomically, so someone edited it, and there is no
// telling which of the lines after it are still kojo's to remove.
func stripGrokMCPBlock(content string) (string, error) {
	var out []string
	inBlock := false
	for _, line := range strings.SplitAfter(content, "\n") {
		switch marker := strings.TrimRight(line, "\r\n"); {
		case inBlock:
			inBlock = marker != grokMCPBlockEnd
		case marker == grokMCPBlockStart:
			inBlock = true
			// The blank line holdGrokMCPConfig puts in front of the block.
			if n := len(out); n > 0 && strings.TrimRight(out[n-1], "\r\n") == "" {
				out = out[:n-1]
			}
		default:
			out = append(out, line)
		}
	}
	if inBlock {
		return "", fmt.Errorf("the kojo managed block has no end marker (%q); restore it or delete the block", grokMCPBlockEnd)
	}
	return strings.Join(out, ""), nil
}

// holdGrokMCPConfig makes the managed block of the agent's
// .grok/config.toml declare exactly servers and keeps it that way until
// release is called, which the caller does once its grok process has
// exited. It reports how many servers the block declares.
func holdGrokMCPConfig(ctx context.Context, agentDirPath string, servers map[string]mcpServerEntry) (declared int, release func(), err error) {
	path := grokMCPConfigPath(agentDirPath)
	for {
		grokMCPConfigMu.Lock()
		raw, existed, err := readGrokMCPConfig(path)
		if err != nil {
			grokMCPConfigMu.Unlock()
			return 0, nil, err
		}
		own, err := stripGrokMCPBlock(raw)
		if err != nil {
			grokMCPConfigMu.Unlock()
			return 0, nil, err
		}
		block := ""
		if taken, closed := grokOperatorMCPNames(own); !closed {
			block = renderGrokMCPBlock(servers, taken)
		}
		next := own
		if block != "" {
			next = block
			if trimmed := strings.TrimRight(own, " \t\r\n"); trimmed != "" {
				next = trimmed + "\n\n" + block
			}
		}

		hold := grokMCPHolds[path]
		if hold != nil && hold.content != next {
			freed := hold.freed
			grokMCPConfigMu.Unlock()
			select {
			case <-freed:
				continue
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			}
		}
		// Also under a hold of the same content: the file may have been
		// edited since, and putting back what the running turns were
		// started with takes nothing from them.
		if !existed || raw != next {
			if err := writeGrokMCPConfig(path, next); err != nil {
				grokMCPConfigMu.Unlock()
				return 0, nil, err
			}
		}
		if hold == nil {
			hold = &grokMCPHold{content: next, freed: make(chan struct{})}
			grokMCPHolds[path] = hold
		}
		hold.users++
		grokMCPConfigMu.Unlock()

		var once sync.Once
		release = func() {
			once.Do(func() {
				grokMCPConfigMu.Lock()
				defer grokMCPConfigMu.Unlock()
				if hold.users--; hold.users == 0 {
					delete(grokMCPHolds, path)
					close(hold.freed)
				}
			})
		}
		return strings.Count(block, "\nenabled = true\n"), release, nil
	}
}

func readGrokMCPConfig(path string) (content string, existed bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// writeGrokMCPConfig replaces the file, or removes it when nothing is
// left to say.
func writeGrokMCPConfig(path, content string) error {
	if strings.TrimSpace(content) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// CreateTemp makes the file 0600: an HTTP server's entry carries
	// the agent's kojo token.
	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.toml.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
