package peer

import (
	"strings"
	"testing"
)

func TestSanitizePeerVersion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"v0.119.1", "v0.119.1"},
		{"v0.119.1-3-g07d4e24c-dirty", "v0.119.1-3-g07d4e24c-dirty"},
		{"  v0.119.1  ", "v0.119.1"},
		{"", ""},
		{"   ", ""},
		{"v1\x1b[31m", ""},            // ANSI escape
		{"v1\n2", ""},                 // newline
		{"v1\u200b2", ""},             // zero-width space
		{"v1\u202e2", ""},             // bidi override
		{"v1 2", ""},                  // interior space (not git-describe shaped)
		{strings.Repeat("a", 65), ""}, // over cap
		{strings.Repeat("a", 64), strings.Repeat("a", 64)}, // at cap
	}
	for _, c := range cases {
		if got := SanitizePeerVersion(c.in); got != c.want {
			t.Errorf("SanitizePeerVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
