// Package proctree kills a process together with every descendant it
// spawned, including descendants that moved into their own process group.
//
// OpenCode (the gemini backend) spawns its shell-tool children detached,
// each in a fresh process group, and installs no signal handler that
// would reap them: signalling only opencode (or only its process group)
// orphans a running `sleep 600` started by the model. Tree therefore
// snapshots the descendant set BEFORE anything is signalled (once the
// parent dies, the children are reparented to init and can no longer be
// found by ancestry) and signals every member.
package proctree

import "time"

// Tree is a snapshot of a process and its descendants.
type Tree struct {
	root  int
	procs []proc
}

type proc struct {
	pid, ppid, pgid int
	// created is the process start time where the platform reports it
	// (Windows; 0 elsewhere). Windows never rewrites a dead parent's pid
	// in its children's ppid, so a reused pid would otherwise adopt
	// unrelated processes: a child created before its "parent" is not
	// its child.
	created int64
	// start is the start time ps(1) reports (Unix; "" elsewhere or when
	// unknown). Second resolution is enough to tell a pid that was freed
	// and reused during the kill grace from the process snapshotted.
	start string
	// gone marks a member that has exited since the snapshot (set by the
	// pre-signal relisting): only the group it led is still signalled.
	gone bool
}

// snapshotRounds bounds the freeze-and-relist loop in Snapshot.
const snapshotRounds = 8

// Snapshot captures pid and its current descendants. Where the platform
// can suspend processes (Unix: SIGSTOP), every member found is frozen
// and the listing repeated until no new member appears, so a child
// forked between the listing and the kill cannot escape the snapshot
// (once its parent dies it is reparented to init and no longer findable
// by ancestry). The caller must terminate the Tree afterwards: Terminate
// resumes the frozen members together with the graceful signal. A failed
// listing still yields a Tree holding the root alone.
func Snapshot(pid int) *Tree {
	return snapshot(pid, Ident{})
}

// snapshot is Snapshot for a root expected to be id (see KillTreeIdent):
// nil, with nothing frozen, when the listing shows pid as another process
// or not at all. A zero id, or a failed listing, accepts the pid as is.
func snapshot(pid int, id Ident) *Tree {
	t := &Tree{root: pid}
	if pid <= 0 {
		return t
	}
	all := listProcs()
	if id.known() && all != nil {
		found := false
		for _, p := range all {
			if p.pid == pid {
				found = id.matches(p)
				break
			}
		}
		if !found {
			return nil
		}
	}
	t.procs = descendants(pid, all)
	if !canFreeze {
		return t
	}
	for i := 0; i < snapshotRounds; i++ {
		freeze(t.procs)
		var added bool
		t.procs, added = mergeProcs(t.procs, descendants(pid, listProcs()))
		if !added {
			break
		}
	}
	return t
}

// mergeProcs appends the members of next not already in cur (same pid
// and start time) and reports whether any were added. Members that have
// exited since stay in cur: they are still signalled by identity.
func mergeProcs(cur, next []proc) ([]proc, bool) {
	have := map[int]string{}
	for _, p := range cur {
		have[p.pid] = p.start
	}
	added := false
	for _, p := range next {
		if st, ok := have[p.pid]; ok && st == p.start {
			continue
		}
		have[p.pid] = p.start
		cur = append(cur, p)
		added = true
	}
	return cur, added
}

// PIDs returns the snapshot's pids, root first.
func (t *Tree) PIDs() []int {
	out := make([]int, 0, len(t.procs))
	for _, p := range t.procs {
		out = append(out, p.pid)
	}
	return out
}

// descendants returns root plus every transitive child in all, root
// first. Root is included even when the listing does not contain it.
func descendants(root int, all []proc) []proc {
	return descendantsBy(root, all, lineageByCreation)
}

// descendantsBy is descendants; byCreation (Windows, where a dead
// parent's pid stays in its children's ppid) means the creation times
// decide parenthood, so a non-root member whose creation time is unknown
// is kept but not expanded: a process claiming it as parent could be the
// child of an earlier process that had the same pid.
func descendantsBy(root int, all []proc, byCreation bool) []proc {
	children := map[int][]proc{}
	var self *proc
	for i := range all {
		p := all[i]
		if p.pid == root {
			self = &all[i]
			continue
		}
		children[p.ppid] = append(children[p.ppid], p)
	}
	out := []proc{{pid: root, ppid: -1, pgid: -1}}
	if self != nil {
		out[0] = *self
	}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		if byCreation && i > 0 && out[i].created == 0 {
			continue
		}
		for _, c := range children[out[i].pid] {
			if seen[c.pid] || (c.created != 0 && out[i].created != 0 && c.created < out[i].created) {
				continue
			}
			seen[c.pid] = true
			out = append(out, c)
		}
	}
	return out
}

// Ident tells a process apart from a later one that reuses its pid: its
// start time as the platform reports it (zero = unknown).
type Ident struct {
	start   string // Unix: ps(1) lstart
	created int64  // Windows: creation time
}

func (id Ident) known() bool { return id.start != "" || id.created != 0 }

func (id Ident) matches(p proc) bool {
	if id.start != "" {
		return p.start == id.start
	}
	if id.created != 0 {
		return p.created == id.created
	}
	return true
}

// Identify returns pid's Ident (zero when it cannot be read). Take it
// while the process is known to be alive, e.g. right after Start.
func Identify(pid int) Ident {
	if pid <= 0 {
		return Ident{}
	}
	return identify(pid)
}

// KillTreeIdent is KillTree for a pid recorded as id: when the pid is no
// longer that process (it exited, was reaped by Wait and handed to an
// unrelated process), nothing is signalled. A zero id behaves like
// KillTree.
func KillTreeIdent(pid int, id Ident, grace time.Duration) {
	if pid <= 0 {
		return
	}
	if t := snapshot(pid, id); t != nil {
		t.TerminateThenKill(grace)
	}
}

// KillTree terminates pid and all of its descendants: a graceful signal
// first, then a forced kill of whatever from the snapshot is still alive
// after grace. It blocks for at most grace (plus the listing).
func KillTree(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	Snapshot(pid).TerminateThenKill(grace)
}

// TerminateThenKill sends the graceful signal to every member, waits up
// to grace for all of them to exit and force-kills the survivors.
func (t *Tree) TerminateThenKill(grace time.Duration) {
	t.Terminate()
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !t.anyAlive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Kill()
}
