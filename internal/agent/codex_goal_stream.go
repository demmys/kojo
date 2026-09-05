package agent

import (
	"encoding/json"
	"log/slog"
)

// Native app-server owns continuation. There is deliberately no turn/start or
// empty-answer retry here: goal/set active starts idle work itself.
func runCodexGoal(scanner *jsonlLineScanner, q *GoalRequest, r *codexGoalRuntime, steer *codexSteerer, respond codexServerRequestResponder, logger *slog.Logger, send func(ChatEvent) bool, questions ...*codexQuestionState) *codexStreamResult {
	combined := &codexStreamResult{}
	var qs *codexQuestionState
	if len(questions) > 0 {
		qs = questions[0]
	}
	current := &codexStreamResult{questions: qs}
	phases := map[string]string{}
	method, params := goalRPC(q, r.threadID)
	startID, err := r.write(method, params)
	if err != nil {
		combined.processError = err.Error()
		return combined
	}
	r.mu.Lock()
	r.ready = true
	r.mu.Unlock()
	var goal *CodexGoal
	defer func() { combined.fullText.WriteString("\n\n" + goalSummary(goal)) }()
	inTurn := false
	activated := false
	currentTurnID := ""
	var checkID int64
	publish := func(g *CodexGoal) bool {
		goal = g
		if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { b.State = g }); err != nil {
			combined.processError = "persist goal checkpoint: " + err.Error()
			return false
		}
		return send(ChatEvent{Type: "goal", Goal: g})
	}
	for scanner.Scan() {
		var msg rpcMessage
		if json.Unmarshal([]byte(scanner.Text()), &msg) != nil {
			continue
		}
		if handled, err := handleCodexServerRequest(&msg, respond, logger); err != nil {
			combined.processError = err.Error()
			break
		} else if handled {
			continue
		}
		if id, ok := msg.numericID(); ok {
			handled := r.resolve(&msg)
			if !handled && steer != nil {
				steer.resolve(id, msg.Error)
			}
			if id == startID || id == checkID {
				if id == checkID {
					checkID = 0
				}
				if id == startID {
					activated = true
				}
				if msg.Error != nil {
					combined.processError = msg.Error.Message
					break
				}
				if id == startID {
					if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) {
						b.ActivationPending = false
						b.RecoveryAttempts = 0
						rememberGoalOperation(b, q.OperationID)
					}); err != nil {
						combined.processError = err.Error()
						break
					}
				}
				if !publish(decodeGoal(msg.Result)) {
					break
				}
				if !inTurn && checkID == 0 && (goal == nil || goal.Status != "active") {
					combined.turnCompleted = true
					return combined
				}
			}
			continue
		}
		switch msg.Method {
		case "thread/goal/updated":
			if !activated {
				continue
			}
			if !publish(decodeGoal(msg.Params)) {
				combined.absorb(current)
				return combined
			}
			if goal != nil && goal.Status == "paused" && inTurn && currentTurnID != "" {
				_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
			}
			// A goal may complete during a turn. Drain the final response before
			// releasing the runner, attachment ownership, and Slack stream.
			if !inTurn && checkID == 0 && (goal == nil || goal.Status != "active") {
				combined.turnCompleted = true
				return combined
			}
		case "thread/goal/cleared":
			if !activated {
				continue
			}
			publish(nil)
			if inTurn && currentTurnID != "" {
				_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
			}
			if !inTurn {
				combined.turnCompleted = true
				return combined
			}
		case "turn/started":
			inTurn = true
			currentTurnID = decodeCodexTurnID(msg.Params)
			if steer != nil {
				steer.setTurnID(decodeCodexTurnID(msg.Params))
			}
		case "turn/completed":
			current.handleNotification(&msg, phases, logger, send)
			if current.turnStatus == "interrupted" && (goal == nil || goal.Status == "paused") {
				current.processError = ""
			}
			if current.processError == "" && current.turnStatus == "completed" {
				if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { b.RuntimeFailures = 0 }); err != nil {
					combined.processError = err.Error()
					return combined
				}
			}
			combined.absorb(current)
			current = &codexStreamResult{questions: qs}
			phases = map[string]string{}
			inTurn = false
			if steer != nil {
				steer.finishTurn()
			}
			if combined.processError != "" {
				return combined
			}

			// Account for state notifications ordered after turn/completed.
			checkID, err = r.write("thread/goal/get", map[string]any{"threadId": r.threadID})
			if err != nil {
				combined.processError = err.Error()
				return combined
			}
			combined.fullText.WriteString("\n\n")
			send(ChatEvent{Type: "text", Delta: "\n\n"})
		default:
			if current.handleNotification(&msg, phases, logger, send) && current.cancelled {
				combined.absorb(current)
				return combined
			}
		}
	}
	combined.absorb(current)
	if combined.processError == "" {
		combined.processError = "Codex goal stream ended before goal stopped"
		if err := scanner.Err(); err != nil {
			combined.processError = codexReadErrorMessage(err)
		}
	}
	return combined
}
