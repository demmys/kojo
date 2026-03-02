//go:build windows

package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/UserExistsError/conpty"
)

// conPTY wraps a ConPTY instance as an io.ReadWriteCloser.
type conPTY struct {
	cpty *conpty.ConPty
}

func (c *conPTY) Read(p []byte) (int, error) {
	return c.cpty.Read(p)
}

func (c *conPTY) Write(p []byte) (int, error) {
	return c.cpty.Write(p)
}

func (c *conPTY) Close() error {
	return c.cpty.Close()
}

// resizeConPTY returns a resize function for the given ConPTY.
func resizeConPTY(cpty *conpty.ConPty) func(cols, rows uint16) error {
	return func(cols, rows uint16) error {
		return cpty.Resize(int(cols), int(rows))
	}
}

// startConPTY starts a process inside a ConPTY.
// Returns the io.ReadWriteCloser, the exec.Cmd, and a resize function.
func startConPTY(cmdLine string, workDir string, cols, rows uint16) (io.ReadWriteCloser, *exec.Cmd, func(cols, rows uint16) error, error) {
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 36
	}

	cpty, err := conpty.Start(cmdLine, conpty.ConPtyDimensions(int(cols), int(rows)), conpty.ConPtyWorkDir(workDir))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("conpty start: %w", err)
	}

	// Get the PID of the process spawned inside the ConPTY.
	pid := cpty.Pid()

	// Build a synthetic exec.Cmd so the rest of the codebase
	// can call cmd.Process.Kill() / cmd.Wait() uniformly.
	proc, err := findProcessByPID(pid)
	if err != nil {
		cpty.Close()
		return nil, nil, nil, fmt.Errorf("conpty find process (pid %d): %w", pid, err)
	}
	cmd := &exec.Cmd{}
	cmd.Process = proc

	rwc := &conPTY{cpty: cpty}
	return rwc, cmd, resizeConPTY(cpty), nil
}

// buildCmdLine constructs a Windows command line string from a tool path and arguments.
func buildCmdLine(toolPath string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, quoteArg(toolPath))
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg quotes a command-line argument for Windows using CommandLineToArgvW rules.
//   - Backslashes are literal unless immediately before a double-quote.
//   - N backslashes before a quote → 2N+1 backslashes + literal quote.
//   - N backslashes at end of string (before closing quote) → 2N backslashes.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '"' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range s {
		if c == '\\' {
			slashes++
			continue
		}
		if c == '"' {
			// Double backslashes before quote, then escape the quote
			for range 2 * slashes {
				b.WriteByte('\\')
			}
			b.WriteString(`\"`)
			slashes = 0
		} else {
			// Emit accumulated backslashes literally
			for range slashes {
				b.WriteByte('\\')
			}
			slashes = 0
			b.WriteRune(c)
		}
	}
	// Double trailing backslashes (they precede the closing quote)
	for range 2 * slashes {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// findProcessByPID returns an os.Process handle for the given PID.
func findProcessByPID(pid int) (*os.Process, error) {
	return os.FindProcess(pid)
}
