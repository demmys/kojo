package extpkg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Service supervision
//
// A package that needs to run code ships an executable and kojo keeps
// it alive: spawn, pipe its output into the server log, restart it with
// exponential backoff when it dies. The process is NOT sandboxed (v1
// threat model: installing an extension is as privileged as running the
// binary yourself), but it is scoped — it reaches kojo only through the
// HTTP API with its own token, and that token only opens the routes the
// operator acknowledged at install time.
//
// The supervisor is declarative. Reconcile computes the set of
// processes the registry says should exist and diffs it against what is
// running, so every mutation path (install, update, enable, disable,
// bind, unbind, config change, removal) is just "mutate the registry,
// then Reconcile".

const (
	// serviceStopGrace is how long a process gets to exit after SIGTERM
	// (Interrupt on Windows) before it is killed.
	serviceStopGrace = 5 * time.Second
	// backoffMin/backoffMax bound the restart delay for a crashing
	// service.
	backoffMin = 500 * time.Millisecond
	backoffMax = 2 * time.Minute
	// healthyRun is how long a process must stay up before its restart
	// backoff is considered recovered and reset to backoffMin.
	healthyRun = 60 * time.Second
	// logLineMax caps a single captured output line so a service that
	// emits an unterminated megabyte cannot pin memory in the reader.
	// Everything past it is consumed and dropped rather than left in
	// the pipe, which would block the child on its next write.
	logLineMax = 64 * 1024
	// groupKillGrace is how long the process group gets after SIGTERM
	// before it is SIGKILLed. Must stay below serviceStopGrace.
	groupKillGrace = 2500 * time.Millisecond
	// shutdownDeadline bounds Supervisor.Shutdown as a whole.
	shutdownDeadline = 30 * time.Second
	// drainGrace is how long the output readers get to finish after
	// the process has exited. A grandchild that inherited the pipe can
	// hold it open indefinitely, so the read ends are closed once this
	// expires.
	drainGrace = 2 * time.Second
)

// serviceSpec is the desired state of one supervised process. Two specs
// that compare equal describe an identical process, so Reconcile can
// decide "restart" versus "leave alone" by comparing them.
type serviceSpec struct {
	ExtensionID string
	AgentID     string // empty for a global-scope service
	// Commit is the checkout the binary came from. It is part of the
	// spec because an update can replace the executable while leaving
	// its path and argv identical — without this the supervisor would
	// see no change and keep running the previous build.
	Commit string
	Bin    string
	Args   []string
	Env    []string
}

func (s serviceSpec) key() string { return s.ExtensionID + "\x00" + s.AgentID }

func (s serviceSpec) equal(o serviceSpec) bool {
	if s.Bin != o.Bin || s.ExtensionID != o.ExtensionID || s.AgentID != o.AgentID {
		return false
	}
	if s.Commit != o.Commit {
		return false
	}
	return sliceEqual(s.Args, o.Args) && sliceEqual(s.Env, o.Env)
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// serviceProc is one running (or restarting) supervised process.
type serviceProc struct {
	spec   serviceSpec
	cancel context.CancelFunc
	done   chan struct{}
}

// Supervisor owns the lifecycle of every extension service process.
type Supervisor struct {
	mgr    *Manager
	logger *slog.Logger

	// recMu serialises Reconcile end to end. The desired set is
	// computed from the registry BEFORE the process map is locked, so
	// two concurrent reconciles could otherwise interleave and let the
	// older snapshot win — resurrecting a service the operator just
	// disabled.
	recMu sync.Mutex

	mu sync.Mutex
	// agentActive, when set, reports whether a per-agent service
	// should run for this agent. The registry tracks bindings, not
	// agent lifecycle, so archiving an agent would otherwise leave its
	// service running against a row nobody can reach.
	agentActive func(agentID string) bool
	procs       map[string]*serviceProc
	// stopping holds processes that have been cancelled but have not
	// finished exiting. They are out of procs already, so Shutdown
	// would not see them; it waits on these too, otherwise the server
	// exits while an extension is still winding down.
	stopping map[*serviceProc]struct{}
	stopped  bool
}

// SetAgentFilter installs the predicate that decides whether a
// per-agent service may run. Nil means "every bound agent".
func (s *Supervisor) SetAgentFilter(fn func(agentID string) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.agentActive = fn
	s.mu.Unlock()
}

// agentAllowed applies the filter. A nil supervisor or nil filter
// admits everything.
func (s *Supervisor) agentAllowed(agentID string) bool {
	s.mu.Lock()
	fn := s.agentActive
	s.mu.Unlock()
	return fn == nil || fn(agentID)
}

// NewSupervisor builds a supervisor for the registry. Supervision stays
// idle until the Manager has an API base (Manager.SetAPIBase): a
// service that cannot call kojo back has nothing useful to do, and
// starting one early would hand it an empty KOJO_API_BASE it has
// already read by the time the listener comes up.
func NewSupervisor(mgr *Manager, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		mgr:      mgr,
		logger:   logger.With("component", "extsvc"),
		procs:    map[string]*serviceProc{},
		stopping: map[*serviceProc]struct{}{},
	}
}

// Reconcile brings the running process set in line with the registry.
// Safe to call repeatedly; callers fire it after every registry
// mutation and once at boot.
func (s *Supervisor) Reconcile() {
	if s == nil || s.mgr == nil || s.mgr.APIBase() == "" {
		return
	}
	s.recMu.Lock()
	defer s.recMu.Unlock()
	want := s.desired()

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	// Stop anything that is gone or whose command line changed. A
	// changed spec is a stop+start rather than an in-place update:
	// the process has already read its old env.
	var stopped []*serviceProc
	for key, proc := range s.procs {
		spec, keep := want[key]
		if keep && proc.spec.equal(spec) {
			continue
		}
		s.stopLocked(key, proc)
		stopped = append(stopped, proc)
	}
	s.mu.Unlock()

	// Wait for the ones just stopped before starting their
	// replacements. Two processes for the same extension+agent must
	// never overlap: they share a data directory and, for a service
	// that binds a port or holds a lock, the new one would simply fail
	// to come up. The wait is bounded by cmd.WaitDelay.
	for _, proc := range stopped {
		<-proc.done
		s.mu.Lock()
		delete(s.stopping, proc)
		s.mu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for key, spec := range want {
		if _, running := s.procs[key]; running {
			continue
		}
		s.startLocked(key, spec)
	}
}

// Shutdown stops every supervised process and blocks until they exit.
func (s *Supervisor) Shutdown() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	procs := make([]*serviceProc, 0, len(s.procs)+len(s.stopping))
	for key, proc := range s.procs {
		proc.cancel()
		delete(s.procs, key)
		procs = append(procs, proc)
	}
	// Processes a concurrent Reconcile already cancelled are still
	// ours to wait for.
	for proc := range s.stopping {
		procs = append(procs, proc)
	}
	s.mu.Unlock()

	// Every process is killed once cmd.WaitDelay expires, so this
	// normally returns in well under the deadline. The deadline is
	// there for the case that bound fails anyway (a process stuck in
	// uninterruptible IO, say): a daemon restart must not hang on an
	// extension.
	deadline := time.After(shutdownDeadline)
	for _, proc := range procs {
		select {
		case <-proc.done:
		case <-deadline:
			s.logger.Warn("extension services did not all exit; abandoning the wait",
				"extension", proc.spec.ExtensionID, "agent", proc.spec.AgentID)
			return
		}
	}
}

// Running reports the extension/agent pairs with a live supervisor
// loop, for the status column in the UI and for tests.
func (s *Supervisor) Running() []serviceSpec {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]serviceSpec, 0, len(s.procs))
	for _, proc := range s.procs {
		out = append(out, proc.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ServiceStatus is one supervised process, in the shape the owner API
// reports it. serviceSpec itself stays internal: it carries the
// process environment, which includes the extension's token.
type ServiceStatus struct {
	ExtensionID string `json:"extensionId"`
	AgentID     string `json:"agentId,omitempty"`
}

// RunningServices lists the live processes for the status column in the
// settings UI.
func (s *Supervisor) RunningServices() []ServiceStatus {
	specs := s.Running()
	out := make([]ServiceStatus, 0, len(specs))
	for _, sp := range specs {
		out = append(out, ServiceStatus{ExtensionID: sp.ExtensionID, AgentID: sp.AgentID})
	}
	return out
}

// stopLocked cancels a process and drops it from the map. It does not
// wait: holding the lock across a 5-second termination grace would
// block every other registry mutation.
func (s *Supervisor) stopLocked(key string, proc *serviceProc) {
	proc.cancel()
	delete(s.procs, key)
	s.stopping[proc] = struct{}{}
	s.logger.Info("extension service stopping",
		"extension", proc.spec.ExtensionID, "agent", proc.spec.AgentID)
}

func (s *Supervisor) startLocked(key string, spec serviceSpec) {
	ctx, cancel := context.WithCancel(context.Background())
	proc := &serviceProc{spec: spec, cancel: cancel, done: make(chan struct{})}
	s.procs[key] = proc
	go func() {
		defer close(proc.done)
		s.run(ctx, spec)
	}()
}

// run is the supervision loop for one process: spawn, stream output,
// wait, back off, repeat until the context is cancelled.
func (s *Supervisor) run(ctx context.Context, spec serviceSpec) {
	log := s.logger.With("extension", spec.ExtensionID)
	if spec.AgentID != "" {
		log = log.With("agent", spec.AgentID)
	}
	delay := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := s.runOnce(ctx, spec, log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= healthyRun {
			// The process ran long enough to count as healthy, so
			// this is a fresh failure rather than a crash loop.
			delay = backoffMin
		}
		log.Warn("extension service exited, restarting", "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > backoffMax {
			delay = backoffMax
		}
	}
}

// runOnce spawns the process and waits for it to exit.
func (s *Supervisor) runOnce(ctx context.Context, spec serviceSpec, log *slog.Logger) error {
	// The command is not run through a shell and every argument comes
	// from the manifest as a discrete string, so there is no word
	// splitting for a crafted arg to exploit.
	cmd := exec.CommandContext(ctx, spec.Bin, spec.Args...)
	cmd.Dir = filepath.Dir(spec.Bin)
	cmd.Env = spec.Env
	// Give the process a chance to flush and exit on cancel before the
	// hard kill CommandContext would otherwise apply immediately.
	cmd.Cancel = func() error { return terminate(cmd) }
	cmd.WaitDelay = serviceStopGrace

	// os.Pipe rather than cmd.StdoutPipe: StdoutPipe's contract is
	// "drain fully, THEN call Wait", and a grandchild holding the
	// write end never gives that first step an end — the supervisor
	// would sit in the read while cmd.WaitDelay, which only runs
	// inside Wait, never gets a chance to kill anything. Owning the
	// pipes lets Wait run first and the readers be cut loose after.
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return err
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	setProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return fmt.Errorf("start %s: %w", spec.Bin, err)
	}
	// The child holds its own descriptors now; keeping the parent's
	// copies open would mean the readers never see EOF.
	outW.Close()
	errW.Close()
	log.Info("extension service started", "bin", spec.Bin, "pid", cmd.Process.Pid)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); pipeToLog(outR, log, slog.LevelInfo) }()
		go func() { defer wg.Done(); pipeToLog(errR, log, slog.LevelWarn) }()
		wg.Wait()
	}()

	// Escalation. cmd.Cancel signals the whole process group, so every
	// descendant already got a SIGTERM; what survives that is a
	// grandchild ignoring it, and cmd.WaitDelay only reaches the direct
	// child.
	//
	// Wait runs on its own goroutine so the SIGKILL decision stays
	// here. Signalling a process GROUP means naming a raw PGID, which
	// the kernel is free to reuse the moment the leader is reaped, so
	// the kill must happen while Wait is still outstanding. That is the
	// whole reason for the shape below, and it forces a choice: kill
	// the group unconditionally on cancel and risk hitting a recycled
	// PGID, or only kill while the leader is provably unreaped and
	// accept that a leader which exits promptly leaves a
	// SIGTERM-ignoring grandchild behind. Closing both at once needs a
	// pidfd, which the standard library does not expose for groups.
	//
	// This picks the second: never signal a group we might no longer
	// own. A grandchild that outlives a SIGTERM its parent obeyed is a
	// bug in the package; signalling a stranger's process group is a
	// bug in kojo.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		select {
		case waitErr = <-waitDone:
		case <-time.After(groupKillGrace):
			killGroup(cmd)
			waitErr = <-waitDone
		}
	}
	// Anything the service left behind sharing the pipe gets a short
	// grace period to finish its line, then the read ends close and
	// the readers return whether it is done or not.
	select {
	case <-drained:
	case <-time.After(drainGrace):
	}
	outR.Close()
	errR.Close()
	<-drained
	return waitErr
}

// pipeToLog forwards a service's output stream into the server log one
// line at a time. Over-long lines are truncated rather than dropped, so
// a chatty service degrades instead of losing its diagnostics.
func pipeToLog(r io.Reader, log *slog.Logger, level slog.Level) {
	br := bufio.NewReaderSize(r, 8192)
	// line accumulates at most logLineMax bytes; everything past that
	// is counted and dropped. ReadSlice rather than ReadString or
	// Scanner: Scanner stops reading entirely on an over-long line,
	// which fills the pipe and blocks the service on its next write,
	// and ReadString would allocate the whole line before returning it
	// — a service emitting a gigabyte without a newline would take the
	// server down with it. ReadSlice hands back one buffer-sized chunk
	// at a time, so the overflow is consumed and discarded.
	var line []byte
	dropped := 0
	flush := func() {
		text := strings.TrimRight(string(line), "\r\n")
		if dropped > 0 {
			text += fmt.Sprintf("…(truncated, %d more bytes)", dropped)
		}
		if text != "" {
			log.Log(context.Background(), level, "extension service output", "line", text)
		}
		line = line[:0]
		dropped = 0
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if room := logLineMax - len(line); room > 0 {
			if len(chunk) > room {
				line = append(line, chunk[:room]...)
				dropped += len(chunk) - room
			} else {
				line = append(line, chunk...)
			}
		} else {
			dropped += len(chunk)
		}
		switch err {
		case nil:
			flush()
		case bufio.ErrBufferFull:
			// Mid-line: keep reading until the newline arrives.
		default:
			if len(line) > 0 || dropped > 0 {
				flush()
			}
			return
		}
	}
}

// desired computes the process set the registry currently calls for.
func (s *Supervisor) desired() map[string]serviceSpec {
	out := map[string]serviceSpec{}
	for _, row := range s.mgr.List() {
		svc := row.Manifest.Contributes.Service
		if svc == nil || !row.Enabled {
			continue
		}
		rel, ok := svc.Exec[runtime.GOOS+"/"+runtime.GOARCH]
		if !ok {
			// Install-time validation rejects this, so reaching it
			// means the checkout was swapped underneath us.
			s.logger.Warn("extension service has no executable for this platform",
				"extension", row.ID)
			continue
		}
		bin := filepath.Join(s.mgr.Dir(row.ID), filepath.FromSlash(rel))
		token, err := s.mgr.Token(row.ID)
		if err != nil || token == "" {
			s.logger.Warn("extension has no token; service not started",
				"extension", row.ID, "error", err)
			continue
		}
		switch svc.Scope {
		case ScopeGlobal:
			spec := s.spec(row, svc, bin, token, "")
			out[spec.key()] = spec
		case ScopePerAgent:
			for _, agentID := range enabledAgents(row) {
				if !s.agentAllowed(agentID) {
					continue
				}
				spec := s.spec(row, svc, bin, token, agentID)
				out[spec.key()] = spec
			}
		}
	}
	return out
}

// enabledAgents lists the agents a package is bound to and enabled for,
// in a stable order.
func enabledAgents(row Installed) []string {
	ids := make([]string, 0, len(row.Agents))
	for id, b := range row.Agents {
		if b.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// spec builds the launch descriptor for one process.
func (s *Supervisor) spec(row Installed, svc *Service, bin, token, agentID string) serviceSpec {
	dataDir := s.mgr.DataDir(row.ID, agentID)
	// A service that cannot write its own state is still worth
	// starting — plenty of them are stateless — so a mkdir failure is
	// logged and the variable still points where it should.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		s.logger.Warn("create extension data dir failed",
			"extension", row.ID, "path", dataDir, "error", err)
	}
	cfg := row.Config
	if agentID != "" {
		if b, ok := row.Agents[agentID]; ok && b.Config != nil {
			cfg = b.Config
		}
	}
	cfgJSON := "{}"
	if len(cfg) > 0 {
		if data, err := json.Marshal(cfg); err == nil {
			cfgJSON = string(data)
		}
	}
	// A minimal environment. The service inherits PATH, HOME and the
	// platform essentials so it can find an interpreter, plus its own
	// KOJO_EXT_* contract — and nothing else from kojo's environment,
	// so an operator's unrelated secrets are not handed to every
	// package they install.
	// A supervised process is spawned by kojo itself, so its
	// environment never appears on anyone's command line and the token
	// can travel in it. The file is written anyway, so a package can
	// use one code path for its service and its MCP server.
	tokenFile := filepath.Join(dataDir, tokenFilename)
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		s.logger.Warn("write extension token file failed",
			"extension", row.ID, "path", tokenFile, "error", err)
	}
	env := baseServiceEnv()
	env = append(env,
		"KOJO_API_BASE="+s.mgr.APIBase(),
		"KOJO_EXT_ID="+row.ID,
		"KOJO_EXT_TOKEN="+token,
		"KOJO_EXT_TOKEN_FILE="+tokenFile,
		"KOJO_EXT_VERSION="+row.Manifest.Version,
		"KOJO_EXT_DATA_DIR="+dataDir,
		"KOJO_EXT_DIR="+s.mgr.Dir(row.ID),
		"KOJO_EXT_CONFIG="+cfgJSON,
	)
	if agentID != "" {
		env = append(env, "KOJO_EXT_AGENT_ID="+agentID)
	}
	// Manifest-declared variables come last but may not override the
	// contract above: a package must not be able to point its own
	// token or API base somewhere else.
	reserved := map[string]bool{}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			reserved[k] = true
		}
	}
	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// A name is checked for shape before it is compared against
		// the reserved set: "KOJO_EXT_TOKEN=x" would otherwise pass
		// the comparison and still land as KOJO_EXT_TOKEN in the
		// child's environment. Manifest validation rejects these at
		// install time; this is the same rule applied to a state file
		// that was edited by hand.
		if !envNameRe.MatchString(k) {
			s.logger.Warn("extension env var ignored (invalid name)",
				"extension", row.ID, "name", k)
			continue
		}
		// The whole KOJO_ prefix is reserved, not just the names set
		// above: a variable this version does not use yet must not be
		// pre-seeded by a package either.
		if reserved[k] || strings.HasPrefix(k, "KOJO_") {
			s.logger.Warn("extension env var ignored (reserved)",
				"extension", row.ID, "name", k)
			continue
		}
		env = append(env, k+"="+svc.Env[k])
	}
	return serviceSpec{
		ExtensionID: row.ID,
		AgentID:     agentID,
		Commit:      row.Commit,
		Bin:         bin,
		Args:        append([]string(nil), svc.Args...),
		Env:         env,
	}
}

// baseServiceEnv is the allowlisted slice of kojo's own environment
// that every service inherits.
func baseServiceEnv() []string {
	var out []string
	for _, k := range []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "TMPDIR",
		"SystemRoot", "USERPROFILE", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA", "PATHEXT", "ComSpec"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// DataDir is the per-extension (optionally per-agent) writable
// directory handed to a service as KOJO_EXT_DATA_DIR. It lives outside
// the checkout so an update, which replaces the checkout wholesale,
// cannot wipe a package's state.
func (m *Manager) DataDir(id, agentID string) string {
	dir := filepath.Join(m.root, dataDirName, id)
	if agentID != "" {
		dir = filepath.Join(dir, "agents", agentID)
	}
	return dir
}
