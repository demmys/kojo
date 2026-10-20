package proctree

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// GroupRecord is a process group a command shell led, as logged by the
// shell wrapper kojo gives OpenCode: pid (= pgid, the shell runs in its
// own group) and the wall-clock second it started. Background jobs the
// command left behind stay in that group after the shell exits, when they
// are no longer findable by ancestry.
type GroupRecord struct {
	PGID  int
	Since time.Time
}

// ParseGroupLog parses "<pid> <unix seconds>" lines; malformed lines are
// skipped.
func ParseGroupLog(b []byte) []GroupRecord {
	var out []GroupRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		sec, e2 := strconv.ParseInt(f[1], 10, 64)
		if e1 != nil || e2 != nil || pid <= 1 || sec <= 0 {
			continue
		}
		out = append(out, GroupRecord{PGID: pid, Since: time.Unix(sec, 0)})
	}
	return out
}

// groupSlack absorbs the second-resolution timestamps on both sides.
const groupSlack = 2 * time.Second

// verifiedGroups returns the recorded groups that still have members and
// whose members are all plausibly the recorded shell's: the leader (if
// alive) started within groupSlack of the record and no member started
// before it. A pgid reused by an unrelated group fails one of the two.
func verifiedGroups(recs []GroupRecord, all []proc, own int) []int {
	byPGID := map[int][]proc{}
	for _, p := range all {
		byPGID[p.pgid] = append(byPGID[p.pgid], p)
	}
	seen := map[int]bool{}
	var out []int
	for _, r := range recs {
		g := r.PGID
		if g <= 1 || g == own || seen[g] {
			continue
		}
		members := byPGID[g]
		if len(members) == 0 {
			continue
		}
		ok := true
		for _, p := range members {
			st, known := parseStart(p.start)
			if !known {
				ok = false
				break
			}
			if st.Before(r.Since.Add(-groupSlack)) || (p.pid == g && st.After(r.Since.Add(groupSlack))) {
				ok = false
				break
			}
		}
		if ok {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// parseStart parses a ps lstart value (LC_ALL=C), e.g.
// "Sat Oct 17 08:52:58 2026", in local time.
func parseStart(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.Local)
	if err != nil {
		t, err = time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.Local)
	}
	return t, err == nil
}

// KillGroups terminates the recorded groups that verifiedGroups accepts:
// SIGTERM (and SIGCONT), then after up to grace SIGKILL to the groups
// that still verify. It blocks for at most grace plus the listings.
func KillGroups(recs []GroupRecord, grace time.Duration) {
	if len(recs) == 0 || !canSignalGroups {
		return
	}
	gs := verifiedGroups(recs, listProcs(), ownGroup())
	if len(gs) == 0 {
		return
	}
	signalGroups(gs, sigTerm)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if len(verifiedGroups(recs, listProcs(), ownGroup())) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	signalGroups(verifiedGroups(recs, listProcs(), ownGroup()), sigKill)
}
