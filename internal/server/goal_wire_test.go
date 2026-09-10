package server

import (
	"encoding/base64"
	"encoding/json"
	"github.com/loppo-llc/kojo/internal/agent"
	"reflect"
	"testing"
)

func TestGoalWireRoundTripPreservesBindingAndNativeAccounting(t *testing.T) {
	original := agent.CodexThreadTransfer{RefName: "main.json", ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e", RolloutRelPath: "sessions/a.jsonl", RolloutContent: []byte("rollout"),
		Goal:       &agent.GoalBinding{Generation: 7, DesiredPaused: true, SetupContext: "original request", OriginPeerID: "hub", RunID: "run"},
		NativeGoal: &agent.CodexGoalTransfer{Deferred: true, Row: &agent.CodexSQLiteRow{Columns: []string{"tokens_used", "time_used_seconds"}, Values: []agent.CodexSQLiteValue{{Type: "integer", Int: 4321}, {Type: "integer", Int: 123}}}},
	}
	body, err := json.Marshal(codexSessionWire{Threads: []codexThreadWire{codexThreadToWire(original)}})
	if err != nil {
		t.Fatal(err)
	}
	var wire codexSessionWire
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	content, err := base64.StdEncoding.DecodeString(wire.Threads[0].RolloutContentB64)
	if err != nil {
		t.Fatal(err)
	}
	restored := wire.Threads[0].toTransfer(content)
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("lost goal data: %+v", restored)
	}
}
