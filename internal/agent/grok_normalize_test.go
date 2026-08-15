package agent

import "testing"

// TestNormalizeAgent_GrokBuildMigration verifies that a persisted agent still
// carrying a retired grok model id ("grok-build", "grok-composer-2.5-fast") is
// rewritten to the current default and that an effort the new model no longer
// accepts is clamped to "high".
func TestNormalizeAgent_GrokBuildMigration(t *testing.T) {
	st := newAgentStore(t)

	cases := []struct {
		name       string
		model      string
		effort     string
		wantModel  string
		wantEffort string
	}{
		{"grok-build max → grok-4.6 high", "grok-build", "max", "grok-4.6", "high"},
		{"grok-build xhigh kept (4.6 supports it)", "grok-build", "xhigh", "grok-4.6", "xhigh"},
		{"grok-build medium kept", "grok-build", "medium", "grok-4.6", "medium"},
		{"grok-composer max clamped", "grok-composer-2.5-fast", "max", "grok-4.6", "high"},
		{"grok-4.5 xhigh clamped", "grok-4.5", "xhigh", "grok-4.5", "high"},
		{"grok-4.5 high kept", "grok-4.5", "high", "grok-4.5", "high"},
		{"grok-4.6 xhigh kept", "grok-4.6", "xhigh", "grok-4.6", "xhigh"},
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
