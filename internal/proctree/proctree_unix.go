//go:build !windows

package proctree

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// listProcs reads pid/ppid/pgid and the start time for every process via
// ps(1), which is present on macOS and every Linux distribution kojo
// targets. LC_ALL=C pins the lstart format.
func listProcs() []proc {
	cmd := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parsePS(out)
}

// lineageByCreation is false: proc.created is never set on Unix, where
// a dead parent's children are reparented, so ppid alone is the lineage.
const lineageByCreation = false

// identify reads pid's start time the way listProcs does.
func identify(pid int) Ident {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return Ident{}
	}
	return Ident{start: strings.Join(strings.Fields(string(out)), " ")}
}

// canSignalGroups reports that KillGroups can signal process groups.
const canSignalGroups = true

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

func ownGroup() int { return syscall.Getpgrp() }

// signalGroups sends sig (followed by SIGCONT for a graceful signal, so a
// stopped job acts on it) to each group.
func signalGroups(gs []int, sig syscall.Signal) {
	for _, g := range gs {
		_ = syscall.Kill(-g, sig)
		if sig != syscall.SIGKILL {
			_ = syscall.Kill(-g, syscall.SIGCONT)
		}
	}
}

// canFreeze reports that Snapshot can suspend members (SIGSTOP).
const canFreeze = true

// freeze suspends every member so none can fork while the snapshot is
// completed. kojo itself is never a member (members descend from root).
func freeze(procs []proc) {
	for _, p := range procs {
		if p.pid > 1 && p.pid != os.Getpid() {
			_ = syscall.Kill(p.pid, syscall.SIGSTOP)
		}
	}
}

func parsePS(out []byte) []proc {
	var procs []proc
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		procs = append(procs, proc{pid: pid, ppid: ppid, pgid: pgid, start: strings.Join(f[3:], " ")})
	}
	return procs
}

// Terminate sends SIGTERM to every process group led by a snapshot
// member and to every member individually, then SIGCONT the same way so
// members frozen by Snapshot run their handlers (a stopped process only
// acts on SIGTERM once resumed). kojo's own process group, and any group
// whose leader is not in the snapshot, is never signalled as a group.
func (t *Tree) Terminate() {
	live := t.live()
	t.signal(live, syscall.SIGTERM)
	t.signal(live, syscall.SIGCONT)
}

// Kill sends SIGKILL the same way Terminate sends SIGTERM.
func (t *Tree) Kill() { t.signal(t.live(), syscall.SIGKILL) }

// live returns the members that are still the process the snapshot saw:
// a pid that exited during the grace and was handed to an unrelated
// process (different start time) is dropped. When the listing fails the
// snapshot is used as is.
func (t *Tree) live() []proc {
	now := listProcs()
	if now == nil {
		return t.procs
	}
	cur := map[int]string{}
	for _, p := range now {
		cur[p.pid] = p.start
	}
	var out []proc
	for _, p := range t.procs {
		st, ok := cur[p.pid]
		if !ok {
			// Exited. Its group may outlive it: a pgid is never reused
			// while any member of the group is alive, so keep it for the
			// group signal only.
			p.gone = true
		} else if p.start != "" && st != "" && st != p.start {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (t *Tree) signal(procs []proc, sig syscall.Signal) {
	for _, g := range groups(procs, syscall.Getpgrp()) {
		_ = syscall.Kill(-g, sig)
	}
	for _, p := range procs {
		if p.pid > 1 && !p.gone {
			_ = syscall.Kill(p.pid, sig)
		}
	}
}

func (t *Tree) anyAlive() bool {
	for _, p := range t.procs {
		if p.pid > 1 && syscall.Kill(p.pid, 0) == nil {
			return true
		}
	}
	return false
}

// groups returns the process groups led by a member of procs, excluding
// own. A group whose leader has exited is included only while a live
// member still belongs to it: with no member left the pgid may have been
// handed to an unrelated group.
func groups(procs []proc, own int) []int {
	leaderLive := map[int]bool{}
	for _, p := range procs {
		if _, ok := leaderLive[p.pid]; !ok || !p.gone {
			leaderLive[p.pid] = !p.gone
		}
	}
	liveMember := map[int]bool{}
	for _, p := range procs {
		if !p.gone {
			liveMember[p.pgid] = true
		}
	}
	seen := map[int]bool{}
	var out []int
	for _, p := range procs {
		live, member := leaderLive[p.pgid]
		if p.pgid > 1 && p.pgid != own && member && (live || liveMember[p.pgid]) && !seen[p.pgid] {
			seen[p.pgid] = true
			out = append(out, p.pgid)
		}
	}
	return out
}
