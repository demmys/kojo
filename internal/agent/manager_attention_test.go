package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestRaiseAndClearAttention walks the flag's whole life: raised with a
// note, readable, then cleared exactly once.
func TestRaiseAndClearAttention(t *testing.T) {
	m := &Manager{}

	if raised, _, _ := m.AttentionFor("a"); raised {
		t.Fatal("a fresh manager should have no pages raised")
	}
	if m.ClearAttention("a") {
		t.Fatal("clearing a page that was never raised should report false")
	}

	before := time.Now()
	got, gotAt := m.RaiseAttention("a", "  デプロイの   承認が要る  ")
	if got != "デプロイの 承認が要る" {
		t.Fatalf("reason not normalised: %q", got)
	}

	raised, reason, at := m.AttentionFor("a")
	if !raised || reason != "デプロイの 承認が要る" {
		t.Fatalf("AttentionFor = %v, %q", raised, reason)
	}
	if at.Before(before) {
		t.Fatalf("timestamp %v predates the call at %v", at, before)
	}
	// The returned pair must describe the entry this call stored, not a
	// re-read — the handler echoes it straight back to the client.
	if !gotAt.Equal(at) {
		t.Fatalf("returned timestamp %v != stored %v", gotAt, at)
	}

	if !m.ClearAttention("a") {
		t.Fatal("clearing a raised page should report true")
	}
	if raised, _, _ := m.AttentionFor("a"); raised {
		t.Fatal("page survived the clear")
	}
	if m.ClearAttention("a") {
		t.Fatal("second clear should be a no-op")
	}
}

// TestRaiseAttentionIsPerAgent — one agent paging must not light up the
// rest of the roster.
func TestRaiseAttentionIsPerAgent(t *testing.T) {
	m := &Manager{}
	m.RaiseAttention("a", "見て")

	if raised, _, _ := m.AttentionFor("b"); raised {
		t.Fatal("agent b picked up agent a's page")
	}
	m.ClearAttention("b")
	if raised, _, _ := m.AttentionFor("a"); !raised {
		t.Fatal("clearing b dropped a's page")
	}
}

// TestRaiseAttentionRenotifies: every call is a deliberate, distinct page,
// so the hook fires again even while the flag is already up (and the newer
// reason wins).
func TestRaiseAttentionRenotifies(t *testing.T) {
	m := &Manager{}
	fired := make(chan string, 4)
	m.OnAttentionRaised = func(_, reason string) { fired <- reason }

	m.RaiseAttention("a", "one")
	m.RaiseAttention("a", "two")

	// The hook runs in its own goroutine, so delivery order is not
	// guaranteed — only that both pages fired.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case got := <-fired:
			seen[got] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 hooks fired: %v", i, seen)
		}
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("hook payloads = %v, want both reasons", seen)
	}
	if _, reason, _ := m.AttentionFor("a"); reason != "two" {
		t.Fatalf("latest reason should win, got %q", reason)
	}
}

// TestTruncateAttentionReason keeps the dashboard row from being blown
// open by a runaway note, and keeps multibyte text intact.
func TestTruncateAttentionReason(t *testing.T) {
	if got := truncateAttentionReason("  line one\nline two\t "); got != "line one line two" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	if got := truncateAttentionReason("   "); got != "" {
		t.Errorf("blank reason should normalise to empty, got %q", got)
	}

	long := strings.Repeat("あ", 500)
	got := truncateAttentionReason(long)
	if n := len([]rune(got)); n != attentionReasonMaxRunes+1 {
		t.Fatalf("truncated length = %d runes, want %d", n, attentionReasonMaxRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}

	exact := strings.Repeat("b", attentionReasonMaxRunes)
	if got := truncateAttentionReason(exact); got != exact {
		t.Errorf("a reason exactly at the cap must pass through untouched")
	}
}

// TestApplyAttentionPopulatesListFields — Manager.List runs applyAttention
// on a copy; the dashboard reads these three fields to render the pill.
func TestApplyAttentionPopulatesListFields(t *testing.T) {
	m := &Manager{}
	m.RaiseAttention("a", "見て")

	a := &Agent{ID: "a"}
	m.applyAttention(a)
	if !a.Attention || a.AttentionReason != "見て" || a.AttentionAt == 0 {
		t.Fatalf("applyAttention = %+v", *a)
	}

	idle := &Agent{ID: "b", Attention: true, AttentionReason: "stale", AttentionAt: 1}
	m.applyAttention(idle)
	if idle.Attention || idle.AttentionReason != "" || idle.AttentionAt != 0 {
		t.Fatalf("applyAttention left stale state on an unpaged agent: %+v", *idle)
	}
}

// TestListSurfacesAttention exercises the real read path the dashboard
// uses — Manager.List must fold the page onto the copy it returns, and
// must not mutate the cached *Agent it copied from (that one gets Saved).
func TestListSurfacesAttention(t *testing.T) {
	m := newTestManager(t)
	cached := &Agent{ID: "ag_paged", Name: "Paged", Tool: "claude"}
	m.mu.Lock()
	m.agents[cached.ID] = cached
	m.mu.Unlock()

	m.RaiseAttention(cached.ID, "見て")

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List returned %d agents", len(list))
	}
	if !list[0].Attention || list[0].AttentionReason != "見て" || list[0].AttentionAt == 0 {
		t.Fatalf("List did not surface the page: %+v", *list[0])
	}
	if cached.Attention || cached.AttentionReason != "" || cached.AttentionAt != 0 {
		t.Fatalf("List leaked runtime state onto the cached agent: %+v", *cached)
	}

	m.ClearAttention(cached.ID)
	if list = m.List(); list[0].Attention {
		t.Fatalf("cleared page still surfaced by List: %+v", *list[0])
	}
}

// TestAttentionNeverPersists locks the transient contract: the flag lives
// in the manager only. If the three fields were missing from either
// reservedAgentKeys or loadStripKeys a page would be written into
// settings_json and resurrect on every restart.
func TestAttentionNeverPersists(t *testing.T) {
	a := &Agent{ID: "ag", Name: "n", Attention: true, AttentionReason: "見て", AttentionAt: 12345}
	got, err := agentToSettings(a, map[string]any{
		"attention":       true,
		"attentionReason": "stale page from a previous run",
		"attentionAt":     999,
	})
	if err != nil {
		t.Fatalf("agentToSettings: %v", err)
	}
	for _, k := range []string{"attention", "attentionReason", "attentionAt"} {
		if v, present := got[k]; present {
			t.Errorf("%s leaked into settings_json: %v", k, v)
		}
	}

	// Load side: a row that somehow already carries the keys (written by
	// an older binary, or hand-edited) must not resurrect the page.
	out := &Agent{}
	if err := settingsToAgent(&store.AgentRecord{
		ID:   "ag",
		Name: "n",
		Settings: map[string]any{
			"attention":       true,
			"AttentionReason": "stale",
			"attentionAt":     999,
			"silentStart":     "22:00", // legitimate neighbour must survive
		},
	}, out); err != nil {
		t.Fatalf("settingsToAgent: %v", err)
	}
	if out.Attention || out.AttentionReason != "" || out.AttentionAt != 0 {
		t.Errorf("stale page survived load: %+v", *out)
	}
	if out.SilentStart != "22:00" {
		t.Errorf("strip ate a legitimate field: %q", out.SilentStart)
	}
}
