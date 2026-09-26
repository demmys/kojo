package agent

import "strings"

// ResponseLanguageSettingKey is the global settings key (CredentialStore
// settings table) holding the agent response language. "" means auto.
const ResponseLanguageSettingKey = "response_language"

// responseLanguageNames maps the allowed setting values to the English
// display name used in the system prompt. Keep in sync with the Web UI
// select options (GlobalSettings).
var responseLanguageNames = map[string]string{
	"ja": "Japanese",
	"en": "English",
	"zh": "Chinese",
	"ko": "Korean",
}

// ValidResponseLanguage reports whether v is an accepted setting value
// ("" = auto, or one of responseLanguageNames).
func ValidResponseLanguage(v string) bool {
	if v == "" {
		return true
	}
	_, ok := responseLanguageNames[v]
	return ok
}

// responseLanguageDirective returns the system-prompt bullet(s) telling the
// agent which language to reply in. Output is deterministic for a given
// value (prompt cache stability). Unknown values fall back to auto.
func responseLanguageDirective(lang string) string {
	var sb strings.Builder
	if name, ok := responseLanguageNames[strings.TrimSpace(lang)]; ok {
		sb.WriteString("- Reply in " + name + " by default. If the user clearly writes in a different language, reply in that language instead.\n")
	} else {
		sb.WriteString("- Reply in the language the user is writing in.\n")
	}
	sb.WriteString("  - This also applies to your narration between tool calls and to final reports after long autonomous work. Do not drift into English just because tool output, code, or logs are in English. Code, identifiers, commands, and quoted output stay as-is.\n")
	return sb.String()
}
