package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

// Migration of the built-in Slack bot to the Slack extension package
//
// Slack used to be part of kojo. It is now the "slack" package, and an
// install that used the built-in bot is moved over here, once per
// agent, without the Owner doing anything when that is possible:
//
//  1. make sure the package is installed (install it from its official
//     source if not),
//  2. bind it to the agent with the old settings and tokens,
//  3. copy the thread transcripts into the package's data directory,
//  4. delete the old tokens, then the old settings.
//
// The old settings are the marker that an agent still needs migrating,
// and they are the last thing removed, so a run that stops anywhere —
// no network, a refused install, a crash — leaves the agent listed and
// the next run picks it up. Every step is safe to repeat: a binding
// that already holds both tokens is left as it is, and a transcript
// that already exists at the destination is never overwritten.
//
// The old transcripts are copied, not moved, and stay where they were.
// They are small, and deleting the only other copy of a conversation is
// not worth the disk it would save.
//
// Which device migrates: the one holding the old settings. They were
// never synced between devices (see keepLocalLegacySlackSettings), so
// exactly one device has them for a given agent — the one whose
// built-in bot was running — and a device without them finds nothing to
// do and deletes nothing.

const (
	// slackOfficialSource is where the Slack package is published. The
	// migration installs from here and nowhere else.
	slackOfficialSource = "https://github.com/loppo-llc/kojo-ext-slack"

	// The package's names for the two tokens (settings.schema.json).
	slackSecretAppToken = "appToken"
	slackSecretBotToken = "botToken"

	// slackMigrationRunTimeout bounds one run. Nearly all of it is the
	// package download.
	slackMigrationRunTimeout = 3 * time.Minute
)

// slackMigrationScopes is the capability set the migration approves on
// the Owner's behalf when it installs the package. Normally only the
// Owner approves a package's capabilities, in the install dialog. The
// exception is made here because the Owner already gave the built-in
// bot exactly this — holding conversations as the agent — when they
// configured it, and the migration only carries that decision over.
// Install refuses a manifest whose scopes differ from the acknowledged
// set, so a package version that asks for anything more is not
// installed automatically: the agent stays pending and the Owner
// reviews the package in the usual dialog.
var slackMigrationScopes = []string{"chat:session"}

// Why an agent is still waiting. The values are part of the API: the
// UI picks its explanation by them.
const (
	slackMigrationInProgress      = "in_progress"
	slackMigrationUnavailable     = "extensions_unavailable"
	slackMigrationInstallFailed   = "install_failed"
	slackMigrationNeedsReview     = "review_required"
	slackMigrationUntrustedSource = "untrusted_source"
	slackMigrationIncompatible    = "incompatible_package"
	slackMigrationTokensFailed    = "tokens_unreadable"
	slackMigrationBindFailed      = "bind_failed"
	slackMigrationCopyFailed      = "history_copy_failed"
	slackMigrationCleanupFailed   = "cleanup_failed"
)

// slackMigrationStatus is one agent that still has built-in Slack
// state, and why.
type slackMigrationStatus struct {
	AgentID string `json:"agentId"`
	Reason  string `json:"reason"`
	// Detail is the underlying error text, for the Owner to act on. It
	// never carries a token: nothing below puts one in an error.
	Detail string `json:"detail,omitempty"`
}

// slackLegacyAgents is the part of the agent manager the migration
// uses. An interface so the tests can run it without a manager.
type slackLegacyAgents interface {
	LegacySlackBots() []agent.LegacySlackBot
	ClearLegacySlackBot(id string) error
}

// slackLegacyTokens reads and deletes the built-in bot's tokens.
type slackLegacyTokens interface {
	GetToken(provider, agentID, sourceID, key string) (string, error)
	DeleteTokensBySource(provider, agentID, sourceID string) error
}

type slackMigration struct {
	agents slackLegacyAgents
	tokens slackLegacyTokens
	// ext is nil when this daemon has no extension registry. Nothing
	// can be migrated then; the status says so.
	ext    *extpkg.Manager
	source string
	logger *slog.Logger
	// changed is called after a run that touched the registry, so the
	// newly bound services start.
	changed func()

	// runMu serializes runs: startup and a manual retry may overlap.
	runMu sync.Mutex

	mu   sync.Mutex
	last map[string]slackMigrationStatus
}

func newSlackMigration(agents slackLegacyAgents, tokens slackLegacyTokens, ext *extpkg.Manager, logger *slog.Logger, changed func()) *slackMigration {
	if logger == nil {
		logger = slog.Default()
	}
	if changed == nil {
		changed = func() {}
	}
	return &slackMigration{
		agents:  agents,
		tokens:  tokens,
		ext:     ext,
		source:  slackOfficialSource,
		logger:  logger.With("component", "slack-migration"),
		changed: changed,
		last:    map[string]slackMigrationStatus{},
	}
}

// Pending lists the agents that still have built-in Slack state. The
// list itself comes from the agent records, not from the last run, so
// it is right before the first run and after anything else changed the
// records; the last run only supplies the reason.
func (m *slackMigration) Pending() []slackMigrationStatus {
	legacy := m.agents.LegacySlackBots()
	out := make([]slackMigrationStatus, 0, len(legacy))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range legacy {
		st, ok := m.last[l.AgentID]
		if !ok {
			st = slackMigrationStatus{AgentID: l.AgentID, Reason: slackMigrationInProgress}
		}
		out = append(out, st)
	}
	return out
}

// Run migrates every agent that still has built-in Slack state. It
// returns nothing: a failure is recorded per agent for Pending to
// report, and is retried by the next run.
func (m *slackMigration) Run(ctx context.Context) {
	m.runMu.Lock()
	defer m.runMu.Unlock()

	legacy := m.agents.LegacySlackBots()
	results := make(map[string]slackMigrationStatus, len(legacy))
	defer func() {
		m.mu.Lock()
		m.last = results
		m.mu.Unlock()
	}()
	if len(legacy) == 0 {
		return
	}
	fail := func(agentID, reason string, err error) {
		st := slackMigrationStatus{AgentID: agentID, Reason: reason}
		if err != nil {
			st.Detail = err.Error()
		}
		results[agentID] = st
		m.logger.Warn("built-in Slack bot not migrated; will retry",
			"agent", agentID, "reason", reason, "err", err)
	}

	pkg, reason, err := m.ensurePackage(ctx)
	if reason != "" {
		for _, l := range legacy {
			fail(l.AgentID, reason, err)
		}
		return
	}

	touched := false
	for _, l := range legacy {
		bound, reason, err := m.migrateAgent(pkg, l)
		touched = touched || bound
		if reason != "" {
			fail(l.AgentID, reason, err)
			continue
		}
		m.logger.Info("built-in Slack bot migrated to the slack package", "agent", l.AgentID)
	}
	if touched {
		m.changed()
	}
}

// ensurePackage returns the installed Slack package, installing it
// first when it is missing. A non-empty reason means no agent can be
// migrated in this run.
func (m *slackMigration) ensurePackage(ctx context.Context) (extpkg.Installed, string, error) {
	if m.ext == nil {
		return extpkg.Installed{}, slackMigrationUnavailable, nil
	}
	pkg, err := m.ext.Get(extensionSlackPackageID)
	if err == nil {
		return pkg, "", nil
	}
	if !errors.Is(err, extpkg.ErrNotFound) {
		return extpkg.Installed{}, slackMigrationInstallFailed, err
	}
	pkg, err = m.ext.Install(ctx, extpkg.InstallRequest{
		URL:       m.source,
		AckScopes: slackMigrationScopes,
		Enabled:   true,
	})
	var mismatch *extpkg.ScopeMismatchError
	switch {
	case err == nil:
		m.logger.Info("slack package installed for migration", "source", m.source, "commit", pkg.Commit)
		// Tell the UI and the supervisor even if no agent ends up
		// bound: the package list has changed.
		m.changed()
		return pkg, "", nil
	case errors.Is(err, extpkg.ErrAlreadyInstalled):
		// The Owner installed it while this run was fetching.
		pkg, err = m.ext.Get(extensionSlackPackageID)
		if err != nil {
			return extpkg.Installed{}, slackMigrationInstallFailed, err
		}
		return pkg, "", nil
	case errors.As(err, &mismatch):
		return extpkg.Installed{}, slackMigrationNeedsReview, err
	default:
		return extpkg.Installed{}, slackMigrationInstallFailed, err
	}
}

// migrateAgent moves one agent. bound reports whether the registry was
// written; a non-empty reason means the agent is still pending.
func (m *slackMigration) migrateAgent(pkg extpkg.Installed, l agent.LegacySlackBot) (bound bool, reason string, err error) {
	binding, hasBinding := pkg.Agents[l.AgentID]

	// The package ID is all that identifies "the Slack package": any
	// repository can ship a manifest that says "slack", and whichever
	// one is installed under that ID gets the session namespace and the
	// tokens. So the tokens are handed over only when the Owner has
	// vouched for this particular checkout — it came from the official
	// source, or the Owner has already bound it to this agent
	// themselves, which is the same decision made by hand. A package
	// under this ID from anywhere else leaves the agent pending for the
	// Owner to sort out; kojo does not guess.
	if err := m.trusted(pkg, l.AgentID); err != nil {
		return false, slackMigrationUntrustedSource, err
	}

	// A token is only safe to hand over as a secret. If this version of
	// the package does not declare the two keys as secrets, the values
	// would be written to the registry file in the clear.
	secretKeys, err := m.ext.SecretSettingKeys(pkg.ID)
	if err != nil {
		return false, slackMigrationIncompatible, err
	}
	if err := acceptsSlackTokens(secretKeys); err != nil {
		return false, slackMigrationIncompatible, err
	}

	appToken, err := m.legacyToken(l.AgentID, agent.LegacySlackAppTokenKey)
	if err != nil {
		return false, slackMigrationTokensFailed, err
	}
	botToken, err := m.legacyToken(l.AgentID, agent.LegacySlackBotTokenKey)
	if err != nil {
		return false, slackMigrationTokensFailed, err
	}

	// A binding that already has both tokens is the Owner's own setup,
	// or this migration's from a run that stopped before the cleanup.
	// Either way it is not rewritten.
	if !hasBinding || !containsAll(binding.Secrets, slackSecretAppToken, slackSecretBotToken) {
		cfg := map[string]any{
			"threadReplies":  l.Config.ThreadReplies,
			"respondDM":      l.Config.ReactDM(),
			"respondMention": l.Config.ReactMention(),
			"respondThread":  l.Config.ReactThread(),
		}
		// Whatever the Owner already set on a partial binding wins
		// over the old value.
		for k, v := range binding.Config {
			cfg[k] = v
		}
		// An empty string means "keep what is stored", so a token the
		// old store did not have leaves the binding's own untouched.
		have := map[string]bool{}
		for _, k := range binding.Secrets {
			have[k] = true
		}
		if !have[slackSecretAppToken] && appToken != "" {
			cfg[slackSecretAppToken] = appToken
			have[slackSecretAppToken] = true
		}
		if !have[slackSecretBotToken] && botToken != "" {
			cfg[slackSecretBotToken] = botToken
			have[slackSecretBotToken] = true
		}
		// Without both tokens the service cannot connect and would
		// only restart in a loop, exactly as the built-in bot could
		// not start. The settings still move; the Owner enables it
		// after entering the tokens.
		complete := have[slackSecretAppToken] && have[slackSecretBotToken]
		enabled := (l.Config.Enabled && complete) || (hasBinding && binding.Enabled)
		// pkg was read a moment ago, and the Owner may have replaced,
		// updated or unbound the package since. The registry repeats
		// both checks on the row it is about to write, under its lock.
		_, err := m.ext.SetAgentBindingIf(pkg.ID, l.AgentID, extpkg.AgentBinding{
			Enabled: enabled,
			Config:  cfg,
		}, func(current extpkg.Installed, secretKeys []string) error {
			if err := m.trusted(current, l.AgentID); err != nil {
				return err
			}
			// cfg was merged from the binding as first read. If the
			// Owner edited it since, writing would undo their edit;
			// the next run merges from what they saved.
			if now, ok := current.Agents[l.AgentID]; ok != hasBinding || !reflect.DeepEqual(now, binding) {
				return errSlackBindingChanged
			}
			return acceptsSlackTokens(secretKeys)
		})
		var untrusted *slackUntrustedSourceError
		switch {
		case errors.As(err, &untrusted):
			return false, slackMigrationUntrustedSource, err
		case errors.Is(err, errSlackTokensNotSecret):
			return false, slackMigrationIncompatible, err
		case err != nil:
			return false, slackMigrationBindFailed, err
		}
		bound = true
	}

	// Everything above worked from a package read at the start. If the
	// Owner removed or replaced the package meanwhile, the binding went
	// with it: the transcripts must not go into the newcomer's data
	// directory, and the old store holds the only copy of the tokens.
	// So look again before each of the two steps. The registry is not
	// locked across them; what is left is an Owner uninstalling within
	// the same instant, which discards the tokens by their own hand.
	stillHolds := func() error {
		current, err := m.ext.Get(pkg.ID)
		if err != nil {
			return err
		}
		if err := m.trusted(current, l.AgentID); err != nil {
			return err
		}
		held := current.Agents[l.AgentID].Secrets
		if (appToken != "" && !containsAll(held, slackSecretAppToken)) ||
			(botToken != "" && !containsAll(held, slackSecretBotToken)) {
			return errors.New("the slack package no longer holds this agent's tokens; it was changed while migrating")
		}
		return nil
	}
	if err := stillHolds(); err != nil {
		return bound, slackMigrationBindFailed, err
	}
	dst := filepath.Join(m.ext.DataDir(pkg.ID, l.AgentID), "chat_history", "slack")
	if err := copyTreeKeepingExisting(l.HistoryDir, dst); err != nil {
		return bound, slackMigrationCopyFailed, err
	}
	if err := stillHolds(); err != nil {
		return bound, slackMigrationBindFailed, err
	}

	// Tokens before settings. The settings are what lists the agent as
	// pending, so if this stops between the two the next run sees the
	// agent again, finds the binding complete, and finishes here. The
	// other order could strand the tokens in the credential store with
	// nothing left to say they need deleting.
	if err := m.tokens.DeleteTokensBySource(agent.LegacySlackTokenProvider, l.AgentID, agent.LegacySlackTokenSource); err != nil {
		return bound, slackMigrationCleanupFailed, err
	}
	if err := m.agents.ClearLegacySlackBot(l.AgentID); err != nil {
		return bound, slackMigrationCleanupFailed, err
	}
	return bound, "", nil
}

var errSlackBindingChanged = errors.New("the agent's slack package settings were changed while migrating; retry")

var errSlackTokensNotSecret = errors.New("the installed slack package does not accept the app and bot tokens as secrets")

// acceptsSlackTokens checks that the package files both tokens as
// secrets, given the keys its schema marks so.
func acceptsSlackTokens(secretKeys []string) error {
	if !containsAll(secretKeys, slackSecretAppToken, slackSecretBotToken) {
		return errSlackTokensNotSecret
	}
	return nil
}

// slackUntrustedSourceError says the installed "slack" package is not
// one the migration may hand tokens to.
type slackUntrustedSourceError struct{ installed, official string }

func (e *slackUntrustedSourceError) Error() string {
	return fmt.Sprintf("the installed slack package comes from %s, not %s", e.installed, e.official)
}

// trusted reports whether the Owner has vouched for this checkout of
// the package as far as one agent is concerned (see migrateAgent).
func (m *slackMigration) trusted(pkg extpkg.Installed, agentID string) error {
	if _, bound := pkg.Agents[agentID]; bound || sameGitSource(pkg.Source, m.source) {
		return nil
	}
	return &slackUntrustedSourceError{installed: pkg.Source, official: m.source}
}

// legacyToken reads one of the built-in bot's tokens. A token that was
// never stored is "", not an error; any other failure is, because
// treating an unreadable token as absent would migrate the agent
// without it and then delete it.
func (m *slackMigration) legacyToken(agentID, key string) (string, error) {
	v, err := m.tokens.GetToken(agent.LegacySlackTokenProvider, agentID, agent.LegacySlackTokenSource, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read the stored Slack %s: %w", key, err)
	}
	return strings.TrimSpace(v), nil
}

func containsAll(list []string, want ...string) bool {
	have := make(map[string]bool, len(list))
	for _, s := range list {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// sameGitSource reports whether two git URLs name the same repository,
// ignoring the differences that do not change which repository it is:
// letter case, a trailing slash or ".git", and whether it is spelled as
// an https, ssh or scp-style address.
func sameGitSource(a, b string) bool {
	return canonicalGitSource(a) == canonicalGitSource(b)
}

func canonicalGitSource(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "https://"):
		s = strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "ssh://git@"):
		s = strings.TrimPrefix(s, "ssh://git@")
	case strings.HasPrefix(s, "git@"):
		// scp-style: git@host:owner/repo
		s = strings.Replace(strings.TrimPrefix(s, "git@"), ":", "/", 1)
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}

// copyTreeKeepingExisting copies the regular files under src to the
// same relative paths under dst. A file that already exists at the
// destination is left alone, whatever it contains, so the copy can be
// repeated and can run next to a package that is already writing there.
// A missing src is not an error. Symlinks and other special files are
// skipped: the tree is plain transcripts, and following a link could
// copy something from outside it.
func copyTreeKeepingExisting(src, dst string) error {
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFileKeepingExisting(path, target)
	})
}

// copyFileKeepingExisting copies one file so that the destination is
// never replaced: the package may be running and writing the same
// thread. The bytes go to a temporary file first; it is then hard-linked
// into place, which fails rather than replaces if the name was taken in
// the meantime, and leaves the destination either absent or complete.
func copyFileKeepingExisting(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".migrate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	err = os.Link(tmpName, dst)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	// A filesystem without hard links. Rename would replace a file the
	// package created in the meantime, so claim the name exclusively
	// and copy into it. A crash mid-copy can leave it short; the old
	// transcript is still in place and that is the lesser loss.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// RunSlackMigration runs the migration once. Called at startup after
// the extension subsystem is up, on its own goroutine: it may download
// the package, and nothing else waits for it.
func (s *Server) RunSlackMigration(ctx context.Context) {
	if s == nil || s.slackMigration == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, slackMigrationRunTimeout)
	defer cancel()
	s.slackMigration.Run(ctx)
}

// writeSlackMigrationStatus is the shared response of both routes.
func (s *Server) writeSlackMigrationStatus(w http.ResponseWriter) {
	pending := []slackMigrationStatus{}
	if s.slackMigration != nil {
		pending = s.slackMigration.Pending()
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"pending": pending,
		// The address to install from by hand when the automatic
		// install cannot work.
		"source": slackOfficialSource,
	})
}

// handleGetSlackMigration reports which agents still have built-in
// Slack state. Owner-only, like the extension routes it sits beside:
// the policy layer denies it to every other role by default.
func (s *Server) handleGetSlackMigration(w http.ResponseWriter, _ *http.Request) {
	s.writeSlackMigrationStatus(w)
}

// handleRetrySlackMigration runs the migration again and reports what
// is left. It waits for the run so the caller sees the outcome.
func (s *Server) handleRetrySlackMigration(w http.ResponseWriter, r *http.Request) {
	s.RunSlackMigration(r.Context())
	s.writeSlackMigrationStatus(w)
}
