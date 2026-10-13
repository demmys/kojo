package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

const slackMigTestManifest = `{
  "id": "slack",
  "name": "Slack",
  "version": "0.1.0",
  "scopes": ["chat:session"],
  "contributes": { "settings": { "scope": "per-agent", "schema": "settings.schema.json" } }
}`

const slackMigTestSchema = `{
  "type": "object",
  "properties": {
    "appToken": { "type": "string", "format": "password" },
    "botToken": { "type": "string", "format": "password" },
    "threadReplies": { "type": "boolean" },
    "respondDM": { "type": "boolean" },
    "respondMention": { "type": "boolean" },
    "respondThread": { "type": "boolean" }
  }
}`

// newSlackMigTestRepo builds a git repository holding a stand-in for
// the Slack package: same ID, scope and settings keys, no executables.
func newSlackMigTestRepo(t *testing.T, manifest, schema string) string {
	t.Helper()
	dir := t.TempDir()
	writeExtTestFiles(t, dir, map[string]string{
		extpkg.ManifestFilename: manifest,
		"settings.schema.json":  schema,
	})
	runExtTestGit(t, dir, "init", "-q", "-b", "main")
	commitExtTestRepo(t, dir)
	return dir
}

// slackMigSecrets is an in-memory credential store. It serves both
// roles the real one does: the built-in bot's tokens and the package's
// secrets.
type slackMigSecrets struct {
	rows      map[string]string
	readErr   error
	deleteErr error
}

func slackMigKey(parts ...string) string { return strings.Join(parts, "\x00") }

func (s *slackMigSecrets) GetToken(provider, agentID, sourceID, key string) (string, error) {
	if s.readErr != nil && provider == agent.LegacySlackTokenProvider {
		return "", s.readErr
	}
	v, ok := s.rows[slackMigKey(provider, agentID, sourceID, key)]
	if !ok {
		return "", sql.ErrNoRows
	}
	return v, nil
}

func (s *slackMigSecrets) SetToken(provider, agentID, sourceID, key, value string, _ time.Time) error {
	s.rows[slackMigKey(provider, agentID, sourceID, key)] = value
	return nil
}

func (s *slackMigSecrets) DeleteToken(provider, agentID, sourceID, key string) error {
	delete(s.rows, slackMigKey(provider, agentID, sourceID, key))
	return nil
}

func (s *slackMigSecrets) DeleteTokensBySource(provider, agentID, sourceID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	prefix := slackMigKey(provider, agentID, sourceID, "")
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *slackMigSecrets) setLegacy(agentID, app, bot string) {
	if app != "" {
		s.rows[slackMigKey(agent.LegacySlackTokenProvider, agentID, "", agent.LegacySlackAppTokenKey)] = app
	}
	if bot != "" {
		s.rows[slackMigKey(agent.LegacySlackTokenProvider, agentID, "", agent.LegacySlackBotTokenKey)] = bot
	}
}

func (s *slackMigSecrets) legacyCount(agentID string) int {
	n := 0
	prefix := slackMigKey(agent.LegacySlackTokenProvider, agentID, "", "")
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// slackMigAgents stands in for the agent manager.
type slackMigAgents struct {
	bots     map[string]agent.LegacySlackBot
	clearErr error
}

func (a *slackMigAgents) LegacySlackBots() []agent.LegacySlackBot {
	var out []agent.LegacySlackBot
	for _, b := range a.bots {
		out = append(out, b)
	}
	return out
}

func (a *slackMigAgents) ClearLegacySlackBot(id string) error {
	if a.clearErr != nil {
		return a.clearErr
	}
	delete(a.bots, id)
	return nil
}

type slackMigFixture struct {
	t       *testing.T
	mig     *slackMigration
	ext     *extpkg.Manager
	agents  *slackMigAgents
	secrets *slackMigSecrets
	repo    string
	changed int
}

// newSlackMigFixture builds a migration whose "official source" is a
// local repository. The package is not installed yet.
func newSlackMigFixture(t *testing.T) *slackMigFixture {
	t.Helper()
	ext, err := extpkg.NewManager(t.TempDir(), "v9.0.0", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	f := &slackMigFixture{
		t:       t,
		ext:     ext,
		agents:  &slackMigAgents{bots: map[string]agent.LegacySlackBot{}},
		secrets: &slackMigSecrets{rows: map[string]string{}},
		repo:    newSlackMigTestRepo(t, slackMigTestManifest, slackMigTestSchema),
	}
	ext.SetSecretStore(f.secrets)
	f.mig = newSlackMigration(f.agents, f.secrets, ext, slog.Default(), func() { f.changed++ })
	f.mig.source = f.repo
	return f
}

// addLegacy registers an agent with built-in Slack settings, both
// tokens and one transcript.
func (f *slackMigFixture) addLegacy(id string, cfg agent.SlackBotConfig) agent.LegacySlackBot {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "chat_history", "slack")
	writeExtTestFiles(f.t, dir, map[string]string{"C1/1700.1.jsonl": "old\n"})
	bot := agent.LegacySlackBot{AgentID: id, Config: cfg, HistoryDir: dir}
	f.agents.bots[id] = bot
	f.secrets.setLegacy(id, "xapp-old", "xoxb-old")
	return bot
}

func (f *slackMigFixture) install(source string) {
	f.t.Helper()
	if _, err := f.ext.Install(context.Background(), extpkg.InstallRequest{
		URL: source, AckScopes: []string{"chat:session"}, Enabled: true,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *slackMigFixture) binding(id string) (extpkg.AgentBinding, bool) {
	f.t.Helper()
	row, err := f.ext.Get("slack")
	if err != nil {
		return extpkg.AgentBinding{}, false
	}
	b, ok := row.Agents[id]
	return b, ok
}

func (f *slackMigFixture) packageSecrets(id string) map[string]string {
	f.t.Helper()
	got, err := f.ext.Secrets("slack", id)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *slackMigFixture) history(id, rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.ext.DataDir("slack", id), "chat_history", "slack", filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

// wantPending asserts the agent is still listed, for the given reason,
// with its settings and tokens untouched.
func (f *slackMigFixture) wantPending(id, reason string) {
	f.t.Helper()
	for _, st := range f.mig.Pending() {
		if st.AgentID != id {
			continue
		}
		if st.Reason != reason {
			f.t.Fatalf("pending reason = %q (%s), want %q", st.Reason, st.Detail, reason)
		}
		if strings.Contains(st.Detail, "xapp-") || strings.Contains(st.Detail, "xoxb-") {
			f.t.Fatalf("detail leaks a token: %q", st.Detail)
		}
		return
	}
	f.t.Fatalf("agent %s is not pending; want reason %q", id, reason)
}

func (f *slackMigFixture) wantDone(id string) {
	f.t.Helper()
	if p := f.mig.Pending(); len(p) != 0 {
		f.t.Fatalf("still pending: %+v", p)
	}
	if n := f.secrets.legacyCount(id); n != 0 {
		f.t.Fatalf("%d legacy token(s) left behind", n)
	}
}

func ptrBool(v bool) *bool { return &v }

func TestSlackMigrationInstallsBindsCopiesAndCleansUp(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{
		Enabled: true, ThreadReplies: true, RespondMention: ptrBool(false),
	})

	f.mig.Run(context.Background())

	f.wantDone("ag_1")
	row, err := f.ext.Get("slack")
	if err != nil {
		t.Fatalf("package not installed: %v", err)
	}
	if !row.Enabled || !reflect.DeepEqual(row.GrantedScopes, []string{"chat:session"}) {
		t.Fatalf("installed row = enabled %v scopes %v", row.Enabled, row.GrantedScopes)
	}
	b, ok := f.binding("ag_1")
	if !ok || !b.Enabled {
		t.Fatalf("binding = %+v ok=%v, want enabled", b, ok)
	}
	// Unset "respond" switches meant true in the built-in bot.
	want := map[string]any{"threadReplies": true, "respondDM": true, "respondMention": false, "respondThread": true}
	if !reflect.DeepEqual(b.Config, want) {
		t.Fatalf("config = %#v, want %#v", b.Config, want)
	}
	if got := f.packageSecrets("ag_1"); got["appToken"] != "xapp-old" || got["botToken"] != "xoxb-old" {
		t.Fatalf("package secrets = %v", got)
	}
	if got := f.history("ag_1", "C1/1700.1.jsonl"); got != "old\n" {
		t.Fatalf("copied transcript = %q", got)
	}
	if f.changed == 0 {
		t.Fatal("services were not reconciled after binding")
	}
	// The tokens must not have landed in the registry file.
	state, err := os.ReadFile(filepath.Join(f.ext.Root(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "xapp-old") || strings.Contains(string(state), "xoxb-old") {
		t.Fatal("a token was written to the registry in the clear")
	}
}

func TestSlackMigrationKeepsEverythingWhenInstallFails(t *testing.T) {
	f := newSlackMigFixture(t)
	bot := f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	good := f.mig.source
	f.mig.source = filepath.Join(t.TempDir(), "no-such-repo")

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationInstallFailed)
	if _, err := f.ext.Get("slack"); !errors.Is(err, extpkg.ErrNotFound) {
		t.Fatalf("package row after a failed install: %v", err)
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}
	if got := f.agents.bots["ag_1"]; !reflect.DeepEqual(got, bot) {
		t.Fatalf("legacy settings changed: %+v", got)
	}

	// The retry, once the source is reachable.
	f.mig.source = good
	f.mig.Run(context.Background())
	f.wantDone("ag_1")
	if b, ok := f.binding("ag_1"); !ok || !b.Enabled {
		t.Fatalf("binding after retry = %+v ok=%v", b, ok)
	}
}

// A package that asks for more than the built-in bot had is not
// approved on the Owner's behalf.
func TestSlackMigrationDoesNotApproveExtraScopes(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.mig.source = newSlackMigTestRepo(t,
		strings.Replace(slackMigTestManifest, `["chat:session"]`, `["chat:session", "agents:read"]`, 1),
		slackMigTestSchema)

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationNeedsReview)
	if _, err := f.ext.Get("slack"); !errors.Is(err, extpkg.ErrNotFound) {
		t.Fatalf("package installed despite the scope difference: %v", err)
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}
}

// The trust decision must hold for the package the binding is written
// to, not the one read earlier: the Owner can replace it in between.
func TestSlackMigrationRechecksSourceWhenBinding(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.install(newSlackMigTestRepo(t, slackMigTestManifest, slackMigTestSchema))
	stale, err := f.ext.Get("slack")
	if err != nil {
		t.Fatal(err)
	}
	// What the migration believes it saw: the official package.
	stale.Source = f.mig.source

	_, reason, err := f.mig.migrateAgent(stale, f.mig.agents.LegacySlackBots()[0])
	if reason != slackMigrationUntrustedSource || err == nil {
		t.Fatalf("reason = %q, err = %v", reason, err)
	}
	if _, ok := f.binding("ag_1"); ok {
		t.Fatal("agent was bound to a package from an unknown source")
	}
	if got := f.packageSecrets("ag_1"); len(got) != 0 {
		t.Fatalf("tokens handed to an unknown package: %v", got)
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}
}

func TestSlackMigrationRefusesPackageFromAnotherSource(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.install(newSlackMigTestRepo(t, slackMigTestManifest, slackMigTestSchema))

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationUntrustedSource)
	if _, ok := f.binding("ag_1"); ok {
		t.Fatal("agent was bound to a package from an unknown source")
	}
	if got := f.packageSecrets("ag_1"); len(got) != 0 {
		t.Fatalf("tokens handed to a package from an unknown source: %v", got)
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}

	// Once the Owner binds that package to the agent themselves, the
	// choice is theirs and the migration completes the binding. What
	// they set stays as they set it.
	if _, err := f.ext.SetAgentBinding("slack", "ag_1", extpkg.AgentBinding{
		Enabled: false, Config: map[string]any{"respondDM": false},
	}); err != nil {
		t.Fatal(err)
	}
	f.mig.Run(context.Background())
	f.wantDone("ag_1")
	b, _ := f.binding("ag_1")
	if b.Config["respondDM"] != false {
		t.Fatalf("Owner's setting was overwritten: %#v", b.Config)
	}
	if !b.Enabled {
		t.Fatal("an enabled bot with both tokens should come out enabled")
	}
	if got := f.packageSecrets("ag_1"); got["appToken"] != "xapp-old" || got["botToken"] != "xoxb-old" {
		t.Fatalf("package secrets = %v", got)
	}
}

func TestSlackMigrationLeavesACompleteBindingAlone(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true, ThreadReplies: true})
	f.install(f.repo)
	if _, err := f.ext.SetAgentBinding("slack", "ag_1", extpkg.AgentBinding{
		Enabled: false,
		Config:  map[string]any{"threadReplies": false, "appToken": "xapp-new", "botToken": "xoxb-new"},
	}); err != nil {
		t.Fatal(err)
	}

	f.mig.Run(context.Background())

	f.wantDone("ag_1")
	b, _ := f.binding("ag_1")
	if b.Enabled || b.Config["threadReplies"] != false {
		t.Fatalf("binding was rewritten: %+v", b)
	}
	if got := f.packageSecrets("ag_1"); got["appToken"] != "xapp-new" || got["botToken"] != "xoxb-new" {
		t.Fatalf("package secrets = %v, want the Owner's own", got)
	}
	// The transcripts still move.
	if got := f.history("ag_1", "C1/1700.1.jsonl"); got != "old\n" {
		t.Fatalf("copied transcript = %q", got)
	}
}

// A binding can list a secret whose stored value is gone. The old store
// then has the only copy, so it must not be emptied.
func TestSlackMigrationKeepsTokensThePackageCannotReadBack(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.install(f.repo)
	if _, err := f.ext.SetAgentBinding("slack", "ag_1", extpkg.AgentBinding{
		Enabled: true,
		Config:  map[string]any{"appToken": "xapp-new", "botToken": "xoxb-new"},
	}); err != nil {
		t.Fatal(err)
	}
	for k := range f.secrets.rows {
		if !strings.HasPrefix(k, slackMigKey(agent.LegacySlackTokenProvider, "")) {
			delete(f.secrets.rows, k)
		}
	}

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationBindFailed)
	if n := f.secrets.legacyCount("ag_1"); n != 2 {
		t.Fatalf("%d legacy token(s) left, want both kept", n)
	}
	if got := f.history("ag_1", "C1/1700.1.jsonl"); got != "" {
		t.Fatalf("transcript copied before the tokens were confirmed: %q", got)
	}
}

// Each of these stops a run partway, the way a crash would, and checks
// that the next run finishes without redoing or losing anything.
func TestSlackMigrationResumesAfterInterruption(t *testing.T) {
	t.Run("after bind", func(t *testing.T) {
		f := newSlackMigFixture(t)
		f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
		// A file where the destination directory belongs makes the
		// copy fail after the binding has been written.
		blocker := filepath.Join(f.ext.DataDir("slack", "ag_1"), "chat_history")
		writeExtTestFiles(t, filepath.Dir(blocker), map[string]string{"chat_history": "x"})

		f.mig.Run(context.Background())
		f.wantPending("ag_1", slackMigrationCopyFailed)
		if f.secrets.legacyCount("ag_1") != 2 {
			t.Fatal("legacy tokens deleted before the transcripts were copied")
		}
		if got := f.packageSecrets("ag_1"); got["botToken"] != "xoxb-old" {
			t.Fatalf("package secrets = %v", got)
		}

		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
		f.mig.Run(context.Background())
		f.wantDone("ag_1")
		if got := f.history("ag_1", "C1/1700.1.jsonl"); got != "old\n" {
			t.Fatalf("copied transcript = %q", got)
		}
	})

	t.Run("after copy", func(t *testing.T) {
		f := newSlackMigFixture(t)
		f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
		f.secrets.deleteErr = errors.New("store down")

		f.mig.Run(context.Background())
		f.wantPending("ag_1", slackMigrationCleanupFailed)
		if _, still := f.agents.bots["ag_1"]; !still {
			t.Fatal("settings cleared although the tokens could not be deleted")
		}

		f.secrets.deleteErr = nil
		f.mig.Run(context.Background())
		f.wantDone("ag_1")
	})

	t.Run("after tokens deleted", func(t *testing.T) {
		f := newSlackMigFixture(t)
		f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
		f.agents.clearErr = agent.ErrAgentBusy

		f.mig.Run(context.Background())
		f.wantPending("ag_1", slackMigrationCleanupFailed)

		// The old tokens are gone now; the binding must survive a run
		// that can no longer read them.
		f.agents.clearErr = nil
		f.mig.Run(context.Background())
		f.wantDone("ag_1")
		b, _ := f.binding("ag_1")
		if !b.Enabled {
			t.Fatal("binding was disabled by the second run")
		}
		if got := f.packageSecrets("ag_1"); got["appToken"] != "xapp-old" || got["botToken"] != "xoxb-old" {
			t.Fatalf("package secrets = %v", got)
		}
	})
}

func TestSlackMigrationKeepsADisabledBotDisabled(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: false, RespondDM: ptrBool(false)})

	f.mig.Run(context.Background())

	f.wantDone("ag_1")
	b, ok := f.binding("ag_1")
	if !ok || b.Enabled {
		t.Fatalf("binding = %+v ok=%v, want present and disabled", b, ok)
	}
	if b.Config["respondDM"] != false {
		t.Fatalf("config = %#v", b.Config)
	}
	if got := f.packageSecrets("ag_1"); got["botToken"] != "xoxb-old" {
		t.Fatalf("package secrets = %v", got)
	}
}

func TestSlackMigrationSettingsWithoutTokens(t *testing.T) {
	f := newSlackMigFixture(t)
	bot := f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true, ThreadReplies: true})
	if err := f.secrets.DeleteTokensBySource(agent.LegacySlackTokenProvider, bot.AgentID, ""); err != nil {
		t.Fatal(err)
	}

	f.mig.Run(context.Background())

	f.wantDone("ag_1")
	b, ok := f.binding("ag_1")
	if !ok {
		t.Fatal("settings were not carried over")
	}
	// A service with no tokens could only fail to start.
	if b.Enabled {
		t.Fatal("binding enabled without tokens")
	}
	if b.Config["threadReplies"] != true || len(b.Secrets) != 0 {
		t.Fatalf("binding = %+v", b)
	}
}

// Tokens with no settings are not a configured bot: the settings are
// what say "this device ran Slack for this agent". The same holds for a
// device that never configured it at all.
func TestSlackMigrationDoesNothingWithoutSettings(t *testing.T) {
	f := newSlackMigFixture(t)
	f.secrets.setLegacy("ag_1", "xapp-old", "xoxb-old")
	f.mig.source = filepath.Join(t.TempDir(), "must-not-be-fetched")

	f.mig.Run(context.Background())

	if p := f.mig.Pending(); len(p) != 0 {
		t.Fatalf("pending = %+v", p)
	}
	if len(f.ext.List()) != 0 {
		t.Fatal("a package was installed on a device with nothing to migrate")
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("tokens deleted on a device that does not own the Slack settings")
	}
	if f.changed != 0 {
		t.Fatal("services reconciled although nothing changed")
	}
}

func TestSlackMigrationDoesNotOverwriteTranscripts(t *testing.T) {
	f := newSlackMigFixture(t)
	bot := f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	writeExtTestFiles(t, bot.HistoryDir, map[string]string{"C2/1800.2.jsonl": "only old\n"})
	dst := filepath.Join(f.ext.DataDir("slack", "ag_1"), "chat_history", "slack")
	writeExtTestFiles(t, dst, map[string]string{"C1/1700.1.jsonl": "newer\n"})

	f.mig.Run(context.Background())

	f.wantDone("ag_1")
	if got := f.history("ag_1", "C1/1700.1.jsonl"); got != "newer\n" {
		t.Fatalf("existing transcript = %q, want it untouched", got)
	}
	if got := f.history("ag_1", "C2/1800.2.jsonl"); got != "only old\n" {
		t.Fatalf("missing transcript = %q, want it copied", got)
	}
	// The originals stay where they were.
	if _, err := os.Stat(filepath.Join(bot.HistoryDir, "C1", "1700.1.jsonl")); err != nil {
		t.Fatalf("original transcript removed: %v", err)
	}
	// No temporary files left next to the copies.
	entries, err := os.ReadDir(filepath.Join(dst, "C2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("destination holds %d entries, want 1", len(entries))
	}
}

func TestSlackMigrationRefusesPackageWithoutSecretFields(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.mig.source = newSlackMigTestRepo(t, slackMigTestManifest,
		strings.ReplaceAll(slackMigTestSchema, `, "format": "password"`, ""))

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationIncompatible)
	if _, ok := f.binding("ag_1"); ok {
		t.Fatal("agent bound to a package that would store the tokens in the clear")
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}
}

func TestSlackMigrationKeepsTokensItCannotRead(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.secrets.readErr = errors.New("cannot decrypt")

	f.mig.Run(context.Background())

	f.wantPending("ag_1", slackMigrationTokensFailed)
	if _, ok := f.binding("ag_1"); ok {
		t.Fatal("agent bound without its tokens")
	}
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("unreadable tokens were deleted")
	}
}

func TestSlackMigrationWithoutRegistry(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	f.mig.ext = nil

	// Before any run the agent is already listed.
	f.wantPending("ag_1", slackMigrationInProgress)
	f.mig.Run(context.Background())
	f.wantPending("ag_1", slackMigrationUnavailable)
	if f.secrets.legacyCount("ag_1") != 2 {
		t.Fatal("legacy tokens were touched")
	}
}

func TestSameGitSource(t *testing.T) {
	const official = "https://github.com/loppo-llc/kojo-ext-slack"
	for src, want := range map[string]bool{
		official:          true,
		official + ".git": true,
		official + "/":    true,
		"https://GitHub.com/Loppo-LLC/kojo-ext-slack.git":  true,
		"git@github.com:loppo-llc/kojo-ext-slack.git":      true,
		"ssh://git@github.com/loppo-llc/kojo-ext-slack":    true,
		"https://github.com/someone-else/kojo-ext-slack":   false,
		"https://github.com/loppo-llc/kojo-ext-slack-fork": false,
		"https://example.com/loppo-llc/kojo-ext-slack":     false,
		"/tmp/loppo-llc/kojo-ext-slack":                    false,
		"":                                                 false,
	} {
		if got := sameGitSource(src, official); got != want {
			t.Errorf("sameGitSource(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestSlackMigrationHandlers(t *testing.T) {
	f := newSlackMigFixture(t)
	f.addLegacy("ag_1", agent.SlackBotConfig{Enabled: true})
	good := f.mig.source
	f.mig.source = filepath.Join(t.TempDir(), "no-such-repo")
	srv := &Server{logger: slog.Default(), slackMigration: f.mig}

	decode := func(body []byte) (out struct {
		Pending []slackMigrationStatus `json:"pending"`
		Source  string                 `json:"source"`
	}) {
		t.Helper()
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		return out
	}

	rr := extCall(t, srv.handleRetrySlackMigration, http.MethodPost, "/api/v1/slack-migration/retry", "", nil)
	got := decode(rr.Body.Bytes())
	if rr.Code != http.StatusOK || len(got.Pending) != 1 || got.Pending[0].Reason != slackMigrationInstallFailed {
		t.Fatalf("retry = %d %s", rr.Code, rr.Body.String())
	}
	if got.Source != slackOfficialSource {
		t.Fatalf("source = %q", got.Source)
	}

	f.mig.source = good
	rr = extCall(t, srv.handleRetrySlackMigration, http.MethodPost, "/api/v1/slack-migration/retry", "", nil)
	if got := decode(rr.Body.Bytes()); len(got.Pending) != 0 {
		t.Fatalf("pending after a good retry: %s", rr.Body.String())
	}

	// A daemon with no migration (PeerOnly) reports an empty list.
	rr = extCall(t, (&Server{logger: slog.Default()}).handleGetSlackMigration, http.MethodGet, "/api/v1/slack-migration", "", nil)
	if strings.TrimSpace(rr.Body.String()) == "" || len(decode(rr.Body.Bytes()).Pending) != 0 {
		t.Fatalf("empty status = %s", rr.Body.String())
	}
}
