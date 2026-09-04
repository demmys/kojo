package extpkg

import (
	"fmt"
	"strings"

	"github.com/loppo-llc/kojo/internal/selfupdate"
)

// constraint is one comparison against a parsed version, e.g. ">=0.127.0".
type constraint struct {
	op  string
	ver selfupdate.Version
}

// parseConstraints splits a space-separated constraint list. Every
// element must carry an explicit operator; a bare version is treated as
// "=" so ">=0.127.0" and "0.127.0" both parse, and the whole list is
// ANDed together.
func parseConstraints(s string) ([]constraint, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, fmt.Errorf("invalid kojoVersion %q: empty constraint", s)
	}
	out := make([]constraint, 0, len(fields))
	for _, f := range fields {
		op := "="
		rest := f
		for _, cand := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(f, cand) {
				op = cand
				rest = strings.TrimPrefix(f, cand)
				break
			}
		}
		v, err := selfupdate.ParseVersion(rest)
		if err != nil {
			return nil, fmt.Errorf("invalid kojoVersion %q: %w", s, err)
		}
		out = append(out, constraint{op: op, ver: v})
	}
	return out, nil
}

// SatisfiesKojoVersion reports whether current satisfies the manifest's
// kojoVersion constraint. An empty constraint always passes. An
// unparseable current version (a dev build stamped "dev" or a bare
// hash) also passes: refusing to install on a development binary would
// make the feature untestable, and the operator installing by URL has
// already accepted the risk.
func (m *Manifest) SatisfiesKojoVersion(current string) (bool, error) {
	if strings.TrimSpace(m.KojoVersion) == "" {
		return true, nil
	}
	cs, err := parseConstraints(m.KojoVersion)
	if err != nil {
		return false, err
	}
	cur, err := selfupdate.ParseVersion(current)
	if err != nil {
		return true, nil
	}
	for _, c := range cs {
		cmp := cur.Compare(c.ver)
		ok := false
		switch c.op {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		case "=":
			ok = cmp == 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
