package extpkg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRejectsBadLocales(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"bad tag", `{"tag":"zh_Hans","name":"x","file":"a.json"}`, "invalid tag"},
		{"builtin", `{"tag":"ja","name":"x","file":"a.json"}`, "built into kojo"},
		{"no name", `{"tag":"ko","name":"  ","file":"a.json"}`, "name is required"},
		{"escaping file", `{"tag":"ko","name":"x","file":"../a.json"}`, "escapes the package root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"p","name":"P","version":"1.0.0","contributes":{"locales":[` + tc.json + `]}}`
			_, err := ParseManifest([]byte(body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestManifestRejectsDuplicateLocaleTags(t *testing.T) {
	body := `{"id":"p","name":"P","version":"1.0.0","contributes":{"locales":[
		{"tag":"ko","name":"한국어","file":"a.json"},
		{"tag":"ko","name":"Korean","file":"b.json"}]}}`
	if _, err := ParseManifest([]byte(body)); err == nil ||
		!strings.Contains(err.Error(), "duplicate tag") {
		t.Fatalf("err = %v, want a duplicate-tag error", err)
	}
}

// readLocaleFile keeps the strings and drops everything else rather than
// failing: the UI renders every value as text, so a nested object would
// show up as "[object Object]" while the rest of the file is fine.
func TestReadLocaleFileKeepsOnlyStrings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loc.json")
	if err := os.WriteFile(path, []byte(`{"a":"x","b":3,"c":{"d":"y"},"e":null,"f":"z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readLocaleFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "x", "f": "z"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestReadLocaleFileRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	big := map[string]string{"k": strings.Repeat("x", localeFileMax+1)}
	data, _ := json.Marshal(big)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLocaleFile(path); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a size-limit error", err)
	}
}

const localeManifest = `{"id":"lang","name":"Lang","version":"1.0.0",
  "contributes":{"locales":[
    {"tag":"zh-Hans","name":"简体中文","file":"locales/zh-Hans.json"},
    {"tag":"zh-Hant","name":"繁體中文","file":"locales/zh-Hant.json"}]}}`

func localeRepo(t *testing.T) string {
	t.Helper()
	return newRepo(t, map[string]string{
		ManifestFilename:       localeManifest,
		"locales/zh-Hans.json": `{"common.cancel":"取消"}`,
		"locales/zh-Hant.json": `{"common.cancel":"取消"}`,
	})
}

func TestLocalesFollowEnablement(t *testing.T) {
	m := newManager(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: localeRepo(t), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got := m.Locales()
	if len(got) != 2 || got[0].Tag != "zh-Hans" || got[0].Name != "简体中文" {
		t.Fatalf("locales = %+v", got)
	}
	msgs, err := m.LocaleMessages("zh-Hant")
	if err != nil {
		t.Fatal(err)
	}
	if msgs["common.cancel"] != "取消" {
		t.Fatalf("messages = %v", msgs)
	}
	// Disabling the package takes the languages with it: the picker
	// must not offer a catalogue the operator turned off.
	if _, err := m.SetEnabled("lang", false); err != nil {
		t.Fatal(err)
	}
	if got := m.Locales(); len(got) != 0 {
		t.Fatalf("locales after disable = %+v", got)
	}
	if _, err := m.LocaleMessages("zh-Hant"); err == nil {
		t.Fatal("LocaleMessages succeeded for a disabled package")
	}
}

func TestLocaleMessagesRejectsUnknownTag(t *testing.T) {
	m := newManager(t)
	if _, err := m.LocaleMessages("../../etc/passwd"); err == nil ||
		!strings.Contains(err.Error(), "invalid locale tag") {
		t.Fatalf("err = %v, want an invalid-tag error", err)
	}
}

func TestInstallRejectsUnparsableLocaleFile(t *testing.T) {
	repo := newRepo(t, map[string]string{
		ManifestFilename: `{"id":"lang","name":"Lang","version":"1.0.0","contributes":{"locales":[{"tag":"ko","name":"한국어","file":"loc.json"}]}}`,
		"loc.json":       "not json",
	})
	m := newManager(t)
	if _, err := m.Install(context.Background(), InstallRequest{URL: repo}); err == nil ||
		!strings.Contains(err.Error(), "usable message catalogue") {
		t.Fatalf("err = %v, want a catalogue error", err)
	}
}
