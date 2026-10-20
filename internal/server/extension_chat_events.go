package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
)

// The events stream is the half of extension chat that core starts.
//
// A turn the package asks for answers on that request's own stream.
// Three things have no such request to answer on: a background task
// finishing in a lingering session, a goal that has to be resumed
// after a restart or a device move, and a device handoff that needs
// the arrival turn run in the conversation it left. A package runs in
// another process, so it holds one long-lived GET open per agent and
// core writes all three to it as NDJSON events.
//
// Nothing here may block core for long. Every write into a stream is
// bounded, and a stream that does not keep up is closed rather than
// waited on: the package reconnects and core carries on.

const (
	// extensionEventBuffer absorbs the burst of a streaming background
	// turn between two writes to the package.
	extensionEventBuffer = 64
	// extensionEventSendTimeout bounds how long core waits for room in
	// a stream. The writer gives up on a single write after
	// extensionChatWriteTimeout, so a buffer that stays full for that
	// long belongs to a connection that is already gone.
	extensionEventSendTimeout = extensionChatWriteTimeout
	// extensionHandoffAckTimeout caps the wait for a package to answer a
	// handoff request when the caller's context carries no deadline of
	// its own. The callers in core use 3–15 seconds.
	extensionHandoffAckTimeout = 30 * time.Second
	// extensionEventErrorLimit bounds the text core accepts back in an
	// acknowledgement's error.
	extensionEventErrorLimit = 1024
)

var (
	errExtensionEventStreamAbsent = errors.New("the extension's event stream is not connected")
	errExtensionEventStreamClosed = errors.New("the extension's event stream closed before it answered")
	errExtensionEventAckTimeout   = errors.New("the extension did not answer in time")
	errExtensionHandoffReleased   = errors.New("the arrival reservation was released")
)

// extensionChatEvent is one line of the events stream. Type selects
// which of the other fields are present.
type extensionChatEvent struct {
	Type string `json:"type"`

	// background_turn, background_event, background_end
	TurnID string           `json:"turnId,omitempty"`
	Event  *agent.ChatEvent `json:"event,omitempty"`

	// background_turn, background_abandoned, goal_resume. Always the
	// package-relative key, as everywhere else on the wire.
	SessionKey string `json:"sessionKey,omitempty"`

	// background_abandoned
	Pending int    `json:"pending,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// goal_resume
	UserID  string `json:"userId,omitempty"`
	Command string `json:"command,omitempty"`

	// handoff_activate, handoff_wait_source, handoff_release
	RequestID      string `json:"requestId,omitempty"`
	ReservationID  string `json:"reservationId,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	ExpectedHolder string `json:"expectedHolder,omitempty"`
}

type extensionEventKey struct {
	extensionID string
	agentID     string
}

// extensionEventHub holds the connected stream of each (package, agent)
// pair. The zero value is ready to use.
type extensionEventHub struct {
	// attachMu orders whole attachments (replace the stream, close the
	// old one, register the new one's handler). Without it two
	// reconnects racing each other could leave the older stream's
	// handler registered last, and then removed by its own close.
	attachMu sync.Mutex

	mu      sync.Mutex
	streams map[extensionEventKey]*extensionEventStream
	// stopped is set by shutdown; from then on attach hands back a
	// stream that is already closed.
	stopped bool
}

// attach makes a new stream the current one for key. A package that
// reconnects has given up on its previous connection, so the older
// stream is closed: its handler returns and its pending work fails
// over instead of waiting on a socket nobody reads.
//
// register, if not nil, installs whatever should live as long as the
// stream and returns its removal, which runs when the stream closes.
func (h *extensionEventHub) attach(key extensionEventKey, register func(*extensionEventStream) func()) *extensionEventStream {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	st := &extensionEventStream{
		hub:         h,
		key:         key,
		out:         make(chan extensionChatEvent, extensionEventBuffer),
		done:        make(chan struct{}),
		sendTimeout: extensionEventSendTimeout,
	}
	h.mu.Lock()
	if h.streams == nil {
		h.streams = map[extensionEventKey]*extensionEventStream{}
	}
	if h.stopped {
		h.mu.Unlock()
		st.close()
		return st
	}
	old := h.streams[key]
	h.streams[key] = st
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
	if register != nil {
		st.onClose(register(st))
	}
	return st
}

func (h *extensionEventHub) get(key extensionEventKey) *extensionEventStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[key]
}

// shutdown closes every connected stream and refuses new ones. The
// events request never ends by itself, and http.Server.Shutdown waits
// for requests rather than ending them, so the daemon has to close
// these before it drains its listeners.
func (h *extensionEventHub) shutdown() {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	h.mu.Lock()
	h.stopped = true
	streams := make([]*extensionEventStream, 0, len(h.streams))
	for _, st := range h.streams {
		streams = append(streams, st)
	}
	h.mu.Unlock()
	for _, st := range streams {
		st.close()
	}
}

// detach forgets st unless a newer stream has already replaced it.
func (h *extensionEventHub) detach(st *extensionEventStream) {
	h.mu.Lock()
	if h.streams[st.key] == st {
		delete(h.streams, st.key)
	}
	h.mu.Unlock()
}

// extensionEventStream is one connected events request.
type extensionEventStream struct {
	hub         *extensionEventHub
	key         extensionEventKey
	out         chan extensionChatEvent
	done        chan struct{}
	sendTimeout time.Duration

	closeOnce sync.Once

	mu         sync.Mutex
	closed     bool
	unregister func()
	turns      map[string]context.CancelCauseFunc
	acks       map[string]chan error
}

// close ends the stream. Everything waiting on it — a background relay,
// a handoff request — watches done and gives up.
func (st *extensionEventStream) close() {
	st.closeOnce.Do(func() {
		close(st.done)
		st.hub.detach(st)
		st.mu.Lock()
		st.closed = true
		unregister := st.unregister
		st.unregister = nil
		st.mu.Unlock()
		if unregister != nil {
			unregister()
		}
	})
}

// onClose arranges for fn to run when the stream closes, or right away
// if it already has.
func (st *extensionEventStream) onClose(fn func()) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		fn()
		return
	}
	st.unregister = fn
	st.mu.Unlock()
}

// send queues evt for the package and reports whether it was queued.
// It never waits past sendTimeout: a stream that stays full that long
// is closed, which is what lets the callers in core treat a false
// return as "the package is gone".
func (st *extensionEventStream) send(evt extensionChatEvent) bool {
	return st.sendContext(context.Background(), evt)
}

// sendContext is send for a caller with a deadline of its own: it also
// gives up, without closing the stream, when ctx ends.
func (st *extensionEventStream) sendContext(ctx context.Context, evt extensionChatEvent) bool {
	select {
	case <-st.done:
		return false
	case <-ctx.Done():
		// Nothing is queued for a caller that has already given up.
		return false
	default:
	}
	select {
	case st.out <- evt:
		return true
	default:
	}
	timer := time.NewTimer(st.sendTimeout)
	defer timer.Stop()
	select {
	case st.out <- evt:
		return true
	case <-st.done:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		st.close()
		return false
	}
}

func (st *extensionEventStream) addTurn(turnID string, cancel context.CancelCauseFunc) {
	st.mu.Lock()
	if st.turns == nil {
		st.turns = map[string]context.CancelCauseFunc{}
	}
	st.turns[turnID] = cancel
	st.mu.Unlock()
}

func (st *extensionEventStream) removeTurn(turnID string) {
	st.mu.Lock()
	delete(st.turns, turnID)
	st.mu.Unlock()
}

// cancelTurn stops a background turn the stream is relaying and reports
// whether it was still running.
func (st *extensionEventStream) cancelTurn(turnID string, cause error) bool {
	st.mu.Lock()
	cancel, ok := st.turns[turnID]
	st.mu.Unlock()
	if ok {
		cancel(cause)
	}
	return ok
}

// request sends evt and waits for the package to acknowledge it through
// ext-chat/handoff-ack. It fails as soon as the stream closes, the
// caller's context ends, or timeout passes.
func (st *extensionEventStream) request(parent context.Context, evt extensionChatEvent, timeout time.Duration) error {
	requestID := newExtensionEventID()
	ack := make(chan error, 1)
	st.mu.Lock()
	if st.acks == nil {
		st.acks = map[string]chan error{}
	}
	st.acks[requestID] = ack
	st.mu.Unlock()
	// abandon gives up the wait with err, unless the package's answer
	// got in first. acknowledge has then already told the package its
	// answer counted, so that answer is the result: the two sides must
	// never disagree on whether a handoff was accepted.
	abandon := func(err error) error {
		st.mu.Lock()
		_, waiting := st.acks[requestID]
		delete(st.acks, requestID)
		st.mu.Unlock()
		if !waiting {
			return <-ack
		}
		return err
	}

	// One deadline covers queueing the request and waiting for its
	// answer, so a congested stream cannot stretch the caller's wait.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	evt.RequestID = requestID
	if !st.sendContext(ctx, evt) {
		if err := ctx.Err(); err != nil {
			return abandon(extensionRequestContextError(err, parent))
		}
		return abandon(errExtensionEventStreamClosed)
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return abandon(extensionRequestContextError(ctx.Err(), parent))
	case <-st.done:
		return abandon(errExtensionEventStreamClosed)
	}
}

// extensionRequestContextError tells the caller's own context ending
// apart from the request's cap running out.
func extensionRequestContextError(err error, parent context.Context) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errExtensionEventAckTimeout
	}
	return err
}

// acknowledge resolves a pending request and reports whether there was
// one. A second acknowledgement of the same request finds nothing.
func (st *extensionEventStream) acknowledge(requestID string, err error) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	ack, ok := st.acks[requestID]
	if ok {
		// Removed and answered under the lock, so the waiter either sees
		// the entry and gives up or finds the answer already buffered.
		delete(st.acks, requestID)
		ack <- err
	}
	return ok
}

func newExtensionEventID() string {
	raw := make([]byte, 16)
	// crypto/rand does not fail on the platforms kojo supports; the IDs
	// only have to be unique within one stream.
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// extensionBackgroundRelay is the keyed background handler a connected
// stream registers for its agent. It forwards an unsolicited turn of a
// lingering session — a run_in_background task reporting back — to the
// package, which posts it into the conversation the session belongs to.
type extensionBackgroundRelay struct {
	stream *extensionEventStream
}

// HandleKeyedBackgroundTurn relays one turn. The handler contract is to
// read events until the channel closes whatever happens, so every exit
// drains; an exit before the end also cancels the turn, because a turn
// whose output reaches nobody should not keep running.
func (h *extensionBackgroundRelay) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel context.CancelCauseFunc) {
	defer func() {
		for range events {
		}
	}()
	stop := func(cause error) {
		if cancel != nil {
			cancel(cause)
		}
	}
	lost := func() { stop(agent.KojoStop(extensionStopReasonDisconnect)) }
	st := h.stream
	suffix, ok := extensionSessionSuffix(agentID, st.key.extensionID, sessionKey)
	if !ok || agentID != st.key.agentID {
		// Not this package's session: the handler slot is per agent, not
		// per namespace, so a keyed session of another surface can land
		// here. The package has nowhere to post it.
		stop(agent.KojoStop(extensionStopReasonNoOutput))
		return
	}
	turnID := newExtensionEventID()
	st.addTurn(turnID, stop)
	defer st.removeTurn(turnID)
	if !st.send(extensionChatEvent{Type: "background_turn", TurnID: turnID, SessionKey: suffix}) {
		lost()
		return
	}
	for {
		select {
		case evt, open := <-events:
			if !open {
				st.send(extensionChatEvent{Type: "background_end", TurnID: turnID})
				return
			}
			if !st.send(extensionChatEvent{Type: "background_event", TurnID: turnID, Event: &evt}) {
				lost()
				return
			}
		case <-st.done:
			lost()
			return
		}
	}
}

// KeyedBackgroundTasksAbandoned tells the package that a lingering
// session ended with tasks still running. Best effort: it is only a
// notice for the package to post.
func (h *extensionBackgroundRelay) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	st := h.stream
	suffix, ok := extensionSessionSuffix(agentID, st.key.extensionID, sessionKey)
	if !ok || agentID != st.key.agentID || pending <= 0 {
		return
	}
	st.send(extensionChatEvent{Type: "background_abandoned", SessionKey: suffix, Pending: pending, Reason: reason})
}

// resumeGoalOnExtensionStream hands a guarded goal resume command to
// the first-party Slack package's stream for the agent. It reports
// whether a stream took it.
func (s *Server) resumeGoalOnExtensionStream(agentID, sessionKey, userID, command string) bool {
	suffix, ok := extensionSessionSuffix(agentID, extensionSlackPackageID, sessionKey)
	if !ok {
		return false
	}
	st := s.extensionEvents.get(extensionEventKey{extensionID: extensionSlackPackageID, agentID: agentID})
	if st == nil {
		return false
	}
	return st.send(extensionChatEvent{Type: "goal_resume", SessionKey: suffix, UserID: userID, Command: command})
}

// extensionHandoffReservation stands in, inside core, for a slot the
// package reserved in its own conversation queue behind a turn. Each
// call core makes on it becomes a request on the events stream.
//
// It looks the stream up at call time rather than holding one, so a
// package that reconnected mid-turn still gets the request.
type extensionHandoffReservation struct {
	hub     *extensionEventHub
	key     extensionEventKey
	id      string
	timeout time.Duration // zero means extensionHandoffAckTimeout

	mu             sync.Mutex
	activating     int
	activated      bool
	released       bool
	releasePending bool
}

var (
	_ agent.HandoffArrivalReservation = (*extensionHandoffReservation)(nil)
	_ goalSourceBarrier               = (*extensionHandoffReservation)(nil)
)

func (r *extensionHandoffReservation) request(ctx context.Context, evt extensionChatEvent) error {
	st := r.hub.get(r.key)
	if st == nil {
		return errExtensionEventStreamAbsent
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = extensionHandoffAckTimeout
	}
	evt.ReservationID = r.id
	return st.request(ctx, evt, timeout)
}

// Activate asks the package to run the arrival turn in the reserved
// slot and waits for it to accept. Core retries a failed delivery with
// the same reservation, so the package must treat a repeated
// handoff_activate for one reservationId as the same activation.
func (r *extensionHandoffReservation) Activate(ctx context.Context, prompt, expectedHolder string) error {
	if r == nil {
		return errors.New("extension arrival reservation is missing")
	}
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return errExtensionHandoffReleased
	}
	if r.activated {
		r.mu.Unlock()
		return nil
	}
	r.activating++
	r.mu.Unlock()

	// The lock is not held across the wait: Release runs from the turn's
	// handler and must not stall behind a package that is slow to answer.
	err := r.request(ctx, extensionChatEvent{Type: "handoff_activate", Prompt: prompt, ExpectedHolder: expectedHolder})

	r.mu.Lock()
	r.activating--
	if err == nil {
		r.activated = true
	}
	release := r.activating == 0 && r.releasePending && !r.activated
	if release {
		r.releasePending = false
		r.released = true
	}
	r.mu.Unlock()
	if release {
		r.sendRelease()
	}
	return err
}

// WaitSourceComplete blocks until the package reports that the turn the
// slot was reserved behind has finished delivering — the checkpoint a
// goal handoff waits for before the goal moves.
func (r *extensionHandoffReservation) WaitSourceComplete(ctx context.Context) error {
	if r == nil {
		return errors.New("extension arrival reservation is missing")
	}
	return r.request(ctx, extensionChatEvent{Type: "handoff_wait_source"})
}

// Release tells the package the slot will not be used. It does nothing
// once the reservation is activated, and waits out an activation that
// is still in flight: whether to release depends on how that ends.
func (r *extensionHandoffReservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.released || r.activated {
		r.mu.Unlock()
		return
	}
	if r.activating > 0 {
		r.releasePending = true
		r.mu.Unlock()
		return
	}
	r.released = true
	r.mu.Unlock()
	r.sendRelease()
}

// sendRelease is best effort. A package that is not connected lost the
// slot along with everything else it held in memory.
func (r *extensionHandoffReservation) sendRelease() {
	if st := r.hub.get(r.key); st != nil {
		st.send(extensionChatEvent{Type: "handoff_release", ReservationID: r.id})
	}
}

// handleExtensionChatEvents serves the events stream.
func (s *Server) handleExtensionChatEvents(w http.ResponseWriter, r *http.Request) {
	p, backend, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	if !extensionOwnsSlackNamespace(p.ExtensionID) {
		// Everything the stream carries is addressed by agent, not by
		// session namespace: one background handler and one goal resume
		// surface per agent. See extensionSlackPackageID.
		writeError(w, http.StatusForbidden, "forbidden", "the events stream is reserved for the first-party Slack package")
		return
	}
	agentID := r.PathValue("id")
	register := func(st *extensionEventStream) func() {
		return backend.RegisterKeyedBackgroundHandler(agentID, &extensionBackgroundRelay{stream: st})
	}
	st := s.extensionEvents.attach(extensionEventKey{extensionID: p.ExtensionID, agentID: agentID}, register)
	defer st.close()
	select {
	case <-st.done:
		// The daemon is shutting down. An error status makes the
		// package back off instead of reconnecting at once.
		writeError(w, http.StatusServiceUnavailable, "unavailable", "kojo is shutting down")
		return
	default:
	}

	w.Header().Set("Content-Type", extensionChatContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	write := func(v any) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(extensionChatWriteTimeout))
		err := enc.Encode(v)
		if err == nil {
			err = controller.Flush()
		}
		_ = controller.SetWriteDeadline(time.Time{})
		return err == nil
	}
	// The first ping is the package's signal that the stream is
	// registered and core-initiated events can now reach it.
	if !write(extensionChatEvent{Type: "ping"}) {
		return
	}
	heartbeat := time.NewTicker(extensionChatHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case evt := <-st.out:
			if !write(evt) {
				return
			}
		case <-heartbeat.C:
			if !write(extensionChatEvent{Type: "ping"}) {
				return
			}
		case <-r.Context().Done():
			return
		case <-st.done:
			return
		}
	}
}

type extensionChatBackgroundCancelRequest struct {
	TurnID string `json:"turnId"`
	// ByUser marks an explicit stop by a person (Slack `!stop`).
	ByUser bool `json:"byUser,omitempty"`
}

// handleExtensionChatBackgroundCancel stops a background turn the
// events stream is relaying (the package's "stop" for a turn it did
// not start). Losing the race with the turn's own end is not an error.
func (s *Server) handleExtensionChatBackgroundCancel(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	var req extensionChatBackgroundCancelRequest
	if !decodeExtensionChatBody(w, r, &req, nil) {
		return
	}
	if req.TurnID == "" || len(req.TurnID) > 128 {
		writeError(w, http.StatusBadRequest, "bad_request", "turnId is required")
		return
	}
	cancelled := false
	if st := s.extensionEvents.get(extensionEventKey{extensionID: p.ExtensionID, agentID: r.PathValue("id")}); st != nil {
		cancelled = st.cancelTurn(req.TurnID, extensionStopCause(req.ByUser))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "cancelled": cancelled})
}

type extensionChatHandoffAckRequest struct {
	RequestID string `json:"requestId"`
	Error     string `json:"error,omitempty"`
}

// handleExtensionChatHandoffAck resolves a handoff_activate or
// handoff_wait_source request. A non-empty error is the package's
// refusal and becomes the error core's caller sees.
func (s *Server) handleExtensionChatHandoffAck(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.extensionChatControl(w, r)
	if !ok {
		return
	}
	var req extensionChatHandoffAckRequest
	if !decodeExtensionChatBody(w, r, &req, nil) {
		return
	}
	if req.RequestID == "" || len(req.RequestID) > 128 || len(req.Error) > extensionEventErrorLimit {
		writeError(w, http.StatusBadRequest, "bad_request", "requestId is required and error is bounded")
		return
	}
	var result error
	if req.Error != "" {
		result = errors.New(req.Error)
	}
	st := s.extensionEvents.get(extensionEventKey{extensionID: p.ExtensionID, agentID: r.PathValue("id")})
	if st == nil || !st.acknowledge(req.RequestID, result) {
		// Core stopped waiting (timeout, reconnect) or never asked. The
		// package must not assume the handoff went through.
		writeError(w, http.StatusNotFound, "not_found", "no request is waiting for this acknowledgement")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}
