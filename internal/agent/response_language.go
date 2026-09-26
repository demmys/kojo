package agent

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ResponseLanguageSettingKey is the global settings key (CredentialStore
// settings table) holding the agent response language. "" means auto.
const ResponseLanguageSettingKey = "response_language"

// ResponseLanguageMaxRunes caps the free-text language value. A language
// name ("日本語", "Brazilian Portuguese", "関西弁の日本語") fits easily; the
// cap keeps the operator-set value from turning into an arbitrary prompt.
const ResponseLanguageMaxRunes = 64

// NormalizeResponseLanguage trims v and reports whether it is an accepted
// setting value: "" (auto) or a single-line free-text language name of at
// most ResponseLanguageMaxRunes runes with no control characters.
func NormalizeResponseLanguage(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > ResponseLanguageMaxRunes {
		return "", false
	}
	for _, r := range v {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", false
		}
	}
	return v, true
}

// responseLanguageDirective returns the system-prompt bullet(s) telling the
// agent which language to reply in. Output is deterministic for a given
// value (prompt cache stability). Invalid values fall back to auto.
func responseLanguageDirective(lang string) string {
	var sb strings.Builder
	if name, ok := NormalizeResponseLanguage(lang); ok && name != "" {
		// Quoted so the value reads as a language name, not as an
		// instruction sentence.
		sb.WriteString("- Reply in this language by default: " + strconv.Quote(name) + ". If the user clearly writes in a different language, reply in that language instead.\n")
	} else {
		sb.WriteString("- Reply in the language the user is writing in.\n")
	}
	sb.WriteString("  - This also applies to your narration between tool calls and to final reports after long autonomous work. Do not drift into English just because tool output, code, or logs are in English. Code, identifiers, commands, and quoted output stay as-is.\n")
	return sb.String()
}
