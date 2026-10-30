package agent

import (
	"bytes"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// grok (1.0.62) can finish a turn and then never exit: when a background
// command completes on its own mid-turn, grok queues a wake for it that
// headless mode never consumes, and the post-turn background wait —
// unlimited under grokBackgroundWaitTimeout — blocks on it forever. kojo
// only finishes the turn when grok exits, so the chat (and e.g. a Discord
// typing indicator) hangs indefinitely.
//
// The watchdog terminates grok once it has been silent on stdout for
// grokIdleKillAfter while running no command of its own. Commands grok
// runs (foreground or background) are child shells carrying
// grokUserCmdMarker; MCP servers are children too but never carry it, so
// legitimate long background work keeps grok alive.
const grokUserCmdMarker = "__grok_user_cmd"

// Variables so tests can shorten them.
var (
	grokIdleKillAfter    = 10 * time.Minute
	grokIdlePollInterval = 15 * time.Second
)

// activityReader records the time of the last successful read.
type activityReader struct {
	r    io.Reader
	last atomic.Int64 // unix nanos
}

func newActivityReader(r io.Reader) *activityReader {
	a := &activityReader{r: r}
	a.last.Store(time.Now().UnixNano())
	return a
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (a *activityReader) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, a.last.Load()))
}

// grokHasUserCommand reports whether pid has a direct child running a
// grok-spawned shell command. ok is false when the process table could
// not be read; callers must then leave grok alone.
func grokHasUserCommand(pid int) (has bool, ok bool) {
	if runtime.GOOS == "windows" {
		return false, false
	}
	out, err := exec.Command("ps", "-ax", "-o", "ppid=,command=").Output()
	if err != nil {
		return false, false
	}
	return psHasUserCommandChild(out, pid), true
}

func psHasUserCommandChild(psOut []byte, pid int) bool {
	want := strconv.Itoa(pid)
	for _, line := range bytes.Split(psOut, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 2 || fields[0] != want {
			continue
		}
		if strings.Contains(string(line), grokUserCmdMarker) {
			return true
		}
	}
	return false
}

// watchGrokIdle polls until done is closed. It returns a flag that is set
// once the watchdog has signalled grok.
func watchGrokIdle(cmd *exec.Cmd, act *activityReader, done <-chan struct{}, logger *slog.Logger, agentID string) *atomic.Bool {
	fired := &atomic.Bool{}
	go func() {
		t := time.NewTicker(grokIdlePollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				idle := act.idleFor(now)
				if idle < grokIdleKillAfter || cmd.Process == nil {
					continue
				}
				has, ok := grokHasUserCommand(cmd.Process.Pid)
				if !ok || has {
					continue
				}
				select {
				case <-done:
					return
				default:
				}
				// Set before signalling so the waiter never sees the exit
				// without the flag; cleared again if the signal failed.
				fired.Store(true)
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					fired.Store(false)
					continue
				}
				logger.Warn("grok: idle with no running command, terminated",
					"agent", agentID, "pid", cmd.Process.Pid, "idle", idle.Round(time.Second))
				return
			}
		}
	}()
	return fired
}
