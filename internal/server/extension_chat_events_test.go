package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

const extEventsTestTimeout = 5 * time.Second

// extEventsClient is a package's end of one events stream.
type extEventsClient struct {
	t      *testing.T
	events chan extensionChatEvent
	cancel context.CancelFunc
}

// newExtEventsHTTP serves the events route over real HTTP: the stream
// is long-lived and read while core writes to it.
func newExtEventsHTTP(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}/ext-chat/events", func(w http.ResponseWriter, r *http.Request) {
		p := auth.Principal{Role: auth.RoleExtension, ExtensionID: r.Header.Get("X-Test-Extension"), Scopes: []string{"chat:session"}}
		s.handleExtensionChatEvents(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// connectExtEvents opens a stream and returns once core has registered
// it, which the first ping announces.
func connectExtEvents(t *testing.T, srv *httptest.Server, extensionID, agentID string) *extEventsClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/agents/"+agentID+"/ext-chat/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Extension", extensionID)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != extensionChatContentType {
		t.Fatalf("events content type = %q", ct)
	}
	c := &extEventsClient{t: t, events: make(chan extensionChatEvent, 256), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		defer close(c.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			var evt extensionChatEvent
			if json.Unmarshal(sc.Bytes(), &evt) != nil {
				return
			}
			c.events <- evt
		}
	}()
	if first := c.next(); first.Type != "ping" {
		t.Fatalf("first event = %+v, want the registration ping", first)
	}
	return c
}

// next returns the next event, failing the test if the stream ends.
func (c *extEventsClient) next() extensionChatEvent {
	c.t.Helper()
	select {
	case evt, ok := <-c.events:
		if !ok {
			c.t.Fatal("events stream ended")
		}
		return evt
	case <-time.After(extEventsTestTimeout):
		c.t.Fatal("no event arrived")
	}
	return extensionChatEvent{}
}

func (c *extEventsClient) expect(typ string) extensionChatEvent {
	c.t.Helper()
	evt := c.next()
	if evt.Type != typ {
		c.t.Fatalf("event = %+v, want type %q", evt, typ)
	}
	return evt
}

// ended waits for the server to close the stream.
func (c *extEventsClient) ended() {
	c.t.Helper()
	deadline := time.After(extEventsTestTimeout)
	for {
		select {
		case _, ok := <-c.events:
			if !ok {
				return
			}
		case <-deadline:
			c.t.Fatal("events stream was not closed")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(extEventsTestTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func slackEventKey(agentID string) extensionEventKey {
	return extensionEventKey{extensionID: extensionSlackPackageID, agentID: agentID}
}

func TestExtensionEventsReservedForSlackPackage(t *testing.T) {
	s, f := newExtChatTestServer(t)
	rr := extChatCall(s.handleExtensionChatEvents, demoExtPrincipal("ag_1"), "GET", "/x", "", map[string]string{"id": "ag_1"})
	if rr.Code != http.StatusForbidden || extErrorCode(t, rr) != "forbidden" {
		t.Fatalf("events for another package: %d %s", rr.Code, rr.Body.String())
	}
	if f.currentHandler() != nil || s.extensionEvents.get(extensionEventKey{extensionID: "demo", agentID: "ag_1"}) != nil {
		t.Fatal("a refused stream was registered")
	}
}

// A background turn of a lingering session reaches the package event by
// event, under the package-relative key, and ends with background_end.
func TestExtensionEventsRelayBackgroundTurn(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")

	h := f.currentHandler()
	if h == nil || f.handlerAgent != "ag_1" {
		t.Fatal("the stream did not register as the agent's background handler")
	}

	events := make(chan agent.ChatEvent)
	cancelled := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.HandleKeyedBackgroundTurn("ag_1", "ag_1:slack:C1:1.2", events, func() { close(cancelled) })
	}()
	turn := c.expect("background_turn")
	if turn.TurnID == "" || turn.SessionKey != "C1:1.2" {
		t.Fatalf("background_turn = %+v", turn)
	}
	events <- agent.ChatEvent{Type: "text", Delta: "built"}
	events <- agent.ChatEvent{Type: "done"}
	for _, want := range []string{"text", "done"} {
		evt := c.expect("background_event")
		if evt.TurnID != turn.TurnID || evt.Event == nil || evt.Event.Type != want {
			t.Fatalf("background_event = %+v, want %s", evt, want)
		}
	}
	close(events)
	if end := c.expect("background_end"); end.TurnID != turn.TurnID {
		t.Fatalf("background_end = %+v", end)
	}
	<-returned
	select {
	case <-cancelled:
		t.Fatal("a turn that ran to its end was cancelled")
	default:
	}

	h.KeyedBackgroundTasksAbandoned("ag_1", "ag_1:slack:C1:1.2", 3, agent.KeyedUserStopReason)
	if evt := c.expect("background_abandoned"); evt.SessionKey != "C1:1.2" || evt.Pending != 3 || evt.Reason != agent.KeyedUserStopReason {
		t.Fatalf("background_abandoned = %+v", evt)
	}

	// The slot is per agent: a keyed session of another surface can be
	// handed to this handler. It is cancelled and drained, not relayed.
	foreign := make(chan agent.ChatEvent, 1)
	foreign <- agent.ChatEvent{Type: "text"}
	close(foreign)
	foreignCancelled := false
	h.HandleKeyedBackgroundTurn("ag_1", "ag_1:ext.demo:k", foreign, func() { foreignCancelled = true })
	if _, open := <-foreign; open || !foreignCancelled {
		t.Fatalf("foreign session: drained=%v cancelled=%v", !open, foreignCancelled)
	}
	h.KeyedBackgroundTasksAbandoned("ag_1", "ag_1:ext.demo:k", 1, "x")

	// Closing the stream gives the slot back.
	c.cancel()
	waitFor(t, "the handler to unregister", func() bool { return f.currentHandler() == nil })
	if s.extensionEvents.get(slackEventKey("ag_1")) != nil {
		t.Fatal("a closed stream stayed in the hub")
	}
}

// If the package goes away mid-turn the turn is cancelled and its
// events are still read to the end: the producer must never block on a
// handler that stopped listening.
func TestExtensionEventsDisconnectCancelsAndDrains(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	h := f.currentHandler()

	events := make(chan agent.ChatEvent)
	cancelled := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.HandleKeyedBackgroundTurn("ag_1", "ag_1:slack:C1:1.2", events, func() { close(cancelled) })
	}()
	c.expect("background_turn")
	events <- agent.ChatEvent{Type: "text", Delta: "partial"}
	c.expect("background_event")

	c.cancel()
	select {
	case <-cancelled:
	case <-time.After(extEventsTestTimeout):
		t.Fatal("the turn was not cancelled when the stream closed")
	}
	// Unbuffered sends only complete if the handler keeps reading.
	for i := 0; i < 3; i++ {
		select {
		case events <- agent.ChatEvent{Type: "text"}:
		case <-time.After(extEventsTestTimeout):
			t.Fatal("the handler stopped reading before the channel closed")
		}
	}
	select {
	case <-returned:
		t.Fatal("the handler returned before the channel closed")
	default:
	}
	close(events)
	select {
	case <-returned:
	case <-time.After(extEventsTestTimeout):
		t.Fatal("the handler did not return after the channel closed")
	}

	// A handler whose stream is already gone, called with no cancel func.
	late := make(chan agent.ChatEvent, 1)
	late <- agent.ChatEvent{Type: "text"}
	close(late)
	h.HandleKeyedBackgroundTurn("ag_1", "ag_1:slack:C1:1.2", late, nil)
	if _, open := <-late; open {
		t.Fatal("events were left unread")
	}
}

// background-cancel stops a relayed turn by ID.
func TestExtensionEventsBackgroundCancel(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	h := f.currentHandler()
	pv := map[string]string{"id": "ag_1"}

	events := make(chan agent.ChatEvent)
	cancelled := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.HandleKeyedBackgroundTurn("ag_1", "ag_1:slack:C1:1.2", events, func() { close(cancelled) })
	}()
	turn := c.expect("background_turn")

	rr := extChatCall(s.handleExtensionChatBackgroundCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"turnId":"nope"}`, pv)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cancelled":false`) {
		t.Fatalf("cancel of an unknown turn: %d %s", rr.Code, rr.Body.String())
	}
	// The turn belongs to the Slack package's stream, not to demo's.
	rr = extChatCall(s.handleExtensionChatBackgroundCancel, demoExtPrincipal("ag_1"), "POST", "/x", `{"turnId":"`+turn.TurnID+`"}`, pv)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cancelled":false`) {
		t.Fatalf("cancel from another package: %d %s", rr.Code, rr.Body.String())
	}
	rr = extChatCall(s.handleExtensionChatBackgroundCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"turnId":""}`, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("cancel without a turn: %d", rr.Code)
	}
	rr = extChatCall(s.handleExtensionChatBackgroundCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"turnId":"`+turn.TurnID+`"}`, pv)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(extEventsTestTimeout):
		t.Fatal("background-cancel did not cancel the turn")
	}
	// The backend answers a cancel with a terminal event; it is relayed.
	events <- agent.ChatEvent{Type: "done"}
	close(events)
	c.expect("background_event")
	c.expect("background_end")
	<-returned
	rr = extChatCall(s.handleExtensionChatBackgroundCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"turnId":"`+turn.TurnID+`"}`, pv)
	if !strings.Contains(rr.Body.String(), `"cancelled":false`) {
		t.Fatalf("cancel of a finished turn: %s", rr.Body.String())
	}
}

// One stream per (package, agent): a reconnect replaces the older
// stream, which is closed, and takes over the handler slot.
func TestExtensionEventsReconnectReplaces(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	first := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	firstHandler := f.currentHandler()
	other := connectExtEvents(t, srv, extensionSlackPackageID, "ag_2")

	second := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	first.ended()
	h := f.currentHandler()
	if h == nil || h == firstHandler {
		t.Fatal("the new stream did not take the handler slot")
	}

	// The old stream's late unregister must not clear the new handler.
	waitFor(t, "the old stream to unregister", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.unregistered >= 1
	})
	if f.currentHandler() != h {
		t.Fatal("the old stream's unregister removed the new handler")
	}

	if !s.resumeGoalOnExtensionStream("ag_1", "ag_1:slack:C1:1.2", "U1", "!goal resume-if th 1") {
		t.Fatal("goal resume was not accepted")
	}
	if evt := second.expect("goal_resume"); evt.SessionKey != "C1:1.2" {
		t.Fatalf("goal_resume = %+v", evt)
	}
	// The other agent's stream is untouched and saw none of it.
	if !s.resumeGoalOnExtensionStream("ag_2", "ag_2:slack:C9:", "U2", "!goal resume-if th 2") {
		t.Fatal("goal resume for the other agent was not accepted")
	}
	if evt := other.expect("goal_resume"); evt.SessionKey != "C9:" || evt.UserID != "U2" {
		t.Fatalf("other agent's goal_resume = %+v", evt)
	}
}

// The stream outlives every request deadline, and http.Server.Shutdown
// waits for requests instead of ending them: the hub has to close the
// streams or the daemon's shutdown waits out its whole deadline.
func TestExtensionEventsShutdownEndsStreams(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	first := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	second := connectExtEvents(t, srv, extensionSlackPackageID, "ag_2")

	s.extensionEvents.shutdown()
	first.ended()
	second.ended()
	// A package that reconnects before the listener is gone is turned
	// away with an error, so it backs off rather than spinning.
	req, err := http.NewRequest("GET", srv.URL+"/api/v1/agents/ag_1/ext-chat/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Extension", extensionSlackPackageID)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("events status after shutdown = %d, want 503", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), extEventsTestTimeout)
	defer cancel()
	if err := srv.Config.Shutdown(ctx); err != nil {
		t.Fatalf("the listener did not drain after the streams were closed: %v", err)
	}
	if s.extensionEvents.get(slackEventKey("ag_1")) != nil || s.extensionEvents.get(slackEventKey("ag_2")) != nil {
		t.Fatal("a closed stream is still registered")
	}
	waitFor(t, "both streams to unregister", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.unregistered >= 2
	})

	// A package that reconnects while the daemon is going down gets a
	// stream that is already over, and no handler slot.
	before := f.currentHandler()
	st := s.extensionEvents.attach(slackEventKey("ag_1"), func(*extensionEventStream) func() {
		t.Error("a stream attached after shutdown was registered")
		return func() {}
	})
	select {
	case <-st.done:
	default:
		t.Fatal("a stream attached after shutdown is open")
	}
	if s.extensionEvents.get(slackEventKey("ag_1")) != nil || f.currentHandler() != before {
		t.Fatal("a stream attached after shutdown was kept")
	}
}

// Goal recovery reaches the package through its event stream.
func TestExtensionEventsGoalResume(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	req := goalRecoveryRequest{AgentID: "ag_1", SessionKey: "ag_1:slack:C1:1.2", UserID: "U123", ThreadID: "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b", Generation: 4}

	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	if err := s.resumeGoalSurface(context.Background(), req); err != nil {
		t.Fatalf("resume over the stream = %v", err)
	}
	evt := c.expect("goal_resume")
	if evt.SessionKey != "C1:1.2" || evt.UserID != "U123" || evt.Command != "!goal resume-if 019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b 4" {
		t.Fatalf("goal_resume = %+v", evt)
	}
	if q, err := agent.ParseGoalCommand(evt.Command); err != nil || q == nil || q.ExpectedGeneration == nil {
		t.Fatalf("the relayed command does not parse as a guarded resume: %v %+v", err, q)
	}

	// A stream for one agent does not answer for another.
	req.AgentID, req.SessionKey = "ag_2", "ag_2:slack:C1:1.2"
	if err := s.resumeGoalSurface(context.Background(), req); err == nil {
		t.Fatal("resume for an agent without a stream succeeded")
	}
	// Nor does a key outside the namespace reach the stream.
	if s.resumeGoalOnExtensionStream("ag_1", "ag_1:ext.demo:k", "U1", "x") {
		t.Fatal("a foreign key was resumed on the slack stream")
	}
}

func newExtHandoffReservation(s *Server, id string) *extensionHandoffReservation {
	return &extensionHandoffReservation{hub: &s.extensionEvents, key: slackEventKey("ag_1"), id: id}
}

func ackExtHandoff(t *testing.T, s *Server, requestID, errText string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(extensionChatHandoffAckRequest{RequestID: requestID, Error: errText})
	return extChatCall(s.handleExtensionChatHandoffAck, slackExtPrincipal("ag_1"), "POST", "/x", string(body), map[string]string{"id": "ag_1"})
}

// handoff_activate and handoff_wait_source are requests: core waits for
// the package's acknowledgement and takes its error.
func TestExtensionHandoffActivateAndAck(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	r := newExtHandoffReservation(s, "res-1")

	result := make(chan error, 1)
	go func() { result <- r.WaitSourceComplete(context.Background()) }()
	wait := c.expect("handoff_wait_source")
	if wait.RequestID == "" || wait.ReservationID != "res-1" {
		t.Fatalf("handoff_wait_source = %+v", wait)
	}
	if rr := ackExtHandoff(t, s, wait.RequestID, "source turn failed"); rr.Code != http.StatusOK {
		t.Fatalf("ack = %d %s", rr.Code, rr.Body.String())
	}
	if err := <-result; err == nil || err.Error() != "source turn failed" {
		t.Fatalf("WaitSourceComplete = %v", err)
	}

	// A refused activation leaves the reservation usable: core retries.
	go func() { result <- r.Activate(context.Background(), "arrived", "peer-2") }()
	act := c.expect("handoff_activate")
	if act.ReservationID != "res-1" || act.Prompt != "arrived" || act.ExpectedHolder != "peer-2" || act.RequestID == wait.RequestID {
		t.Fatalf("handoff_activate = %+v", act)
	}
	ackExtHandoff(t, s, act.RequestID, "busy")
	if err := <-result; err == nil || err.Error() != "busy" {
		t.Fatalf("refused Activate = %v", err)
	}

	go func() { result <- r.Activate(context.Background(), "arrived", "peer-2") }()
	act = c.expect("handoff_activate")
	// An acknowledgement only counts from the package that owns the stream.
	body := `{"requestId":"` + act.RequestID + `"}`
	if rr := extChatCall(s.handleExtensionChatHandoffAck, demoExtPrincipal("ag_1"), "POST", "/x", body, map[string]string{"id": "ag_1"}); rr.Code != http.StatusNotFound {
		t.Fatalf("ack from another package = %d", rr.Code)
	}
	if rr := ackExtHandoff(t, s, act.RequestID, ""); rr.Code != http.StatusOK {
		t.Fatalf("ack = %d %s", rr.Code, rr.Body.String())
	}
	if err := <-result; err != nil {
		t.Fatalf("Activate = %v", err)
	}
	// Answered once: a repeat finds nothing waiting.
	if rr := ackExtHandoff(t, s, act.RequestID, ""); rr.Code != http.StatusNotFound || extErrorCode(t, rr) != "not_found" {
		t.Fatalf("second ack = %d %s", rr.Code, rr.Body.String())
	}
	if rr := ackExtHandoff(t, s, "", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("ack without requestId = %d", rr.Code)
	}

	// Activated: a retry succeeds without asking again, and the slot is
	// now the package's to finish, so Release says nothing.
	if err := r.Activate(context.Background(), "arrived", "peer-2"); err != nil {
		t.Fatalf("repeated Activate = %v", err)
	}
	r.Release()
	s.resumeGoalOnExtensionStream("ag_1", "ag_1:slack:C1:1.2", "U1", "marker")
	if evt := c.next(); evt.Type != "goal_resume" {
		t.Fatalf("after an activated reservation's Release: %+v", evt)
	}
}

func TestExtensionHandoffFailsWithoutAnAnswer(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)

	// No stream at all.
	r := newExtHandoffReservation(s, "res-1")
	if err := r.Activate(context.Background(), "p", "h"); !errors.Is(err, errExtensionEventStreamAbsent) {
		t.Fatalf("Activate without a stream = %v", err)
	}
	if err := r.WaitSourceComplete(context.Background()); !errors.Is(err, errExtensionEventStreamAbsent) {
		t.Fatalf("WaitSourceComplete without a stream = %v", err)
	}

	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")

	// A caller that has already given up sends nothing: the next event
	// the package sees is the request after it.
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	if err := r.Activate(gone, "never sent", "h"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Activate with a cancelled context = %v", err)
	}

	// The caller's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := r.Activate(ctx, "p", "h")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Activate past its context = %v", err)
	}
	stale := c.expect("handoff_activate")
	if stale.Prompt != "p" {
		t.Fatalf("a request from a cancelled caller was sent: %+v", stale)
	}

	// The reservation's own cap, for a caller without a deadline.
	r.timeout = 30 * time.Millisecond
	if err := r.WaitSourceComplete(context.Background()); !errors.Is(err, errExtensionEventAckTimeout) {
		t.Fatalf("unanswered WaitSourceComplete = %v", err)
	}
	c.expect("handoff_wait_source")
	// An answer that arrives after core gave up is refused, so the
	// package does not go on to run an arrival core has written off.
	if rr := ackExtHandoff(t, s, stale.RequestID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("late ack = %d", rr.Code)
	}

	// The stream closing mid-wait fails the request at once.
	r.timeout = time.Minute
	result := make(chan error, 1)
	go func() { result <- r.Activate(context.Background(), "p", "h") }()
	c.expect("handoff_activate")
	c.cancel()
	select {
	case err := <-result:
		if !errors.Is(err, errExtensionEventStreamClosed) {
			t.Fatalf("Activate across a disconnect = %v", err)
		}
	case <-time.After(extEventsTestTimeout):
		t.Fatal("Activate kept waiting on a closed stream")
	}
}

func TestExtensionHandoffRelease(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")

	// Unused: the package is told once, and the slot cannot be
	// activated afterwards.
	r := newExtHandoffReservation(s, "res-1")
	r.Release()
	r.Release()
	if evt := c.expect("handoff_release"); evt.ReservationID != "res-1" {
		t.Fatalf("handoff_release = %+v", evt)
	}
	if err := r.Activate(context.Background(), "p", "h"); !errors.Is(err, errExtensionHandoffReleased) {
		t.Fatalf("Activate after Release = %v", err)
	}

	// Released while an activation is in flight: the outcome decides.
	// Refused, the release goes out afterwards.
	r = newExtHandoffReservation(s, "res-2")
	result := make(chan error, 1)
	go func() { result <- r.Activate(context.Background(), "p", "h") }()
	act := c.expect("handoff_activate")
	r.Release()
	ackExtHandoff(t, s, act.RequestID, "no")
	if err := <-result; err == nil {
		t.Fatal("refused Activate succeeded")
	}
	if evt := c.expect("handoff_release"); evt.ReservationID != "res-2" {
		t.Fatalf("deferred handoff_release = %+v", evt)
	}

	// Accepted, it is dropped: the package is running the arrival.
	r = newExtHandoffReservation(s, "res-3")
	go func() { result <- r.Activate(context.Background(), "p", "h") }()
	act = c.expect("handoff_activate")
	r.Release()
	ackExtHandoff(t, s, act.RequestID, "")
	if err := <-result; err != nil {
		t.Fatalf("Activate = %v", err)
	}
	s.resumeGoalOnExtensionStream("ag_1", "ag_1:slack:C1:1.2", "U1", "marker")
	if evt := c.next(); evt.Type != "goal_resume" {
		t.Fatalf("after an accepted activation: %+v", evt)
	}
}

// The ext-chat route wires the reservation to the turn: usable while
// the turn streams, released when it ends unused.
func TestExtensionChatHandoffReservationLifecycle(t *testing.T) {
	s, f := newExtChatTestServer(t)
	srv := newExtEventsHTTP(t, s)
	c := connectExtEvents(t, srv, extensionSlackPackageID, "ag_1")
	f.chatEvents = make(chan agent.ChatEvent)
	f.chatStarted = make(chan struct{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", "/x",
			`{"sessionKey":"C1:1.2","message":"hi","handoffReservationId":"res-9"}`, map[string]string{"id": "ag_1"})
	}()
	<-f.chatStarted
	f.mu.Lock()
	reservation := f.chatOpts.HandoffArrivalReservation
	f.mu.Unlock()

	// Core would mint a capability for exactly this value.
	if got := s.mintHandoffArrivalCapability("ag_1", "ag_1:slack:C1:1.2", reservation); got == "" {
		t.Fatal("core refused to mint a capability for the reservation")
	}
	result := make(chan error, 1)
	go func() { result <- reservation.(goalSourceBarrier).WaitSourceComplete(context.Background()) }()
	wait := c.expect("handoff_wait_source")
	if wait.ReservationID != "res-9" {
		t.Fatalf("handoff_wait_source = %+v", wait)
	}
	ackExtHandoff(t, s, wait.RequestID, "")
	if err := <-result; err != nil {
		t.Fatalf("WaitSourceComplete = %v", err)
	}

	close(f.chatEvents)
	<-done
	if evt := c.expect("handoff_release"); evt.ReservationID != "res-9" {
		t.Fatalf("handoff_release = %+v", evt)
	}
}

// A package that stops reading costs core a bounded wait, once: the
// stream is closed and every later send fails immediately.
func TestExtensionEventStreamNeverBlocksCore(t *testing.T) {
	var hub extensionEventHub
	st := hub.attach(slackEventKey("ag_1"), nil)
	st.sendTimeout = 20 * time.Millisecond
	for i := 0; i < extensionEventBuffer; i++ {
		if !st.send(extensionChatEvent{Type: "ping"}) {
			t.Fatalf("send %d into an empty buffer failed", i)
		}
	}
	start := time.Now()
	if st.send(extensionChatEvent{Type: "ping"}) {
		t.Fatal("send into a full, unread stream succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("send blocked for %v", elapsed)
	}
	select {
	case <-st.done:
	default:
		t.Fatal("a stalled stream was left open")
	}
	if hub.get(slackEventKey("ag_1")) != nil {
		t.Fatal("a stalled stream stayed in the hub")
	}
	start = time.Now()
	if st.send(extensionChatEvent{Type: "ping"}) || time.Since(start) > 10*time.Millisecond*50 {
		t.Fatal("send on a closed stream did not fail fast")
	}
	// Closing twice, and detaching after a replacement, are harmless.
	next := hub.attach(slackEventKey("ag_1"), nil)
	st.close()
	if hub.get(slackEventKey("ag_1")) != next {
		t.Fatal("closing an old stream removed its replacement")
	}
}
