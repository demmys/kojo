package agent

import (
	"strings"
	"testing"
)

func TestResponseLanguageDirective(t *testing.T) {
	auto := responseLanguageDirective("")
	if !strings.Contains(auto, "Reply in the language the user is writing in.") {
		t.Errorf("auto directive missing: %q", auto)
	}
	ja := responseLanguageDirective("ja")
	if !strings.Contains(ja, "Reply in Japanese by default.") ||
		!strings.Contains(ja, "reply in that language instead") {
		t.Errorf("ja directive missing: %q", ja)
	}
	if got := responseLanguageDirective("xx"); got != auto {
		t.Errorf("unknown value should fall back to auto, got %q", got)
	}
	for _, d := range []string{auto, ja} {
		if !strings.Contains(d, "narration between tool calls") ||
			!strings.Contains(d, "Do not drift into English") {
			t.Errorf("directive missing tool-loop clause: %q", d)
		}
	}
	if responseLanguageDirective("ja") != ja {
		t.Error("directive must be deterministic")
	}
}

func TestValidResponseLanguage(t *testing.T) {
	for _, v := range []string{"", "ja", "en", "zh", "ko"} {
		if !ValidResponseLanguage(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"xx", "JA", "japanese", " ja"} {
		if ValidResponseLanguage(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
}

func TestBuildSystemPromptIncludesResponseLanguage(t *testing.T) {
	a := &Agent{ID: "ag_resp_lang", Tool: "claude"}
	p := buildSystemPrompt(a, newQuietLogger(), "", nil, false, "ja")
	d := responseLanguageDirective("ja")
	idx := strings.Index(p, d)
	if idx < 0 {
		t.Fatalf("prompt missing ja directive")
	}
	if idx > strings.Index(p, "Speak naturally") {
		t.Errorf("directive should appear early in the Instructions block")
	}
	if !strings.Contains(buildSystemPrompt(a, newQuietLogger(), "", nil, false, ""), responseLanguageDirective("")) {
		t.Errorf("prompt missing auto directive")
	}
}
