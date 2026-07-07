package agent

import "testing"

func TestValidateTTSProvider(t *testing.T) {
	cases := []struct {
		name string
		c    *TTSConfig
		ok   bool
	}{
		{"nil", nil, true},
		{"empty provider gemini defaults", &TTSConfig{Voice: "Kore"}, true},
		{"gemini explicit valid", &TTSConfig{Provider: "gemini", Voice: "Kore"}, true},
		{"gemini invalid voice", &TTSConfig{Provider: "gemini", Voice: "eve"}, false},
		{"gemini invalid model", &TTSConfig{Provider: "gemini", Model: "bogus"}, false},
		{"grok builtin voice", &TTSConfig{Provider: "grok", Voice: "eve"}, true},
		{"grok voice case-insensitive", &TTSConfig{Provider: "grok", Voice: "Eve"}, true},
		{"grok custom voice", &TTSConfig{Provider: "grok", Voice: "my-custom_1"}, true},
		{"grok ignores model", &TTSConfig{Provider: "grok", Model: "anything", Voice: "eve"}, true},
		{"grok bad voice", &TTSConfig{Provider: "grok", Voice: "bad voice!"}, false},
		{"unknown provider", &TTSConfig{Provider: "azure"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTTS(tc.c)
			if tc.ok && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}
