package extpkg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// scpLikeRe matches git's "user@host:path" shorthand, which is not a
// URL and therefore has to be recognised separately.
var scpLikeRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:[^\s]+$`)

// allowedSchemes are the transports an operator may install from.
// Plain "http://" and "git://" are deliberately absent: neither is
// authenticated, and a package installs an executable kojo then runs
// unsandboxed, so anyone on the path between here and the remote would
// get code execution. "file" and bare absolute paths stay because they
// are the only way to install a package that is not published yet.
var allowedSchemes = []string{"https://", "ssh://", "file://"}

// ValidateSourceURL rejects inputs git would treat as an option rather
// than a repository, and anything outside the allowed transports.
func ValidateSourceURL(raw string) error {
	u := strings.TrimSpace(raw)
	if u == "" {
		return fmt.Errorf("repository URL is required")
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("invalid repository URL %q", raw)
	}
	if strings.ContainsAny(u, "\x00\n\r") {
		return fmt.Errorf("invalid repository URL %q", raw)
	}
	for _, s := range allowedSchemes {
		if strings.HasPrefix(u, s) {
			return nil
		}
	}
	if scpLikeRe.MatchString(u) {
		return nil
	}
	if filepath.IsAbs(u) {
		return nil
	}
	return fmt.Errorf("unsupported repository URL %q: use https://, ssh://, git@host:path or an absolute path", raw)
}

// ValidateRef rejects refs that could be mistaken for git options. An
// empty ref means "the remote's default branch".
func ValidateRef(ref string) error {
	r := strings.TrimSpace(ref)
	if r == "" {
		return nil
	}
	if strings.HasPrefix(r, "-") || strings.ContainsAny(r, " \t\x00\n\r") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

// fetchInto materialises repo@ref into dst (which must not exist yet)
// as a depth-1 checkout and returns the resolved commit SHA. init +
// fetch is used instead of clone so a bare commit SHA works the same
// way a tag or branch does.
func fetchInto(ctx context.Context, dst, url, ref string) (string, error) {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	target := ref
	if strings.TrimSpace(target) == "" {
		target = "HEAD"
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
		{"fetch", "--depth", "1", "--tags", "origin", target},
		{"checkout", "-q", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, dst, args...); err != nil {
			return "", err
		}
	}
	sha, err := runGit(ctx, dst, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// runGit executes git in dir with prompts disabled so a private
// repository fails fast instead of hanging the request on a
// credential prompt.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
