//go:build !windows

package proctree

import (
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseGroupLog(t *testing.T) {
	got := ParseGroupLog([]byte("123 1700000000\nbad\n1 1700000000\n45 x\n46 1700000001 extra\n\n789 1700000002\n"))
	want := []GroupRecord{
		{PGID: 123, Since: time.Unix(1700000000, 0)},
		{PGID: 789, Since: time.Unix(1700000002, 0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGroupLog = %+v", got)
	}
}

func TestVerifiedGroups(t *testing.T) {
	since := time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)
	ps := func(d time.Duration) string { return since.Add(d).Format("Mon Jan _2 15:04:05 2006") }
	all := []proc{
		{pid: 100, ppid: 1, pgid: 100, start: ps(0)},                // leader alive, matches
		{pid: 101, ppid: 100, pgid: 100, start: ps(time.Minute)},    // its job
		{pid: 201, ppid: 1, pgid: 200, start: ps(5 * time.Second)},  // leader gone, orphaned job
		{pid: 300, ppid: 1, pgid: 300, start: ps(time.Hour)},        // pid reused by a later leader
		{pid: 401, ppid: 1, pgid: 400, start: ps(-time.Hour)},       // member older than the record
		{pid: 501, ppid: 1, pgid: 500, start: ""},                   // unknown start
		{pid: 600, ppid: 1, pgid: 600, start: ps(0)},                // own group
		{pid: 701, ppid: 1, pgid: 701, start: ps(time.Second)},      // not recorded
		{pid: 801, ppid: 1, pgid: 800, start: ps(-1 * time.Second)}, // within the slack
	}
	var recs []GroupRecord
	for _, g := range []int{100, 200, 300, 400, 500, 600, 900, 800, 100} {
		recs = append(recs, GroupRecord{PGID: g, Since: since})
	}
	got := verifiedGroups(recs, all, 600)
	if !reflect.DeepEqual(got, []int{100, 200, 800}) {
		t.Fatalf("verifiedGroups = %v", got)
	}
}

// startOrphanJob starts a setsid shell that backgrounds `sleep` and
// exits, the way an OpenCode shell-tool command leaves a job behind, and
// returns the group id and the job's pid.
func startOrphanJob(t *testing.T) (pgid, job int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	job, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("job pid %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(job, syscall.SIGKILL) })
	return cmd.Process.Pid, job
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestKillGroups_ReapsOrphanedJob(t *testing.T) {
	since := time.Now()
	pgid, job := startOrphanJob(t)
	if !alive(job) {
		t.Fatal("job not running")
	}
	log := strconv.Itoa(pgid) + " " + strconv.FormatInt(since.Unix(), 10) + "\n"
	KillGroups(ParseGroupLog([]byte(log)), 2*time.Second)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !alive(job) {
			return
		}
	}
	t.Fatalf("orphaned job %d (group %d) survived", job, pgid)
}

func TestKillGroups_SkipsGroupOlderThanRecord(t *testing.T) {
	pgid, job := startOrphanJob(t)
	// A record from well after the job started: the group must be a
	// reused id, so it is left alone.
	recs := []GroupRecord{{PGID: pgid, Since: time.Now().Add(time.Hour)}}
	KillGroups(recs, 200*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if !alive(job) {
		t.Fatal("job of an unverified group was killed")
	}
}

// The wrapper kojo gives OpenCode logs exactly what KillGroups expects.
func TestKillGroups_WrapperLogFormat(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/pgids"
	cmd := exec.Command("/bin/sh", "-c", `printf '%s %s\n' "$$" "$(date +%s)" >> "$1"; exec /bin/sh -c 'sleep 300 >/dev/null 2>&1 & echo $!'`, "wrapper", logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	job, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if job <= 1 {
		t.Fatalf("job pid %q", out)
	}
	t.Cleanup(func() { _ = syscall.Kill(job, syscall.SIGKILL) })
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	recs := ParseGroupLog(b)
	if len(recs) != 1 || recs[0].PGID != cmd.Process.Pid {
		t.Fatalf("log %q parsed to %+v (shell pid %d)", b, recs, cmd.Process.Pid)
	}
	KillGroups(recs, 2*time.Second)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !alive(job) {
			return
		}
	}
	t.Fatalf("job %d survived", job)
}
