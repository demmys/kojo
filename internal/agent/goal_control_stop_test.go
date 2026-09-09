package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestGoalReplyRespectsExplicitStop(t *testing.T) {
	for _, status := range []string{"active", "blocked", "paused", "usage_limited", "budget_limited", "complete"} {
		for _, paused := range []bool{false, true} {
			b := &GoalBinding{DesiredPaused: paused}
			want := !paused && status != "paused" && status != "complete"
			if got := goalResumesOnReply(b, &CodexGoal{Status: status}); got != want {
				t.Errorf("status=%s pause=%v got %v", status, paused, got)
			}
		}
	}
	if goalResumesOnReply(nil, &CodexGoal{Status: "active"}) || goalResumesOnReply(&GoalBinding{}, nil) {
		t.Fatal("missing goal resumed")
	}
}

func TestSlackGoalAuthorizationAcrossLiveAndIdle(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	key := id + ":slack:C:T"
	writeCodexThreadRef(id, key, codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e", Goal: &GoalBinding{UserID: "UOWNER", State: &CodexGoal{Status: "blocked"}}}, slog.Default())
	for _, live := range []bool{false, true} {
		path := codexThreadRefPath(id, key)
		if live {
			codexGoalRuntimes.Store(path, &codexGoalRuntime{isGoal: true, userID: "UOWNER"})
		}
		for _, action := range []string{"pause", "clear", "status", "resume", "budget", "start", "reply"} {
			for _, user := range []string{"UOWNER", "UOTHER", ""} {
				opts := OneShotOpts{SessionKey: key, GoalUserID: user, Goal: &GoalRequest{Action: action}}
				if action == "reply" {
					opts.Goal = nil
				}
				allowed := user == "UOWNER" || (action == "reply" && user == "")
				if err := authorizeSlackGoal(id, opts); (err == nil) != allowed {
					t.Errorf("live=%v action=%s user=%s err=%v", live, action, user, err)
				}
			}
		}
		codexGoalRuntimes.Delete(path)
	}
	// A new conversation has no binding to the old goal.
	if err := authorizeSlackGoal(id, OneShotOpts{SessionKey: id + ":slack:C:NEW", GoalUserID: "UOTHER"}); err != nil {
		t.Fatal(err)
	}
}

func TestSlackGoalControlChecksStartingRuntimeOwner(t *testing.T) {
	id, _ := setupCodexTransferTest(t)
	key := id + ":slack:C:NEW"
	path := codexThreadRefPath(id, key)
	codexGoalRuntimes.Store(path, &codexGoalRuntime{isGoal: true, userID: "UOWNER"})
	defer codexGoalRuntimes.Delete(path)
	if err := authorizeSlackGoal(id, OneShotOpts{SessionKey: key, GoalUserID: "UOTHER", Goal: &GoalRequest{Action: "pause"}}); err == nil {
		t.Fatal("foreign control admitted before goal persisted")
	}
}

func TestGoalSteerChecksUserForLocalAndPeer(t *testing.T) {
	m := newTestManager(t)
	a := &Agent{ID: "ag_goal_owner", Tool: ToolCodex}
	m.agents[a.ID] = a
	m.oneShotCancels = make(map[string]map[int64]context.CancelFunc)
	m.oneShotSessions = make(map[string]map[int64]string)
	m.oneShotArmed = make(map[string]map[int64]time.Time)
	key := a.ID + ":slack:C:T"
	writeCodexThreadRef(a.ID, key, codexThreadRef{ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e", Goal: &GoalBinding{UserID: "UOWNER", State: &CodexGoal{Status: "active"}}}, slog.Default())
	run := m.trackOneShot(a.ID, func() {}, key, "hub", "")
	defer m.untrackOneShot(a.ID, run)
	invoked := 0
	m.oneShotSteers = map[string]SteerFunc{key: func(string) error { invoked++; return nil }}
	for _, origin := range []string{"", "hub", "wrong-hub"} {
		for _, user := range []string{"UOWNER", "UOTHER", ""} {
			before := invoked
			err := m.SteerOneShotAsUser(a.ID, key, origin, "reply", user)
			allowed := user == "UOWNER" && origin != "wrong-hub"
			if (err == nil) != allowed || (invoked > before) != allowed {
				t.Errorf("origin=%s user=%s invoked=%d err=%v", origin, user, invoked-before, err)
			}
		}
	}
}
