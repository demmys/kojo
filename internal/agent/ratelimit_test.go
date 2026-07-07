package agent

import (
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestParseClaudeStream_RateLimitEvent verifies the stream decoder turns a
// CLI rate_limit_event into a "rate_limit" ChatEvent carrying the parsed
// info, and never folds it into the turn text.
func TestParseClaudeStream_RateLimitEvent(t *testing.T) {
	events, result := collectEvents(t,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1783526400,"rateLimitType":"seven_day","utilization":0.76,"isUsingOverage":false,"surpassedThreshold":0.75},"uuid":"u","session_id":"s"}`,
	)

	if result.fullText != "" {
		t.Errorf("rate_limit_event leaked into fullText: %q", result.fullText)
	}

	var got *RateLimitInfo
	for _, e := range events {
		if e.Type == "rate_limit" {
			got = e.RateLimit
		}
	}
	if got == nil {
		t.Fatalf("no rate_limit ChatEvent emitted; events=%+v", events)
	}
	if got.Status != "allowed_warning" {
		t.Errorf("Status = %q, want allowed_warning", got.Status)
	}
	if got.RateLimitType != "seven_day" {
		t.Errorf("RateLimitType = %q, want seven_day", got.RateLimitType)
	}
	if got.ResetsAt != 1783526400 {
		t.Errorf("ResetsAt = %d, want 1783526400", got.ResetsAt)
	}
	if got.Utilization != 0.76 {
		t.Errorf("Utilization = %v, want 0.76", got.Utilization)
	}
	if got.SurpassedThreshold != 0.75 {
		t.Errorf("SurpassedThreshold = %v, want 0.75", got.SurpassedThreshold)
	}
}

// TestManagerRateLimit_RecordAndReload exercises recordRateLimit → in-memory
// read → kv persistence → reload (fresh Manager sharing the same store).
func TestManagerRateLimit_RecordAndReload(t *testing.T) {
	st := newAgentStore(t)
	m := &Manager{store: st, logger: testLogger()}

	if _, ok := m.RateLimit("ag"); ok {
		t.Fatalf("RateLimit on fresh agent = ok; want not present")
	}

	info := RateLimitInfo{
		Status:        "allowed_warning",
		RateLimitType: "seven_day",
		ResetsAt:      1783526400,
		Utilization:   0.76,
	}
	m.recordRateLimit("ag", info)

	// In-memory read.
	snap, ok := m.RateLimit("ag")
	if !ok {
		t.Fatalf("RateLimit after record = not present")
	}
	if snap.Status != "allowed_warning" || snap.Utilization != 0.76 {
		t.Errorf("snapshot = %+v, want status allowed_warning util 0.76", snap)
	}
	if snap.ObservedAt == 0 {
		t.Errorf("ObservedAt not stamped")
	}

	// kv row shape.
	ctx := t.Context()
	rec, err := st.db.GetKV(ctx, rateLimitKVNamespace, rateLimitKVKey("ag"))
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if rec.Type != store.KVTypeJSON {
		t.Errorf("kv type = %q, want JSON", rec.Type)
	}
	if rec.Scope != store.KVScopeMachine {
		t.Errorf("kv scope = %q, want machine", rec.Scope)
	}

	// Reload path: a fresh Manager (empty in-memory cache) sharing the
	// same store must hydrate from kv.
	m2 := &Manager{store: st, logger: testLogger()}
	snap2, ok := m2.RateLimit("ag")
	if !ok {
		t.Fatalf("reload RateLimit = not present")
	}
	if snap2.Status != "allowed_warning" || snap2.ResetsAt != 1783526400 {
		t.Errorf("reloaded snapshot = %+v", snap2)
	}
}

// TestManagerRateLimit_ExpiredSnapshotAbsent verifies a snapshot whose window
// has already reset is treated as absent (no stale badge) and lazily purged
// from both the in-memory cache and the kv row.
func TestManagerRateLimit_ExpiredSnapshotAbsent(t *testing.T) {
	st := newAgentStore(t)
	m := &Manager{store: st, logger: testLogger()}

	past := time.Now().Unix() - 60 // window reset a minute ago
	m.recordRateLimit("ag", RateLimitInfo{
		Status:      "rejected",
		ResetsAt:    past,
		Utilization: 1.0,
	})

	// In-memory read must report absent and drop the stale entry.
	if _, ok := m.RateLimit("ag"); ok {
		t.Fatalf("RateLimit on expired snapshot = ok; want absent")
	}
	m.rateLimitsMu.Lock()
	_, cached := m.rateLimits["ag"]
	m.rateLimitsMu.Unlock()
	if cached {
		t.Error("expired snapshot left in in-memory cache")
	}

	// The kv row should have been lazily deleted.
	ctx := t.Context()
	if _, err := st.db.GetKV(ctx, rateLimitKVNamespace, rateLimitKVKey("ag")); err == nil {
		t.Error("expired snapshot kv row not deleted")
	}

	// A reload path (fresh Manager) that finds only an expired persisted row
	// must also report absent.
	m.recordRateLimit("ag", RateLimitInfo{Status: "rejected", ResetsAt: past, Utilization: 1.0})
	m2 := &Manager{store: st, logger: testLogger()}
	if _, ok := m2.RateLimit("ag"); ok {
		t.Fatalf("reload of expired snapshot = ok; want absent")
	}
}

// TestManagerRateLimit_ZeroResetsNeverExpires guards the zero-ResetsAt case:
// a snapshot with an unknown window must not be treated as expired.
func TestManagerRateLimit_ZeroResetsNeverExpires(t *testing.T) {
	st := newAgentStore(t)
	m := &Manager{store: st, logger: testLogger()}
	m.recordRateLimit("ag", RateLimitInfo{Status: "allowed_warning", Utilization: 0.8})
	if _, ok := m.RateLimit("ag"); !ok {
		t.Fatalf("RateLimit with zero ResetsAt = absent; want present")
	}
}
