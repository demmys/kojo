package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/extpkg"
)

func extChatCall(h http.HandlerFunc, p auth.Principal, method, target, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func demoExtPrincipal(agents ...string) auth.Principal {
	return auth.Principal{Role: auth.RoleExtension, ExtensionID: "demo", Scopes: []string{"chat:session"}, AgentScope: agents}
}

func TestExtensionSessionKeyIsNamespaced(t *testing.T) {
	got := extensionSessionKey("ag_1", "discord", "c1:t2")
	if got != "ag_1:ext.discord:c1:t2" {
		t.Fatalf("session key = %q", got)
	}
	// A package cannot spell its way into the Slack bot's key space:
	// whatever suffix it picks sits behind its own "ext.<id>:" tag.
	if strings.HasPrefix(extensionSessionKey("ag_1", "discord", "slack:C1:1.2"), "ag_1:slack:") {
		t.Fatal("extension key collides with the slack namespace")
	}
	// The first-party Slack package is the exception: it takes over the
	// keys the built-in bot derived, trailing colon included.
	for suffix, want := range map[string]string{
		"C1:1700000000.000100": "ag_1:slack:C1:1700000000.000100",
		"D9:":                  "ag_1:slack:D9:",
	} {
		got := extensionSessionKey("ag_1", extensionSlackPackageID, suffix)
		if got != want {
			t.Errorf("slack key for %q = %q, want %q", suffix, got, want)
		}
		if back, ok := extensionSessionSuffix("ag_1", extensionSlackPackageID, got); !ok || back != suffix {
			t.Errorf("suffix of %q = %q, %v", got, back, ok)
		}
	}
	// Only the exact ID qualifies.
	for _, id := range []string{"Slack", "slack2", "slack-bot", "ext.slack"} {
		if strings.HasPrefix(extensionSessionKey("ag_1", id, "C1:1.2"), "ag_1:slack:") {
			t.Errorf("package %q reached the slack namespace", id)
		}
	}
	if _, ok := extensionSessionSuffix("ag_1", extensionSlackPackageID, "ag_1:ext.demo:k"); ok {
		t.Error("a foreign key was accepted as a slack session")
	}
	if _, ok := extensionSessionSuffix("ag_1", "demo", "ag_2:ext.demo:k"); ok {
		t.Error("another agent's key was accepted")
	}
	for _, bad := range []string{"", ":x", "a b", "a/b", "../x", strings.Repeat("a", 161)} {
		if extensionSessionKeyRe.MatchString(bad) {
			t.Errorf("session key %q accepted", bad)
		}
	}
}

func TestExtensionChatRejectsNonExtensionsAndBadInput(t *testing.T) {
	s := newExtTestServer(t)
	pv := map[string]string{"id": "ag_1"}

	// The Owner has no extension ID to namespace a session with.
	rr := extChatCall(s.handleExtensionChat, auth.Principal{Role: auth.RoleOwner}, "POST", "/api/v1/agents/ag_1/ext-chat", `{"sessionKey":"k","message":"hi"}`, pv)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("owner call = %d", rr.Code)
	}
	// No router on this fixture (PeerOnly shape): unavailable, not a panic.
	rr = extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat", `{"sessionKey":"k","message":"hi"}`, pv)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-router call = %d", rr.Code)
	}

	s.externalChat = newExternalChatRouter(s)
	for name, body := range map[string]string{
		"bad key":       `{"sessionKey":"a b","message":"hi"}`,
		"empty message": `{"sessionKey":"k","message":"  "}`,
		"unknown field": `{"sessionKey":"k","message":"hi","originPeerId":"p"}`,
		"outside path":  `{"sessionKey":"k","message":"hi","attachments":[{"path":"/etc/passwd"}]}`,
	} {
		rr := extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat", body, pv)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s", name, rr.Code, rr.Body.String())
		}
	}
	rr = extChatCall(s.handleExtensionChatSteer, demoExtPrincipal("ag_1"), "POST", "/api/v1/agents/ag_1/ext-chat/steer", `{"sessionKey":"k","content":""}`, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty steer = %d", rr.Code)
	}
}

func TestExtensionChatSessionsAreExclusive(t *testing.T) {
	var e extensionChatSessions
	cancelled := 0
	if !e.acquire("k", func() { cancelled++ }) || e.acquire("k", nil) {
		t.Fatal("a session key must be held by one turn at a time")
	}
	if !e.acquire("other", nil) {
		t.Fatal("unrelated keys must not block each other")
	}
	// Cancelling stops the turn but leaves the key held: the turn is
	// still winding down.
	if !e.cancel("k") || cancelled != 1 || e.acquire("k", nil) {
		t.Fatalf("cancel: cancelled=%d", cancelled)
	}
	if e.cancel("idle") {
		t.Fatal("cancel reported a turn for an idle key")
	}
	e.release("k")
	if !e.acquire("k", nil) {
		t.Fatal("release did not free the key")
	}
}

// fakeExtChatBackend records what the extension chat routes ask of the
// external chat router.
type fakeExtChatBackend struct {
	mu sync.Mutex

	chatMessage string
	chatOpts    agent.OneShotOpts
	chatCtx     context.Context
	chatEvents  chan agent.ChatEvent
	chatErr     error
	chatStarted chan struct{}

	steerKey, steerContent, steerUser string
	steerAsUser                       bool
	answerKey                         string
	answer                            agent.QuestionAnswer
	stopKey                           string
	goalKey, goalUser                 string
	goalHandled                       bool
	err                               error

	handlerAgent string
	handler      agent.KeyedBackgroundHandler
	unregistered int
}

func (f *fakeExtChatBackend) ChatOneShot(ctx context.Context, agentID, message string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatMessage, f.chatOpts, f.chatCtx = message, opts, ctx
	if f.chatStarted != nil {
		close(f.chatStarted)
	}
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	if f.chatEvents == nil {
		ch := make(chan agent.ChatEvent)
		close(ch)
		return ch, nil
	}
	return f.chatEvents, nil
}

func (f *fakeExtChatBackend) SteerOneShot(ctx context.Context, agentID, sessionKey, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerKey, f.steerContent, f.steerUser, f.steerAsUser = sessionKey, content, "", false
	return f.err
}

func (f *fakeExtChatBackend) SteerOneShotAsUser(ctx context.Context, agentID, sessionKey, content, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerKey, f.steerContent, f.steerUser, f.steerAsUser = sessionKey, content, userID, true
	return f.err
}

func (f *fakeExtChatBackend) AnswerOneShotQuestion(ctx context.Context, agentID, sessionKey string, answer agent.QuestionAnswer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answerKey, f.answer = sessionKey, answer
	return f.err
}

func (f *fakeExtChatBackend) StopThreadBackgroundTasks(ctx context.Context, agentID, sessionKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopKey = sessionKey
	return f.err
}

func (f *fakeExtChatBackend) StopIdleGoal(ctx context.Context, agentID, sessionKey, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.goalKey, f.goalUser = sessionKey, userID
	return f.goalHandled, f.err
}

// RegisterKeyedBackgroundHandler mirrors the manager: one slot per
// agent, and an unregister that only clears its own registration.
func (f *fakeExtChatBackend) RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlerAgent, f.handler = agentID, h
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unregistered++
		if f.handler == h {
			f.handler = nil
		}
	}
}

func (f *fakeExtChatBackend) currentHandler() agent.KeyedBackgroundHandler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handler
}

func slackExtPrincipal(agents ...string) auth.Principal {
	return auth.Principal{Role: auth.RoleExtension, ExtensionID: extensionSlackPackageID, Scopes: []string{"chat:session"}, AgentScope: agents}
}

func newExtChatTestServer(t *testing.T) (*Server, *fakeExtChatBackend) {
	t.Helper()
	s := newExtTestServer(t)
	f := &fakeExtChatBackend{}
	s.extensionChatBackend = f
	return s, f
}

func extErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error envelope: %s", rr.Body.String())
	}
	return body.Error.Code
}

// The goal, handoff, question and lingering options act on machinery
// that is keyed on the Slack namespace and single-slot per agent. Any
// other package asking for them is refused before a turn starts.
func TestExtensionChatReservedOptions(t *testing.T) {
	s, f := newExtChatTestServer(t)
	pv := map[string]string{"id": "ag_1"}
	const target = "/api/v1/agents/ag_1/ext-chat"

	for field, body := range map[string]string{
		"goal":                  `{"sessionKey":"k","message":"hi","goal":{"action":"pause"}}`,
		"goalUserId":            `{"sessionKey":"k","message":"hi","goalUserId":"U1"}`,
		"interactiveQuestions":  `{"sessionKey":"k","message":"hi","interactiveQuestions":true}`,
		"lingerBackgroundTasks": `{"sessionKey":"k","message":"hi","lingerBackgroundTasks":true}`,
		"forceFreshSession":     `{"sessionKey":"k","message":"hi","forceFreshSession":true}`,
		"expectedHolderPeer":    `{"sessionKey":"k","message":"hi","expectedHolderPeer":"peer-1"}`,
		"handoffReservationId":  `{"sessionKey":"k","message":"hi","handoffReservationId":"r1"}`,
	} {
		rr := extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", target, body, pv)
		if rr.Code != http.StatusForbidden || extErrorCode(t, rr) != "forbidden" || !strings.Contains(rr.Body.String(), field) {
			t.Errorf("%s from another package: %d %s", field, rr.Code, rr.Body.String())
		}
	}
	if f.chatMessage != "" {
		t.Fatal("a refused request reached the router")
	}

	// preserveTerminalOnCancel is open to every package and lands in
	// that package's own namespace.
	rr := extChatCall(s.handleExtensionChat, demoExtPrincipal("ag_1"), "POST", target,
		`{"sessionKey":"k","message":"hi","preserveTerminalOnCancel":true}`, pv)
	if rr.Code != http.StatusOK || !f.chatOpts.PreserveTerminalOnCancel || f.chatOpts.SessionKey != "ag_1:ext.demo:k" {
		t.Fatalf("plain package turn: %d %s opts=%+v", rr.Code, rr.Body.String(), f.chatOpts)
	}
	if f.chatOpts.Goal != nil || f.chatOpts.HandoffArrivalReservation != nil || f.chatOpts.LingerBackgroundTasks {
		t.Fatalf("plain package turn carried reserved options: %+v", f.chatOpts)
	}

	// The Slack package gets every option, mapped onto the turn.
	rr = extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", target, `{
		"sessionKey":"C1:1.2","message":"go",
		"goal":{"action":"set","objective":"ship it"},"goalUserId":"U123",
		"interactiveQuestions":true,"preserveTerminalOnCancel":true,"lingerBackgroundTasks":true,
		"forceFreshSession":true,"expectedHolderPeer":"peer-1","handoffReservationId":"r1"}`, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("slack turn: %d %s", rr.Code, rr.Body.String())
	}
	o := f.chatOpts
	if o.SessionKey != "ag_1:slack:C1:1.2" || o.Goal == nil || o.Goal.Objective != "ship it" || o.GoalUserID != "U123" ||
		!o.InteractiveQuestions || !o.PreserveTerminalOnCancel || !o.LingerBackgroundTasks ||
		!o.ForceFreshSession || o.ExpectedHolderPeer != "peer-1" || !o.DisableKojoAttachmentInstructions {
		t.Fatalf("slack turn opts = %+v", o)
	}
	if missingHandoffArrivalReservation(o.HandoffArrivalReservation) {
		t.Fatal("handoffReservationId did not produce a usable reservation")
	}
	if _, ok := o.HandoffArrivalReservation.(goalSourceBarrier); !ok {
		t.Fatal("the reservation is not a goal source barrier")
	}

	// A goal command needs no message; a user ID that goal recovery
	// would refuse later is refused now.
	rr = extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", target,
		`{"sessionKey":"C1:1.2","message":"","goal":{"action":"pause"},"goalUserId":"U123"}`, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("goal without message: %d %s", rr.Code, rr.Body.String())
	}
	for name, body := range map[string]string{
		"lowercase user":  `{"sessionKey":"C1:1.2","message":"hi","goalUserId":"u123"}`,
		"bad reservation": `{"sessionKey":"C1:1.2","message":"hi","handoffReservationId":"a b"}`,
	} {
		rr = extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", target, body, pv)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}

	// A goal command from someone other than the goal's owner.
	f.chatErr = agent.ErrGoalOwnerForbidden
	rr = extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", target,
		`{"sessionKey":"C1:1.2","message":"","goal":{"action":"clear"},"goalUserId":"U999"}`, pv)
	if rr.Code != http.StatusForbidden || extErrorCode(t, rr) != "goal_owner_forbidden" {
		t.Fatalf("foreign goal command: %d %s", rr.Code, rr.Body.String())
	}
}

func TestExtensionChatControlRoutes(t *testing.T) {
	s, f := newExtChatTestServer(t)
	pv := map[string]string{"id": "ag_1"}
	slack := slackExtPrincipal("ag_1")
	const key = "ag_1:slack:C1:1.2"

	// steer: with a user it is attributed, without it is not.
	rr := extChatCall(s.handleExtensionChatSteer, slack, "POST", "/x", `{"sessionKey":"C1:1.2","content":"more","userId":"U1"}`, pv)
	if rr.Code != http.StatusOK || !f.steerAsUser || f.steerUser != "U1" || f.steerKey != key || f.steerContent != "more" {
		t.Fatalf("steer as user: %d %+v", rr.Code, f)
	}
	rr = extChatCall(s.handleExtensionChatSteer, demoExtPrincipal("ag_1"), "POST", "/x", `{"sessionKey":"k","content":"more"}`, pv)
	if rr.Code != http.StatusOK || f.steerAsUser || f.steerKey != "ag_1:ext.demo:k" {
		t.Fatalf("plain steer: %d %+v", rr.Code, f)
	}

	// question
	rr = extChatCall(s.handleExtensionChatQuestion, slack, "POST", "/x",
		`{"sessionKey":"C1:1.2","answer":{"requestId":"q1","answers":{"Which?":"A"}}}`, pv)
	if rr.Code != http.StatusOK || f.answerKey != key || f.answer.RequestID != "q1" || f.answer.Answers["Which?"] != "A" {
		t.Fatalf("question: %d %s %+v", rr.Code, rr.Body.String(), f.answer)
	}
	rr = extChatCall(s.handleExtensionChatQuestion, slack, "POST", "/x", `{"sessionKey":"C1:1.2","answer":{}}`, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("question without requestId: %d", rr.Code)
	}

	// background-stop
	rr = extChatCall(s.handleExtensionChatBackgroundStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2"}`, pv)
	if rr.Code != http.StatusOK || f.stopKey != key {
		t.Fatalf("background-stop: %d %s", rr.Code, rr.Body.String())
	}

	// goal-stop
	for _, handled := range []bool{true, false} {
		f.goalHandled = handled
		rr = extChatCall(s.handleExtensionChatGoalStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2","userId":"U1"}`, pv)
		want := `{"handled":false}`
		if handled {
			want = `{"handled":true}`
		}
		if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != want || f.goalKey != key || f.goalUser != "U1" {
			t.Fatalf("goal-stop handled=%v: %d %s", handled, rr.Code, rr.Body.String())
		}
	}
	rr = extChatCall(s.handleExtensionChatGoalStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2","userId":"not a user"}`, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("goal-stop bad user: %d", rr.Code)
	}

	// Every sentinel has one code, whichever route meets it.
	routes := map[string]struct {
		h    http.HandlerFunc
		body string
	}{
		"steer":           {s.handleExtensionChatSteer, `{"sessionKey":"C1:1.2","content":"x","userId":"U1"}`},
		"question":        {s.handleExtensionChatQuestion, `{"sessionKey":"C1:1.2","answer":{"requestId":"q1"}}`},
		"background-stop": {s.handleExtensionChatBackgroundStop, `{"sessionKey":"C1:1.2"}`},
	}
	for name, c := range map[string]struct {
		route  string
		err    error
		status int
		code   string
	}{
		"steer idle":          {"steer", agent.ErrAgentNotBusy, 409, "not_busy"},
		"steer unsupported":   {"steer", agent.ErrSteerUnsupported, 409, "steer_unsupported"},
		"steer uncertain":     {"steer", agent.ErrSteerDeliveryUncertain, 502, "steer_uncertain"},
		"steer not the owner": {"steer", agent.ErrGoalOwnerForbidden, 403, "goal_owner_forbidden"},
		"steer foreign turn":  {"steer", agent.ErrSteerOriginForbidden, 403, "forbidden"},
		"steer other":         {"steer", errors.New("boom"), 502, "steer_failed"},
		"question gone":       {"question", agent.ErrQuestionNotFound, 404, "question_not_found"},
		"question idle":       {"question", agent.ErrAgentNotBusy, 409, "not_busy"},
		"question invalid":    {"question", agent.ErrInvalidQuestionAnswer, 400, "invalid_answers"},
		"question other":      {"question", errors.New("boom"), 502, "question_failed"},
		"nothing in bg":       {"background-stop", agent.ErrBackgroundSessionNotFound, 404, "background_not_found"},
		"bg stop other":       {"background-stop", errors.New("boom"), 502, "background_stop_failed"},
		"no such agent":       {"background-stop", agent.ErrAgentNotFound, 404, "not_found"},
	} {
		// Wrapped, as the router returns them from a remote holder.
		f.err = errors.Join(errors.New("holder said"), c.err)
		rr := extChatCall(routes[c.route].h, slack, "POST", "/x", routes[c.route].body, pv)
		if rr.Code != c.status || extErrorCode(t, rr) != c.code {
			t.Errorf("%s: %d %s, want %d %s", name, rr.Code, rr.Body.String(), c.status, c.code)
		}
	}

	// goal-stop keeps "handled" when the stop is recorded but the holder
	// has not confirmed it, and reports the rest as errors.
	f.goalHandled, f.err = true, errors.New("holder unreachable")
	rr = extChatCall(s.handleExtensionChatGoalStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2","userId":"U1"}`, pv)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"handled":true`) || !strings.Contains(rr.Body.String(), `"error":"holder unreachable"`) {
		t.Fatalf("goal-stop handled with error: %d %s", rr.Code, rr.Body.String())
	}
	f.goalHandled = false
	rr = extChatCall(s.handleExtensionChatGoalStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2","userId":"U1"}`, pv)
	if rr.Code != http.StatusBadGateway || extErrorCode(t, rr) != "goal_stop_failed" {
		t.Fatalf("goal-stop failure: %d %s", rr.Code, rr.Body.String())
	}
	f.err = agent.ErrGoalOwnerForbidden
	rr = extChatCall(s.handleExtensionChatGoalStop, slack, "POST", "/x", `{"sessionKey":"C1:1.2","userId":"U2"}`, pv)
	if rr.Code != http.StatusForbidden || extErrorCode(t, rr) != "goal_owner_forbidden" {
		t.Fatalf("goal-stop by another user: %d %s", rr.Code, rr.Body.String())
	}
}

// Every extension chat route derives its session namespace from the
// token's package ID, so a principal without one is refused, and a
// host without a router answers unavailable instead of panicking.
func TestExtensionChatRoutesRefuseNonExtensions(t *testing.T) {
	s, _ := newExtChatTestServer(t)
	bare := newExtTestServer(t)
	pv := map[string]string{"id": "ag_1"}
	for name, c := range map[string]struct {
		method string
		h, off http.HandlerFunc
		body   string
	}{
		"steer":             {"POST", s.handleExtensionChatSteer, bare.handleExtensionChatSteer, `{"sessionKey":"k","content":"x"}`},
		"cancel":            {"POST", s.handleExtensionChatCancel, bare.handleExtensionChatCancel, `{"sessionKey":"k"}`},
		"question":          {"POST", s.handleExtensionChatQuestion, bare.handleExtensionChatQuestion, `{"sessionKey":"k","answer":{"requestId":"q"}}`},
		"goal-stop":         {"POST", s.handleExtensionChatGoalStop, bare.handleExtensionChatGoalStop, `{"sessionKey":"k"}`},
		"background-stop":   {"POST", s.handleExtensionChatBackgroundStop, bare.handleExtensionChatBackgroundStop, `{"sessionKey":"k"}`},
		"background-cancel": {"POST", s.handleExtensionChatBackgroundCancel, bare.handleExtensionChatBackgroundCancel, `{"turnId":"t"}`},
		"handoff-ack":       {"POST", s.handleExtensionChatHandoffAck, bare.handleExtensionChatHandoffAck, `{"requestId":"r"}`},
		"events":            {"GET", s.handleExtensionChatEvents, bare.handleExtensionChatEvents, ``},
	} {
		for role, p := range map[string]auth.Principal{
			"owner": {Role: auth.RoleOwner},
			"agent": {Role: auth.RoleAgent, AgentID: "ag_1"},
		} {
			rr := extChatCall(c.h, p, c.method, "/x", c.body, pv)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s as %s = %d %s", name, role, rr.Code, rr.Body.String())
			}
		}
		rr := extChatCall(c.off, slackExtPrincipal("ag_1"), c.method, "/x", c.body, pv)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without a router = %d %s", name, rr.Code, rr.Body.String())
		}
		if c.method == "POST" {
			rr := extChatCall(c.h, slackExtPrincipal("ag_1"), c.method, "/x", `{"sessionKey":"k","nope":1}`, pv)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s with an unknown field = %d %s", name, rr.Code, rr.Body.String())
			}
		}
	}
}

// ext-chat/cancel stops the turn but keeps its stream, so the terminal
// event the backend still produces reaches the package. A disconnect
// could not deliver it.
func TestExtensionChatCancelKeepsTheStream(t *testing.T) {
	s, f := newExtChatTestServer(t)
	f.chatEvents = make(chan agent.ChatEvent)
	f.chatStarted = make(chan struct{})
	pv := map[string]string{"id": "ag_1"}

	rr := extChatCall(s.handleExtensionChatCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"sessionKey":"C1:1.2"}`, pv)
	if rr.Code != http.StatusConflict || extErrorCode(t, rr) != "not_busy" {
		t.Fatalf("cancel with no turn: %d %s", rr.Code, rr.Body.String())
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- extChatCall(s.handleExtensionChat, slackExtPrincipal("ag_1"), "POST", "/x",
			`{"sessionKey":"C1:1.2","message":"hi","preserveTerminalOnCancel":true}`, pv)
	}()
	<-f.chatStarted
	f.mu.Lock()
	turnCtx := f.chatCtx
	f.mu.Unlock()

	// Another package's cancel of the same suffix is a different session.
	rr = extChatCall(s.handleExtensionChatCancel, demoExtPrincipal("ag_1"), "POST", "/x", `{"sessionKey":"C1:1.2"}`, pv)
	if rr.Code != http.StatusConflict || turnCtx.Err() != nil {
		t.Fatalf("foreign cancel: %d ctx=%v", rr.Code, turnCtx.Err())
	}
	rr = extChatCall(s.handleExtensionChatCancel, slackExtPrincipal("ag_1"), "POST", "/x", `{"sessionKey":"C1:1.2"}`, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-turnCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the turn")
	}
	f.chatEvents <- agent.ChatEvent{Type: "done", BackgroundTasksPending: 2}
	close(f.chatEvents)
	select {
	case rr := <-done:
		if !strings.Contains(rr.Body.String(), `"type":"done"`) {
			t.Fatalf("terminal event missing from a cancelled turn: %s", rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end")
	}
	if !s.extensionChats.acquire("ag_1:slack:C1:1.2", nil) {
		t.Fatal("session key still held after the turn ended")
	}
}

type extTestSecrets map[string]string

func (m extTestSecrets) GetToken(p, a, s, k string) (string, error) {
	v, ok := m[a+"/"+s+"/"+k]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}
func (m extTestSecrets) SetToken(p, a, s, k, v string, _ time.Time) error {
	m[a+"/"+s+"/"+k] = v
	return nil
}
func (m extTestSecrets) DeleteToken(p, a, s, k string) error       { delete(m, a+"/"+s+"/"+k); return nil }
func (m extTestSecrets) DeleteTokensBySource(p, a, s string) error { return nil }

func TestExtensionSecretsRoute(t *testing.T) {
	s := newExtTestServer(t)
	s.extensions.SetSecretStore(extTestSecrets{})
	dir := t.TempDir()
	writeExtTestFiles(t, dir, map[string]string{
		extpkg.ManifestFilename: `{"id":"demo","name":"Demo","version":"1.0.0",
			"contributes":{"skills":["skills/demo"],"settings":{"scope":"per-agent","schema":"s.json"}}}`,
		"skills/demo/SKILL.md": "# demo\n",
		"s.json":               `{"type":"object","properties":{"botToken":{"type":"string","format":"password"}}}`,
	})
	runExtTestGit(t, dir, "init", "-q", "-b", "main")
	commitExtTestRepo(t, dir)
	if _, err := s.extensions.Install(context.Background(), extpkg.InstallRequest{URL: dir, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// The owner-facing binding route accepts the secret and hands back
	// only its name.
	rr := extCall(t, s.handlePutExtensionAgentBinding, "PUT", "/api/v1/extensions/demo/agents/ag_1",
		`{"enabled":true,"config":{"botToken":"hunter2"}}`, map[string]string{"id": "demo", "agentId": "ag_1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("binding = %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "hunter2") || !strings.Contains(rr.Body.String(), `"secrets":["botToken"]`) {
		t.Fatalf("binding response = %s", rr.Body.String())
	}
	if rr := extCall(t, s.handleListExtensions, "GET", "/api/v1/extensions", "", nil); strings.Contains(rr.Body.String(), "hunter2") {
		t.Fatal("list leaked the secret")
	}

	rr = extChatCall(s.handleExtensionSecrets, demoExtPrincipal("ag_1"), "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"botToken":"hunter2"`) {
		t.Fatalf("own secrets = %d %s", rr.Code, rr.Body.String())
	}
	// Bound to ag_2 only: ag_1's secrets are out of reach.
	rr = extChatCall(s.handleExtensionSecrets, demoExtPrincipal("ag_2"), "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unbound agent secrets = %d", rr.Code)
	}
	// Another package's token resolves to that package, which has none.
	other := demoExtPrincipal("ag_1")
	other.ExtensionID = "other"
	rr = extChatCall(s.handleExtensionSecrets, other, "GET", "/api/v1/extension/secrets?agentId=ag_1", "", nil)
	if rr.Code == http.StatusOK {
		t.Fatalf("foreign package read secrets: %s", rr.Body.String())
	}
	rr = extChatCall(s.handleExtensionSecrets, auth.Principal{Role: auth.RoleOwner}, "GET", "/api/v1/extension/secrets", "", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("owner on the self route = %d", rr.Code)
	}
}
