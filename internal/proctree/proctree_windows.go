//go:build windows

package proctree

import (
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// listProcs reads pid/ppid (and the start time, see proc.created) for
// every process from a Toolhelp32 snapshot.
func listProcs() []proc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var procs []proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		if pid <= 0 {
			continue
		}
		procs = append(procs, proc{pid: pid, ppid: int(e.ParentProcessID), pgid: -1, created: processCreated(e.ProcessID)})
	}
	return procs
}

// lineageByCreation: Windows keeps a dead parent's pid in its children's
// ppid, so parenthood is checked against creation times.
const lineageByCreation = true

func identify(pid int) Ident { return Ident{created: processCreated(uint32(pid))} }

// processCreated returns pid's creation time (100ns ticks), 0 if unknown.
func processCreated(pid uint32) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
		return 0
	}
	return int64(c.HighDateTime)<<32 | int64(c.LowDateTime)
}

// Terminate and Kill both force-kill: Windows has no graceful signal for
// console processes kojo could send instead. `taskkill /T /F` on the root
// walks the parent-child links while the root is alive (catching children
// spawned after the snapshot); every pid in the snapshot (taken while the
// tree was intact, so it includes children whose parent has exited since)
// is then terminated individually. Each process is opened and its
// creation time compared with the snapshot first: an open handle keeps a
// pid from being reused, so a pid that was freed and handed to an
// unrelated process since the snapshot is never killed. Processes already
// orphaned when the snapshot was taken cannot be found by ancestry.
func (t *Tree) Terminate() { t.Kill() }

func (t *Tree) Kill() {
	var rootCreated int64
	for _, p := range t.procs {
		if p.pid == t.root {
			rootCreated = p.created
		}
	}
	if t.root > 0 {
		if h, ok := openSame(t.root, rootCreated); ok {
			cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(t.root))
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			_ = cmd.Run()
			_ = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
	}
	for _, p := range t.procs {
		// A member whose creation time could not be read cannot be
		// told apart from a reused pid: leave it (taskkill /T above
		// still reaches it while its parent chain is intact). The root
		// is kojo's own child, whose handle Go holds until Wait, so its
		// pid cannot have been reused.
		if p.pid == t.root || p.pid <= 0 || p.created == 0 {
			continue
		}
		if h, ok := openSame(p.pid, p.created); ok {
			_ = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
	}
}

// openSame opens pid for termination if it is still the process the
// snapshot saw (same creation time; created == 0 means unknown, which
// Kill accepts for the root only). The caller closes the handle.
func openSame(pid int, created int64) (windows.Handle, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	if created != 0 {
		var c, x, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil || int64(c.HighDateTime)<<32|int64(c.LowDateTime) != created {
			windows.CloseHandle(h)
			return 0, false
		}
	}
	return h, true
}

func (t *Tree) anyAlive() bool { return false }

// canFreeze is false: Windows has no per-process suspend kojo can undo
// with the kill (Snapshot lists once).
const canFreeze = false

func freeze([]proc) {}

// Windows shells are not detached into groups kojo could signal.
const canSignalGroups = false

const (
	sigTerm = 0
	sigKill = 1
)

func ownGroup() int { return -1 }

func signalGroups([]int, int) {}
