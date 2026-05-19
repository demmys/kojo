package server

import (
	"testing"
	"time"
)

// TestAgentRuntimeSnapshot_DisplayETag pins the composite HTTP ETag
// shape so the cache-busting suffixes don't get accidentally
// dropped. The whole point of the composite is that runtime field
// changes (nextCronAt advancing on cron fire, cronPausedGlobal
// flipping when the Dashboard toggle is hit) MUST flip the etag —
// otherwise a 304 fast-path lets a browser serve the cached body
// with stale values forever. See the bug fixed alongside this test:
// "agent settings Next check-in 表示が過去日時に張り付く".
func TestAgentRuntimeSnapshot_DisplayETag(t *testing.T) {
	t1 := time.Date(2026, 5, 19, 11, 24, 0, 0, time.FixedZone("JST", 9*3600))
	t2 := time.Date(2026, 5, 19, 11, 54, 0, 0, time.FixedZone("JST", 9*3600))

	cases := []struct {
		name string
		snap agentRuntimeSnapshot
		want string
	}{
		{
			name: "empty row etag → empty composite (signals skip-header)",
			snap: agentRuntimeSnapshot{rowETag: "", nextCron: t1, cronPaused: true},
			want: "",
		},
		{
			name: "row etag only, no schedule, not paused",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa"},
			want: "7-aaa",
		},
		{
			name: "row + paused suffix",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", cronPaused: true},
			want: "7-aaa.p",
		},
		{
			name: "row + nextCron suffix (the bug case)",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1},
			// 2026-05-19T11:24+09:00 = unix 1779755040 = base36 "tjfizc"
			want: "7-aaa.n" + base36Of(t1.Unix()),
		},
		{
			name: "row + paused + nextCron (full composite)",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1, cronPaused: true},
			want: "7-aaa.p.n" + base36Of(t1.Unix()),
		},
		{
			name: "nextCron advance flips the composite",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t2},
			want: "7-aaa.n" + base36Of(t2.Unix()),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.snap.displayETag()
			if got != c.want {
				t.Errorf("displayETag() = %q, want %q", got, c.want)
			}
		})
	}

	// Cross-case invariant: the same row etag with different
	// nextCron MUST yield distinct composites. This is the
	// regression guard for the cache-stale bug.
	a := agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1}.displayETag()
	b := agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t2}.displayETag()
	if a == b {
		t.Fatalf("nextCron advance must change displayETag, both = %q", a)
	}
}

func base36Of(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}
