//go:build !windows

package proctree

import (
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDescendants(t *testing.T) {
	all := []proc{
		{pid: 1, ppid: 0, pgid: 1},
		{pid: 10, ppid: 1, pgid: 10},  // root
		{pid: 11, ppid: 10, pgid: 10}, // child
		{pid: 12, ppid: 11, pgid: 12}, // detached grandchild
		{pid: 13, ppid: 12, pgid: 12},
		{pid: 20, ppid: 1, pgid: 20}, // unrelated
		{pid: 21, ppid: 20, pgid: 20},
	}
	got := descendants(10, all)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.pid)
	}
	if pids[0] != 10 {
		t.Fatalf("root not first: %v", pids)
	}
	sort.Ints(pids)
	if !reflect.DeepEqual(pids, []int{10, 11, 12, 13}) {
		t.Fatalf("descendants = %v", pids)
	}
	// Root missing from the listing (already exited) is still included.
	if got := descendants(99, all); len(got) != 1 || got[0].pid != 99 {
		t.Fatalf("missing root = %+v", got)
	}
}

func TestTreeGroups(t *testing.T) {
	tr := &Tree{root: 10, procs: []proc{
		{pid: 10, ppid: 1, pgid: 5}, // in the caller's group
		{pid: 11, ppid: 10, pgid: 11},
		{pid: 12, ppid: 11, pgid: 11},
		{pid: 13, ppid: 10, pgid: 77}, // group led by a non-member
	}}
	if got := groups(tr.procs, 5); !reflect.DeepEqual(got, []int{11}) {
		t.Fatalf("groups = %v", got)
	}
	// Leader exited: its group is signalled only while a live member
	// remains in it.
	gone := []proc{{pid: 11, pgid: 11, gone: true}, {pid: 12, pgid: 11}, {pid: 20, pgid: 20, gone: true}}
	if got := groups(gone, 5); !reflect.DeepEqual(got, []int{11}) {
		t.Fatalf("groups with exited leaders = %v", got)
	}
}

func TestParsePS(t *testing.T) {
	got := parsePS([]byte("  1     0     1\n 42  1 42 Sat Oct 17 08:52:58 2026\nbad line\n 7 x 7\n"))
	want := []proc{{pid: 1, ppid: 0, pgid: 1}, {pid: 42, ppid: 1, pgid: 42, start: "Sat Oct 17 08:52:58 2026"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePS = %+v", got)
	}
}

// TestKillTree_DetachedChild starts a shell whose child runs in its own
// process group (as OpenCode's shell tool does) and checks that both die.
func TestKillTree_DetachedChild(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "set -m; sleep 300 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	var child int
	for _, c := range string(buf[:n]) {
		if c >= '0' && c <= '9' {
			child = child*10 + int(c-'0')
		}
	}
	if child == 0 {
		t.Fatal("no child pid")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	KillTree(cmd.Process.Pid, 2*time.Second)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("root survived")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if exec.Command("kill", "-0", itoa(child)).Run() != nil {
			return
		}
	}
	_ = exec.Command("kill", "-9", itoa(child)).Run()
	t.Fatalf("detached child %d survived", child)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestMergeProcs(t *testing.T) {
	cur := []proc{{pid: 10, start: "a"}, {pid: 11, start: "b"}}
	got, added := mergeProcs(cur, []proc{{pid: 10, start: "a"}, {pid: 11, start: "b"}})
	if added || len(got) != 2 {
		t.Fatalf("no-op merge = %+v %v", got, added)
	}
	got, added = mergeProcs(got, []proc{{pid: 10, start: "a"}, {pid: 12, start: "c"}})
	if !added || len(got) != 3 || got[2].pid != 12 {
		t.Fatalf("merge = %+v %v", got, added)
	}
}

func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 300; :")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func psState(pid int) string {
	out, _ := exec.Command("ps", "-o", "state=", "-p", itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// Snapshot freezes the members it found (so none can fork past the
// snapshot) and TerminateThenKill resumes and ends them.
func TestSnapshot_FreezesUntilTerminated(t *testing.T) {
	cmd := startSleeper(t)
	var child int
	for deadline := time.Now().Add(5 * time.Second); child == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, p := range descendants(cmd.Process.Pid, listProcs()) {
			if p.pid != cmd.Process.Pid {
				child = p.pid
			}
		}
	}
	if child == 0 {
		t.Fatal("sleep child not found")
	}
	tr := Snapshot(cmd.Process.Pid)
	if st := psState(child); !strings.HasPrefix(st, "T") {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("child state after Snapshot = %q, want stopped", st)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	tr.TerminateThenKill(2 * time.Second)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("root survived")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(child, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Fatalf("child %d survived", child)
}

// A member whose pid now belongs to a process with another start time
// (pid reused during the grace) is not signalled.
func TestKill_SkipsReusedPID(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	tr := &Tree{root: pid, procs: []proc{{pid: pid, ppid: 1, pgid: -1, start: "Thu Jan  1 00:00:00 1970"}}}
	tr.Kill()
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process with a different start time was killed: %v", err)
	}
}

// KillTreeIdent signals nothing when the pid now belongs to a different
// process (start time differs) and kills the tree when it matches.
func TestKillTreeIdent(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	id := Identify(pid)
	if !id.known() {
		t.Fatalf("Identify(%d) = %+v", pid, id)
	}
	KillTreeIdent(pid, Ident{start: "Thu Jan  1 00:00:00 1970"}, time.Second)
	time.Sleep(100 * time.Millisecond)
	if st := psState(pid); st == "" || strings.HasPrefix(st, "Z") || strings.HasPrefix(st, "T") {
		t.Fatalf("mismatched ident touched the process (state %q)", st)
	}
	KillTreeIdent(pid, id, time.Second)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("matching ident did not kill the process")
	}
	// Reaped: the pid is not listed (or is someone else), nothing happens.
	KillTreeIdent(pid, id, time.Second)
}
