package agent

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// TOML value encoding for Codex `-c key=value` overrides.
//
// Codex parses the right-hand side of `-c` as TOML and falls back to
// treating it as a literal string when that fails. Anything kojo passes
// through therefore has to be valid TOML, and anything that originates
// outside kojo (an extension manifest's args and env) has to be quoted
// rather than interpolated raw — otherwise a value containing a quote
// would let the package write config keys it was not given.

// tomlString encodes s as a TOML basic string.
//
// strconv.Quote is deliberately NOT used: its escape set is Go's, and
// \a, \v and \x41 are all things Go emits and TOML has no syntax for.
// A value carrying one — a bell character in an argument, a stray
// control byte in an env var — would produce config Codex cannot parse,
// taking the agent's whole invocation down with it. TOML's basic-string
// escapes are the short list below plus \uXXXX for everything else.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\u%04X`, r)
			case r == utf8.RuneError:
				// Invalid UTF-8 in the source. TOML is UTF-8 only, so
				// there is nothing to encode it as; the replacement
				// character is the honest substitution.
				b.WriteString(`\uFFFD`)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlStringArray encodes a string slice as a TOML array.
func tomlStringArray(vals []string) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, tomlString(v))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// tomlStringTable encodes a string map as a TOML inline table. Keys are
// emitted in sorted order so the resulting argv is deterministic, and
// quoted so a key with a dot cannot split into a nested path.
func tomlStringTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, tomlString(k)+" = "+tomlString(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
