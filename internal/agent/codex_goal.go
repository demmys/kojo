package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// GoalRequest is explicit user intent, never extracted from model output or
// historical context. Controls use the same authenticated conversation route.
type GoalRequest struct {
	ExpectedRunID      string `json:"expectedRunId,omitempty"`
	OperationID        string `json:"operationId,omitempty"`
	ExpectedGeneration *int64 `json:"expectedGeneration,omitempty"`
	ExpectedThreadID   string `json:"expectedThreadId,omitempty"`
	Action             string `json:"action"`
	Objective          string `json:"objective,omitempty"`
	TokenBudget        *int64 `json:"tokenBudget,omitempty"`
}

type CodexGoal struct {
	ThreadID        string `json:"threadId"`
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// GoalBinding travels with the native thread pointer. DesiredPaused fences
// cancellation even when the CLI cannot acknowledge pause before being killed.
type GoalBinding struct {
	UserID            string     `json:"userId,omitempty"`
	RuntimeFailures   int        `json:"runtimeFailures,omitempty"`
	ActivationPending bool       `json:"activationPending,omitempty"`
	RunID             string     `json:"runId,omitempty"`
	RecoveryPending   bool       `json:"recoveryPending,omitempty"`
	RecoveryAttempts  int        `json:"recoveryAttempts,omitempty"`
	SeenOperations    []string   `json:"seenOperations,omitempty"`
	SetupContext      string     `json:"setupContext,omitempty"`
	Generation        int64      `json:"generation"`
	OriginPeerID      string     `json:"originPeerId,omitempty"`
	SessionKey        string     `json:"sessionKey"`
	DesiredPaused     bool       `json:"desiredPaused"`
	State             *CodexGoal `json:"state,omitempty"`
}

func ParseGoalCommand(text string) (*GoalRequest, error) {
	text = strings.TrimSpace(text)
	if text != "!goal" && !strings.HasPrefix(text, "!goal ") && !strings.HasPrefix(text, "!goal\n") {
		return nil, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, "!goal"))
	r := &GoalRequest{Action: "start", Objective: rest}
	switch rest {
	case "", "status":
		r.Action = "status"
		r.Objective = ""
	case "pause", "resume", "clear":
		r.Action = rest
		r.Objective = ""
	}
	if strings.HasPrefix(rest, "start ") {
		r.Objective = strings.TrimSpace(strings.TrimPrefix(rest, "start "))
	}
	if strings.HasPrefix(rest, "budget ") {
		r.Action = "budget"
		r.Objective = ""
		var n int64
		if _, err := fmt.Sscanf(rest, "budget %d", &n); err != nil {
			return nil, errors.New("usage: !goal budget <positive token limit>")
		}
		if fmt.Sprintf("budget %d", n) != rest {
			return nil, errors.New("usage: !goal budget <positive token limit>")
		}
		r.TokenBudget = &n
	}
	if strings.HasPrefix(rest, "--tokens ") {
		budgetAndGoal := strings.TrimSpace(strings.TrimPrefix(rest, "--tokens "))
		budget, objective, ok := strings.Cut(budgetAndGoal, " ")
		n, err := strconv.ParseInt(budget, 10, 64)
		if !ok || err != nil {
			return nil, errors.New("usage: !goal --tokens N <objective>")
		}
		r.Objective = strings.TrimSpace(objective)
		r.TokenBudget = &n
	}
	if strings.HasPrefix(rest, "resume-if ") {
		fields := strings.Fields(rest)
		if len(fields) != 3 && len(fields) != 4 {
			return nil, errors.New("invalid recovery command")
		}
		tid := fields[1]
		gen, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || !isCodexThreadID(tid) || gen < 0 {
			return nil, errors.New("invalid recovery command")
		}
		r.Action = "resume"
		r.Objective = ""
		r.ExpectedThreadID = tid
		r.ExpectedGeneration = &gen
		if len(fields) == 4 {
			r.ExpectedRunID = fields[3]
		}
	}
	return r, r.Validate()
}
func (r *GoalRequest) Validate() error {
	if r == nil {
		return nil
	}
	if len(r.OperationID) > 256 {
		return errors.New("goal operation id too long")
	}
	switch r.Action {
	case "start":
		if strings.TrimSpace(r.Objective) == "" || utf8.RuneCountInString(r.Objective) > 4000 {
			return errors.New("goal objective must contain 1–4000 characters")
		}
	case "status", "pause", "resume", "clear", "budget":
		if r.Objective != "" {
			return errors.New("only start accepts a goal objective")
		}
	default:
		return errors.New("unknown goal action")
	}
	if r.ExpectedGeneration != nil && (r.Action != "resume" || *r.ExpectedGeneration < 0 || !isCodexThreadID(r.ExpectedThreadID)) {
		return errors.New("invalid recovery fence")
	}
	if r.ExpectedRunID != "" && (r.ExpectedGeneration == nil || len(r.ExpectedRunID) != 64 || strings.Trim(r.ExpectedRunID, "0123456789abcdef") != "") {
		return errors.New("invalid recovery run nonce")
	}
	if r.TokenBudget != nil && *r.TokenBudget <= 0 {
		return errors.New("goal token budget must be positive")
	}
	if r.Action == "budget" && r.TokenBudget == nil {
		return errors.New("goal budget required")
	}
	return nil
}
func goalSummary(g *CodexGoal) string {
	if g == nil {
		return "Goal: none."
	}
	budget := ""
	if g.TokenBudget != nil {
		budget = fmt.Sprintf(" / %d", *g.TokenBudget)
	}
	return fmt.Sprintf("Goal: %s\n%s\nTokens: %d%s · Time: %ds", g.Status, g.Objective, g.TokensUsed, budget, g.TimeUsedSeconds)
}

var goalRefLocks keyedMutex
var goalAdmissions keyedMutex

func updateGoalBinding(agentID, key string, f func(*GoalBinding)) error {
	unlock := goalRefLocks.Lock(codexThreadRefPath(agentID, key))
	defer unlock()
	ref, err := readCodexThreadRef(agentID, key)
	if err != nil {
		return err
	}
	if ref == nil {
		return errors.New("no native Codex thread for this conversation")
	}
	if ref.Goal == nil {
		ref.Goal = &GoalBinding{SessionKey: key}
	}
	f(ref.Goal)
	b, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return atomicfile.WriteBytes(codexThreadRefPath(agentID, key), b, 0o600)
}

// Runtime ownership is conversation-scoped. A second app-server must not load
// the same native goal while it is running, including during startup/teardown.
var codexGoalRuntimes sync.Map // ref path -> *codexGoalRuntime

type codexGoalRuntime struct {
	isGoal        bool
	runID, origin string
	stopRequested bool
	mu            sync.Mutex
	controlMu     sync.Mutex
	pending       map[int64]chan *rpcMessage
	write         func(string, any) (int64, error)
	threadID      string
	closed        bool
	ready         bool
	agentID, key  string
}

func (r *codexGoalRuntime) rpc(ctx context.Context, method string, params any) (*rpcMessage, error) {
	r.mu.Lock()
	if r.closed || r.write == nil || !r.ready {
		r.mu.Unlock()
		return nil, errors.New("goal runtime is starting or stopping; retry status")
	}
	id, err := r.write(method, params)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	ch := make(chan *rpcMessage, 1)
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case msg := <-ch:
		if msg == nil {
			return nil, errors.New("goal runtime stopped before acknowledgement")
		}
		if msg.Error != nil {
			return nil, errors.New(msg.Error.Message)
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("goal operation acknowledgement timed out; query status before retrying")
	}
}
func (r *codexGoalRuntime) resolve(msg *rpcMessage) bool {
	id, ok := msg.numericID()
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.pending[id]
	if ok {
		delete(r.pending, id)
		ch <- msg
	}
	return ok
}
func (r *codexGoalRuntime) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for id, ch := range r.pending {
		close(ch)
		delete(r.pending, id)
	}
}
func (r *codexGoalRuntime) control(ctx context.Context, q *GoalRequest) (*CodexGoal, error) {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	r.mu.Lock()
	ready := r.ready && !r.closed
	tid := r.threadID
	r.mu.Unlock()
	if !ready {
		return nil, errors.New("goal runtime is starting or stopping; retry status")
	}

	if q.OperationID != "" {
		binding, err := goalBindingFor(r.agentID, r.key)
		if err != nil {
			return nil, err
		}
		if goalOperationSeen(binding, q.OperationID) {
			return binding.State, nil
		}
	}
	if q.Action == "start" || q.Action == "resume" {
		return nil, errors.New("goal is already running; pause it before starting or resuming")
	}
	if q.Action == "pause" || q.Action == "clear" {
		if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { b.DesiredPaused = true; b.Generation++ }); err != nil {
			return nil, err
		}
	}
	method, params := goalRPC(q, tid)
	msg, err := r.rpc(ctx, method, params)
	if err != nil {
		return nil, err
	}
	goal := decodeGoal(msg.Result)
	if q.Action == "clear" {
		goal = nil
	}
	if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) {
		b.State = goal
		rememberGoalOperation(b, q.OperationID)
	}); err != nil {
		return nil, err
	}
	return goal, nil
}
func goalRPC(q *GoalRequest, tid string) (string, map[string]any) {
	p := map[string]any{"threadId": tid}
	switch q.Action {
	case "status":
		return "thread/goal/get", p
	case "clear":
		return "thread/goal/clear", p
	case "pause":
		p["status"] = "paused"
	case "resume":
		p["status"] = "active"
	case "start":
		p["objective"] = q.Objective
		p["status"] = "active"
	}
	if q.TokenBudget != nil {
		p["tokenBudget"] = *q.TokenBudget
	}
	return "thread/goal/set", p
}
func decodeGoal(raw *json.RawMessage) *CodexGoal {
	if raw == nil {
		return nil
	}
	var v struct {
		Goal *CodexGoal `json:"goal"`
	}
	if json.Unmarshal(*raw, &v) != nil {
		return nil
	}
	return v.Goal
}
func goalControlEvents(g *CodexGoal) <-chan ChatEvent {
	ch := make(chan ChatEvent, 2)
	ch <- ChatEvent{Type: "goal", Goal: g}
	ch <- ChatEvent{Type: "done", Message: assembleAssistantMessage(goalSummary(g), "", nil, nil)}
	close(ch)
	return ch
}

func goalBindingFor(agentID, key string) (*GoalBinding, error) {
	ref, err := readCodexThreadRef(agentID, key)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, nil
	}
	return ref.Goal, nil
}

func (m *Manager) validateGoalBackend(agentID string, q *GoalRequest) error {
	if err := q.Validate(); err != nil {
		return err
	}
	a, ok := m.Get(agentID)
	if !ok {
		return errors.New("agent not found")
	}
	if a.Tool != ToolCodex {
		return errors.New("Goal mode currently requires the native Codex backend")
	}
	return nil
}

func (m *Manager) GoalSnapshot(agentID, key string) (*GoalBinding, error) {
	if _, ok := m.Get(agentID); !ok {
		return nil, ErrAgentNotFound
	}
	b, err := goalBindingFor(agentID, key)
	if b != nil {
		b.SetupContext = ""
		b.SeenOperations = nil
		b.RunID = ""
		b.UserID = ""
	}
	return b, err
}

func goalOperationSeen(b *GoalBinding, id string) bool {
	if b == nil || id == "" {
		return false
	}
	for _, seen := range b.SeenOperations {
		if seen == id {
			return true
		}
	}
	return false
}
func rememberGoalOperation(b *GoalBinding, id string) {
	if id == "" || goalOperationSeen(b, id) {
		return
	}
	b.SeenOperations = append(b.SeenOperations, id)
	if len(b.SeenOperations) > 128 {
		b.SeenOperations = b.SeenOperations[len(b.SeenOperations)-128:]
	}
}

// A live goal cannot use the legacy self-call handoff snapshot: that snapshot
// predates the tool result and final goal accounting. Operator-initiated moves
// drain the runner first and can preserve native state without this ambiguity.
func NativeGoalRunning(id, key string) bool {
	raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key))
	if !ok {
		return false
	}
	r := raw.(*codexGoalRuntime)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isGoal && !r.closed
}
