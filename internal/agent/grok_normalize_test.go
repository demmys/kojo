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
			a := &Agent{ID: "a", Tool: "grok", Model: tc.model, Effort: tc.effort}
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

func TestNormalizeAgent_RetiredGrokModelIsScopedToGrokBackend(t *testing.T) {
	st := newAgentStore(t)
	for _, tool := range []string{"custom", "llama.cpp", "claude"} {
		t.Run(tool, func(t *testing.T) {
			for _, model := range []string{"grok-composer-2.5-fast", "grok-4.6"} {
				a := &Agent{ID: "a", Tool: tool, Model: model, Effort: "max"}
				st.normalizeAgent(a)
				if a.Model != model {
					t.Fatalf("model = %q, want endpoint model %q unchanged", a.Model, model)
				}
				if a.Effort != "max" {
					t.Fatalf("effort = %q, want endpoint effort unchanged", a.Effort)
				}
			}
		})
	}
}

func TestValidToolModelEffort_CustomModelNamespaceIsIndependent(t *testing.T) {
	for _, tool := range []string{"custom", "llama.cpp"} {
		if !ValidToolModelEffort(tool, "grok-4.6", "max") {
			t.Fatalf("%s endpoint inherited built-in Grok effort constraints", tool)
		}
		for _, unsupported := range []string{"none", "minimal", "xhigh"} {
			if ValidToolModelEffort(tool, "grok-4.6", unsupported) {
				t.Fatalf("%s endpoint unexpectedly accepted effort %q", tool, unsupported)
			}
		}
	}
}

func TestNewAgent_NormalizesRetiredGrokModel(t *testing.T) {
	a, err := newAgent(AgentConfig{
		Name:   "grok agent",
		Tool:   "grok",
		Model:  "grok-composer-2.5-fast",
		Effort: "xhigh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "grok-4.6" || a.Effort != "xhigh" {
		t.Fatalf("model/effort = %q/%q, want grok-4.6/xhigh", a.Model, a.Effort)
	}
}

func TestManagerUpdate_NormalizesRetiredGrokModelImmediately(t *testing.T) {
	m := newTestManager(t)
	a := &Agent{ID: "ag_grok", Name: "grok", Tool: "grok", Model: "grok-4.5", Effort: "high"}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	if err := m.store.Upsert(a); err != nil {
		t.Fatal(err)
	}

	retired := "grok-composer-2.5-fast"
	got, err := m.Update(a.ID, AgentUpdateConfig{Model: &retired})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "grok-4.6" {
		t.Fatalf("model = %q, want grok-4.6 without reload", got.Model)
	}
}
