package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func cancelledWith(cause error) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	return ctx
}

func TestStopCauseOfClassification(t *testing.T) {
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	expiredCause, cancelExpiredCause := context.WithTimeoutCause(context.Background(), time.Nanosecond, KojoStop("制限時間 5 分超過"))
	defer cancelExpiredCause()
	<-expired.Done()
	<-expiredCause.Done()
	child, cancelChild := context.WithCancel(cancelledWith(UserStop()))
	defer cancelChild()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		ok     bool
		byUser bool
		reason string
	}{
		{"user", cancelledWith(UserStop()), true, true, ""},
		{"kojo reason", cancelledWith(KojoStop(stopReasonReset)), true, false, stopReasonReset},
		{"kojo without reason", cancelledWith(KojoStop("")), true, false, stopReasonInternal},
		{"wrapped cause", cancelledWith(errors.Join(errors.New("x"), KojoStop(stopReasonFork))), true, false, stopReasonFork},
		{"agent requested", cancelledWith(AgentRequestedStop()), false, false, ""},
		{"noted by caller", cancelledWith(ErrStopNotedByCaller), false, false, ""},
		{"plain cancel defaults to kojo", cancelledWith(nil), true, false, stopReasonInternal},
		{"untyped cause defaults to kojo", cancelledWith(errors.New("boom")), true, false, stopReasonInternal},
		{"deadline is a kojo timeout", expired, true, false, stopReasonTimeout},
		{"deadline with cause", expiredCause, true, false, "制限時間 5 分超過"},
		{"child inherits parent cause", child, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := stopCauseOf(tc.ctx)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (cause %v)", ok, tc.ok, c)
			}
			if !ok {
				return
			}
			if c.ByUser != tc.byUser || (!tc.byUser && c.Reason != tc.reason) {
				t.Fatalf("cause = %+v, want byUser=%v reason=%q", c, tc.byUser, tc.reason)
			}
		})
	}
}

func TestTurnStopCause(t *testing.T) {
	alive := context.Background()
	if _, ok := turnStopCause(alive, turnEnd{terminal: true}); ok {
		t.Fatal("a normal done is not a stop")
	}
	if _, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: "API error"}); ok {
		t.Fatal("a backend error is not a stop")
	}
	if _, ok := turnStopCause(alive, turnEnd{}); ok {
		t.Fatal("a stream ending on its own is not a stop")
	}
	if c, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: ErrMsgCancelled}); !ok || c.ByUser || c.Reason != stopReasonRemote {
		t.Fatalf("cancelled terminal with a live context = %+v, %v", c, ok)
	}
	if c, ok := turnStopCause(alive, turnEnd{terminal: true, errMsg: "codex turn interrupted"}); !ok || c.ByUser || c.Reason != stopReasonCodex {
		t.Fatalf("codex interrupt = %+v, %v", c, ok)
	}
	if c, ok := turnStopCause(cancelledWith(UserStop()), turnEnd{}); !ok || !c.ByUser {
		t.Fatalf("user-cancelled turn = %+v, %v", c, ok)
	}
	// A terminal that arrived while the context was alive wins over a later
	// cancel (the turn had already finished).
	if _, ok := turnStopCause(cancelledWith(UserStop()), turnEnd{terminal: true}); ok {
		t.Fatal("a turn that finished before the cancel is not a stop")
	}
}

func TestStopNoteText(t *testing.T) {
	if got, want := stopNoteText(&StopCause{ByUser: true}), "[kojo] 直前のターンはユーザーの明示的な操作で停止されました。"; got != want {
		t.Fatalf("user note = %q, want %q", got, want)
	}
	want := "[kojo] 直前のターンはユーザーではなく kojo が停止しました（理由: エージェントのリセット）。会話ログに \"interrupted by user\" 等とあってもユーザーの操作ではありません。"
	if got := stopNoteText(&StopCause{Reason: stopReasonReset}); got != want {
		t.Fatalf("kojo note = %q, want %q", got, want)
	}
}

func TestCronRunContextTimeoutCause(t *testing.T) {
	if c, ok := stopCauseOf(cancelledWith(cronTimeoutCause(15 * time.Minute))); !ok || c.ByUser || c.Reason != "制限時間 15 分超過" {
		t.Fatalf("cron timeout cause = %+v, %v", c, ok)
	}
	ctx, cancel, timeout := cronRunContext(1)
	defer cancel()
	if dl, ok := ctx.Deadline(); !ok || timeout != time.Minute || time.Until(dl) > time.Minute {
		t.Fatalf("cron deadline = %v, %v, %v", dl, ok, timeout)
	}
	// Cancelling before the deadline (normal end) is not a timeout stop:
	// the turn ended first, so it records a terminal.
	cancel()
	if _, ok := turnStopCause(ctx, turnEnd{terminal: true}); ok {
		t.Fatal("finished cron turn reported as stopped")
	}
}

func TestWithoutStopNotesAndCommitRewritten(t *testing.T) {
	old := stopNoteText(&StopCause{Reason: stopReasonReset})
	text := "[kojo] other note\n" + old
	if got := withoutStopNotes(text); got != "[kojo] other note" {
		t.Fatalf("withoutStopNotes = %q", got)
	}
	if got := withoutLines("a\nb\na\nc", "a\nc"); got != "b\na" {
		t.Fatalf("withoutLines = %q", got)
	}
}

// stopNoteBackend records each turn's message and context. A blocking turn
// runs until its context is cancelled; otherwise it finishes at once.
type stopNoteBackend struct {
	msgs  chan string
	ctxs  chan context.Context
	block chan bool
}

func newStopNoteBackend() *stopNoteBackend {
	return &stopNoteBackend{msgs: make(chan string, 16), ctxs: make(chan context.Context, 16), block: make(chan bool, 16)}
}

func (b *stopNoteBackend) Name() string    { return "stopnote-test" }
func (b *stopNoteBackend) Available() bool { return true }
func (b *stopNoteBackend) Chat(ctx context.Context, _ *Agent, msg, _ string, _ ChatOptions) (<-chan ChatEvent, error) {
	block := false
	select {
	case block = <-b.block:
	default:
	}
	b.msgs <- msg
	b.ctxs <- ctx
	out := make(chan ChatEvent, 1)
	go func() {
		defer close(out)
		if block {
			<-ctx.Done()
			return
		}
		m := newAssistantMessage()
		m.Content = "ok"
		out <- ChatEvent{Type: "done", Message: m}
	}()
	return out, nil
}

func newStopNoteManager(t *testing.T, dir string) (*Manager, *stopNoteBackend, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	b := newStopNoteBackend()
	const id = "ag_stopnote"
	a, ok := m.Get(id)
	if !ok {
		a = &Agent{ID: id, Name: "StopNote", Tool: b.Name()}
		if err := m.store.Upsert(a); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	m.mu.Lock()
	m.agents[id] = &Agent{ID: id, Name: "StopNote", Tool: b.Name()}
	m.backends[b.Name()] = b
	m.mu.Unlock()
	_ = a
	return m, b, id
}

func waitIdle(t *testing.T, m *Manager, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitChatIdle(ctx, id); err != nil {
		t.Fatalf("WaitChatIdle: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.busyMu.Lock()
		active := m.summarizing
		m.busyMu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("summary did not stop")
		}
		time.Sleep(time.Millisecond)
	}
}

func drainEvents(ch <-chan ChatEvent) {
	for range ch {
	}
}

func mainTurn(t *testing.T, m *Manager, b *stopNoteBackend, id string, block bool) (string, context.Context, <-chan ChatEvent) {
	t.Helper()
	b.block <- block
	events, err := m.Chat(context.Background(), id, "hello", "user", nil, BusySourceUser)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return <-b.msgs, <-b.ctxs, events
}

// The WebUI stop button: the main turn is a user stop, the external turns it
// takes down with it are kojo stops. The main note is delivered once.
func TestAbortByUserNotesAndOneTimeDelivery(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k1"

	_, mainCtx, mainEvents := mainTurn(t, m, b, id, true)
	b.block <- true
	osEvents, err := m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	<-b.msgs
	osCtx := <-b.ctxs

	m.AbortByUser(id)
	drainEvents(mainEvents)
	drainEvents(osEvents)
	waitIdle(t, m, id)

	if c, ok := stopCauseOf(mainCtx); !ok || !c.ByUser {
		t.Fatalf("main turn cause = %+v, %v", c, ok)
	}
	if c, ok := stopCauseOf(osCtx); !ok || c.ByUser || c.Reason != stopReasonMainChatAbort {
		t.Fatalf("one-shot cause = %+v, %v", c, ok)
	}
	userNote := stopNoteText(&StopCause{ByUser: true})
	if got := m.PendingMainStopNote(id); got != userNote {
		t.Fatalf("main note = %q, want %q", got, userNote)
	}
	if got := m.peekKeyedNote(id, key); got != stopNoteText(&StopCause{Reason: stopReasonMainChatAbort}) {
		t.Fatalf("keyed note = %q", got)
	}

	// Next main turn carries the note once.
	msg, _, events := mainTurn(t, m, b, id, false)
	drainEvents(events)
	waitIdle(t, m, id)
	if !strings.Contains(msg, userNote) {
		t.Fatalf("next turn message lacks the note: %q", msg)
	}
	if strings.HasPrefix(msg, userNote) && strings.Contains(msg, "<context>") {
		t.Fatalf("note must sit inside the context block: %q", msg)
	}
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("note not consumed: %q", got)
	}
	msg, _, events = mainTurn(t, m, b, id, false)
	drainEvents(events)
	waitIdle(t, m, id)
	if strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("note delivered twice: %q", msg)
	}

	// Next keyed turn carries its note once, too.
	osEvents, err = m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	msg = <-b.msgs
	<-b.ctxs
	drainEvents(osEvents)
	if !strings.Contains(msg, stopNoteText(&StopCause{Reason: stopReasonMainChatAbort})) {
		t.Fatalf("keyed turn lacks the note: %q", msg)
	}
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("keyed note not consumed: %q", got)
	}
	osEvents, err = m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatalf("ChatOneShot: %v", err)
	}
	msg = <-b.msgs
	<-b.ctxs
	drainEvents(osEvents)
	if strings.Contains(msg, stopNotePrefix) {
		t.Fatalf("keyed note delivered twice: %q", msg)
	}
}

// A kojo stop of a keyed turn, a caller-noted turn, an agent-requested stop
// and a caller-provided TurnNote.
func TestKeyedTurnStopNotes(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	key := id + ":ext.test:k2"

	run := func(opts OneShotOpts, stop func()) string {
		t.Helper()
		b.block <- stop != nil
		ev, err := m.ChatOneShot(context.Background(), id, "external", opts)
		if err != nil {
			t.Fatalf("ChatOneShot: %v", err)
		}
		msg := <-b.msgs
		<-b.ctxs
		if stop != nil {
			stop()
		}
		drainEvents(ev)
		return msg
	}

	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, KojoStop(stopReasonSwitch)) })
	want := stopNoteText(&StopCause{Reason: stopReasonSwitch})
	if got := m.peekKeyedNote(id, key); got != want {
		t.Fatalf("keyed note = %q, want %q", got, want)
	}
	// A second stop before any delivery replaces the first.
	m.commitKeyedNote(id, key, want)
	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, UserStop()) })
	run(OneShotOpts{SessionKey: key + "x"}, nil) // unrelated key
	m.RecordKeyedStopCause(id, key, KojoStop(stopReasonReset))
	if got := m.peekKeyedNote(id, key); got != stopNoteText(&StopCause{Reason: stopReasonReset}) {
		t.Fatalf("latest stop must replace the undelivered one: %q", got)
	}
	m.commitKeyedNote(id, key, m.peekKeyedNote(id, key))

	// The Hub records relayed turns; the holder records none.
	run(OneShotOpts{SessionKey: key, StopNotedByCaller: true}, func() { m.CancelOneShotsForAgent(id, UserStop()) })
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("caller-noted turn left a note: %q", got)
	}
	// A stop the agent asked for leaves none.
	run(OneShotOpts{SessionKey: key}, func() { m.CancelOneShotsForAgent(id, AgentRequestedStop()) })
	if got := m.peekKeyedNote(id, key); got != "" {
		t.Fatalf("agent-requested stop left a note: %q", got)
	}
	// A caller's TurnNote is injected.
	msg := run(OneShotOpts{SessionKey: key, TurnNote: "[kojo] hub note"}, nil)
	if !strings.Contains(msg, "[kojo] hub note") {
		t.Fatalf("TurnNote missing: %q", msg)
	}
}

func TestResetGuardStopsWithCause(t *testing.T) {
	m, b, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	_, ctx, events := mainTurn(t, m, b, id, true)
	release, err := m.acquireResetGuard(id, KojoStop(stopReasonReset))
	if err != nil {
		t.Fatal(err)
	}
	drainEvents(events)
	release()
	waitIdle(t, m, id)
	if c, ok := stopCauseOf(ctx); !ok || c.Reason != stopReasonReset {
		t.Fatalf("cause = %+v, %v", c, ok)
	}
	if got := m.PendingMainStopNote(id); got != stopNoteText(&StopCause{Reason: stopReasonReset}) {
		t.Fatalf("note = %q", got)
	}
	m.dropMainStopNote(id)
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("dropped note still pending: %q", got)
	}
}

// Shutdown is the most common kojo stop: the main-chat note must survive the
// restart and reach the first turn after it.
func TestMainStopNoteSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m, b, id := newStopNoteManager(t, dir)
	_, ctx, events := mainTurn(t, m, b, id, true)
	m.schedulersStarted = true
	m.Shutdown()
	drainEvents(events)
	waitIdle(t, m, id)
	if c, ok := stopCauseOf(ctx); !ok || c.Reason != stopReasonShutdown {
		t.Fatalf("cause = %+v, %v", c, ok)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2, b2, _ := newStopNoteManager(t, dir)
	t.Cleanup(func() { _ = m2.Close() })
	want := stopNoteText(&StopCause{Reason: stopReasonShutdown})
	if got := m2.PendingMainStopNote(id); got != want {
		t.Fatalf("note after restart = %q, want %q", got, want)
	}
	msg, _, events := mainTurn(t, m2, b2, id, false)
	drainEvents(events)
	waitIdle(t, m2, id)
	if !strings.Contains(msg, want) {
		t.Fatalf("first turn after restart lacks the note: %q", msg)
	}
	if got := m2.PendingMainStopNote(id); got != "" {
		t.Fatalf("note not consumed: %q", got)
	}
}

func TestThreadStopCauses(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	var got error
	gdm.threadCancelMu.Lock()
	gdm.threadCancels["g1"] = func(c error) { got = c }
	gdm.threadCancelMu.Unlock()
	if !gdm.StopThreadTurn("g1") {
		t.Fatal("StopThreadTurn found no turn")
	}
	if c, ok := stopCauseOf(cancelledWith(got)); !ok || !c.ByUser {
		t.Fatalf("thread stop button = %+v, %v", c, ok)
	}
	gdm.cancelThreadTurn("g1")
	if c, ok := stopCauseOf(cancelledWith(got)); !ok || c.ByUser || c.Reason != keyedThreadDeletedReason {
		t.Fatalf("thread deletion = %+v, %v", c, ok)
	}
}

func TestShutdownSkipsEndedTurn(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	// A turn whose stream is over but still holds the slot (post-turn
	// indexing) gets no shutdown note.
	_, cancel := context.WithCancelCause(context.Background())
	m.busyMu.Lock()
	m.busy[id] = busyEntry{cancelCause: cancel}
	m.busyMu.Unlock()
	m.markMainTurnEnded(id)
	m.schedulersStarted = true
	m.Shutdown()
	if got := m.PendingMainStopNote(id); got != "" {
		t.Fatalf("ended turn got a shutdown note: %q", got)
	}
}

func TestKeyedStopNoteSkipsDeletedThread(t *testing.T) {
	m, _, id := newStopNoteManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Close() })
	m.RecordKeyedStopCause(id, "groupdm:g1", KojoStop(keyedThreadDeletedReason))
	if got := m.peekKeyedNote(id, "groupdm:g1"); got != "" {
		t.Fatalf("deleted thread got a note: %q", got)
	}
	m.RecordKeyedStopCause(id, "groupdm:g1", UserStop())
	if got := m.peekKeyedNote(id, "groupdm:g1"); got != stopNoteText(&StopCause{ByUser: true}) {
		t.Fatalf("user stop note = %q", got)
	}
}
