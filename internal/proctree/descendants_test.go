package proctree

import "testing"

// A process whose ppid names a pid that was reused by a later process is
// not that later process's child (Windows keeps stale ppids).
func TestDescendants_SkipsReusedParentPID(t *testing.T) {
	all := []proc{
		{pid: 10, ppid: 1, created: 500},
		{pid: 11, ppid: 10, created: 400}, // older than pid 10: stale ppid
		{pid: 12, ppid: 10, created: 600},
		{pid: 13, ppid: 11, created: 700}, // only reachable through 11
		{pid: 14, ppid: 12},               // unknown start time: kept
		{pid: 15, ppid: 14, created: 800}, // below an unverifiable member
	}
	got := map[int]bool{}
	for _, p := range descendantsBy(10, all, true) {
		got[p.pid] = true
	}
	for _, pid := range []int{10, 12, 14} {
		if !got[pid] {
			t.Errorf("pid %d missing from %v", pid, got)
		}
	}
	for _, pid := range []int{11, 13, 15} {
		if got[pid] {
			t.Errorf("pid %d wrongly adopted: %v", pid, got)
		}
	}
	// Without creation-time lineage (Unix) the ppid chain alone decides.
	got = map[int]bool{}
	for _, p := range descendantsBy(10, all, false) {
		got[p.pid] = true
	}
	if !got[15] {
		t.Errorf("pid 15 missing without creation lineage: %v", got)
	}
}
