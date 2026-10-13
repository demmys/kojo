package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestIsCodexAppsEnabledDefaults(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		a    *Agent
		want bool
	}{
		{"nil agent", nil, false},
		{"codex default on", &Agent{Tool: ToolCodex}, true},
		{"custom-codex default off", &Agent{Tool: ToolCustomCodex}, false},
		{"claude ignores default", &Agent{Tool: ToolClaude}, false},
		{"codex explicit off", &Agent{Tool: ToolCodex, CodexApps: &off}, false},
		{"custom-codex explicit on", &Agent{Tool: ToolCustomCodex, CodexApps: &on}, true},
	}
	for _, tc := range cases {
		if got := tc.a.IsCodexAppsEnabled(); got != tc.want {
			t.Errorf("%s: IsCodexAppsEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCodexAppsOverrides(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		a    *Agent
		want []string
	}{
		{"codex default leaves CLI default", &Agent{Tool: ToolCodex}, nil},
		{"codex explicit on", &Agent{Tool: ToolCodex, CodexApps: &on}, []string{"-c", "features.apps=true"}},
		{"codex explicit off", &Agent{Tool: ToolCodex, CodexApps: &off}, []string{"-c", "features.apps=false"}},
		{"custom-codex default off", &Agent{Tool: ToolCustomCodex}, []string{"-c", "features.apps=false"}},
		{"custom-codex explicit on", &Agent{Tool: ToolCustomCodex, CodexApps: &on}, []string{"-c", "features.apps=true"}},
	}
	for _, tc := range cases {
		if got := codexAppsOverrides(tc.a); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: codexAppsOverrides() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCodexAppsUpdateIsHolderOnly(t *testing.T) {
	off := false
	if (&AgentUpdateConfig{CodexApps: &off}).HubLocalOnly() {
		t.Fatal("codexApps PATCH must be holder-only (changes app-server launch flags)")
	}
	if IsHubLocalSafePatchKey("codexapps") {
		t.Fatal("codexapps must not be hub-local-safe")
	}
}

func TestCodexAppsPersistsInSettingsJSON(t *testing.T) {
	off := false
	b, err := json.Marshal(&Agent{ID: "ag_x", Tool: ToolCodex, CodexApps: &off})
	if err != nil {
		t.Fatal(err)
	}
	var back Agent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.CodexApps == nil || *back.CodexApps {
		t.Fatalf("codexApps round-trip lost: %s", b)
	}
	b, _ = json.Marshal(&Agent{ID: "ag_y", Tool: ToolCodex})
	if string(b) != "" && json.Valid(b) {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["codexApps"]; ok {
			t.Fatalf("nil codexApps must be omitted so the per-tool default survives: %s", b)
		}
	}
}
