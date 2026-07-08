package agent

import "testing"

// TestNormalizeAgent_GrokBuildMigration verifies that a persisted agent still
// carrying the retired "grok-build" model id is rewritten to "grok-4.5" and
// that an effort the new model no longer accepts is clamped to "high".
func TestNormalizeAgent_GrokBuildMigration(t *testing.T) {
	st := newAgentStore(t)

	cases := []struct {
		name       string
		model      string
		effort     string
		wantModel  string
		wantEffort string
	}{
		{"grok-build xhigh → grok-4.5 high", "grok-build", "xhigh", "grok-4.5", "high"},
		{"grok-build max → grok-4.5 high", "grok-build", "max", "grok-4.5", "high"},
		{"grok-build medium kept", "grok-build", "medium", "grok-4.5", "medium"},
		{"grok-composer xhigh clamped", "grok-composer-2.5-fast", "xhigh", "grok-composer-2.5-fast", "high"},
		{"grok-4.5 high kept", "grok-4.5", "high", "grok-4.5", "high"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{ID: "a", Model: tc.model, Effort: tc.effort}
			st.normalizeAgent(a)
			if a.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", a.Model, tc.wantModel)
			}
			if a.Effort != tc.wantEffort {
				t.Errorf("effort = %q, want %q", a.Effort, tc.wantEffort)
			}
		})
	}
}
