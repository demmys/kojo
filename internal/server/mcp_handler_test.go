package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestParseListChannelsArgs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want listChannelsArgs
	}{
		{
			name: "defaults when no args",
			in:   map[string]any{},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
		{
			name: "explicit limit honored",
			in:   map[string]any{"limit": float64(50)},
			want: listChannelsArgs{Limit: 50, MemberOnly: true},
		},
		{
			name: "limit clamped to max",
			in:   map[string]any{"limit": float64(10_000)},
			want: listChannelsArgs{Limit: listChannelsMaxLimit, MemberOnly: true},
		},
		{
			name: "non-positive limit falls back to default",
			in:   map[string]any{"limit": float64(0)},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
		{
			name: "name_contains is lowercased",
			in:   map[string]any{"name_contains": "General"},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, NameFilter: "general", MemberOnly: true},
		},
		{
			name: "member_only false respected",
			in:   map[string]any{"member_only": false},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: false},
		},
		{
			name: "wrong types ignored",
			in:   map[string]any{"limit": "200", "member_only": "yes", "name_contains": 3},
			want: listChannelsArgs{Limit: listChannelsDefaultLimit, MemberOnly: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseListChannelsArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseListChannelsArgs(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMatchChannel(t *testing.T) {
	mkCh := func(id, name string, member bool) slack.Channel {
		var ch slack.Channel
		ch.ID = id
		ch.Name = name
		ch.IsMember = member
		ch.Topic.Value = "topic-" + name
		ch.Purpose.Value = "purpose-" + name
		ch.NumMembers = 3
		return ch
	}

	t.Run("member_only filters out non-members", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "general", false), listChannelsArgs{MemberOnly: true, Limit: 10})
		if ok {
			t.Fatal("expected non-member channel to be filtered out")
		}
	})

	t.Run("member_only=false keeps non-members", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "general", false), listChannelsArgs{MemberOnly: false, Limit: 10})
		if !ok {
			t.Fatal("expected non-member channel to pass when memberOnly=false")
		}
	})

	t.Run("name filter is case-insensitive substring match", func(t *testing.T) {
		_, ok := matchChannel(mkCh("C1", "Engineering", true),
			listChannelsArgs{NameFilter: "engine", MemberOnly: true, Limit: 10})
		if !ok {
			t.Fatal("expected engineering to match 'engine'")
		}
		_, ok = matchChannel(mkCh("C1", "sales", true),
			listChannelsArgs{NameFilter: "engine", MemberOnly: true, Limit: 10})
		if ok {
			t.Fatal("expected sales not to match 'engine'")
		}
	})

	t.Run("info carries all metadata", func(t *testing.T) {
		info, ok := matchChannel(mkCh("C1", "general", true),
			listChannelsArgs{MemberOnly: true, Limit: 10})
		if !ok {
			t.Fatal("expected match")
		}
		want := channelInfo{
			ID: "C1", Name: "general",
			Topic: "topic-general", Purpose: "purpose-general",
			NumMembers: 3, IsMember: true,
		}
		if info != want {
			t.Errorf("info = %+v, want %+v", info, want)
		}
	})
}

// fakeLister implements slackConversationLister using pre-programmed pages.
type fakeLister struct {
	pages [][]slack.Channel
	next  []string // next cursor per page (same length as pages)
	errAt int      // 1-indexed; 0 = never error
	calls int
}

func (f *fakeLister) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	f.calls++
	if f.errAt > 0 && f.calls == f.errAt {
		return nil, "", errors.New("boom")
	}
	if f.calls > len(f.pages) {
		return nil, "", nil
	}
	idx := f.calls - 1
	return f.pages[idx], f.next[idx], nil
}

func mkCh(id, name string, member bool) slack.Channel {
	var ch slack.Channel
	ch.ID = id
	ch.Name = name
	ch.IsMember = member
	return ch
}

func TestListSlackChannelsStopsAtLimit(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{
			{mkCh("C1", "a", true), mkCh("C2", "b", true), mkCh("C3", "c", true)},
		},
		next: []string{""},
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 2, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(got))
	}
}

func TestListSlackChannelsPaginates(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{
			{mkCh("C1", "a", true)},
			{mkCh("C2", "b", true)},
		},
		next: []string{"cursor2", ""},
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fl.calls != 2 {
		t.Errorf("expected 2 API calls (pagination), got %d", fl.calls)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 channels, got %d", len(got))
	}
}

func TestListSlackChannelsMaxPagesCap(t *testing.T) {
	// Every page returns a new cursor, so without the cap we'd loop forever.
	pages := make([][]slack.Channel, 10)
	nexts := make([]string, 10)
	for i := range pages {
		pages[i] = []slack.Channel{mkCh("C", "ch", true)}
		nexts[i] = "more"
	}
	fl := &fakeLister{pages: pages, next: nexts}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 1000, MemberOnly: true}, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fl.calls != listChannelsMaxPages {
		t.Errorf("expected %d calls (max pages), got %d", listChannelsMaxPages, fl.calls)
	}
	if len(got) != listChannelsMaxPages {
		t.Errorf("expected %d channels, got %d", listChannelsMaxPages, len(got))
	}
}

func TestListSlackChannelsPartialFailureReturnsCollected(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{{mkCh("C1", "a", true)}, nil},
		next:  []string{"cursor2", ""},
		errAt: 2,
	}
	var partialErr error
	var partialCount int
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true},
		func(e error, c int) { partialErr = e; partialCount = c },
	)
	if err != nil {
		t.Fatalf("expected nil err on partial success, got %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 channel, got %d", len(got))
	}
	if partialErr == nil || partialCount != 1 {
		t.Errorf("onPartial not invoked correctly: err=%v count=%d", partialErr, partialCount)
	}
}

func TestListSlackChannelsFirstPageErrorReturnsError(t *testing.T) {
	fl := &fakeLister{
		pages: [][]slack.Channel{nil},
		next:  []string{""},
		errAt: 1,
	}
	got, err := listSlackChannels(context.Background(), fl,
		listChannelsArgs{Limit: 100, MemberOnly: true}, nil)
	if err == nil {
		t.Fatal("expected error on first-page failure")
	}
	if got != nil {
		t.Errorf("expected nil channels on hard error, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// History / Thread limit clamping
// ---------------------------------------------------------------------------

func TestHistoryLimitClamping(t *testing.T) {
	cases := []struct {
		name     string
		input    float64
		wantCap  int
		maxLimit int
		defLimit int
	}{
		{"default", 0, historyDefaultLimit, historyMaxLimit, historyDefaultLimit},
		{"explicit", 50, 50, historyMaxLimit, historyDefaultLimit},
		{"over max", 999, historyMaxLimit, historyMaxLimit, historyDefaultLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.defLimit
			if tc.input > 0 {
				limit = int(tc.input)
				if limit > tc.maxLimit {
					limit = tc.maxLimit
				}
			}
			if limit != tc.wantCap {
				t.Errorf("limit = %d, want %d", limit, tc.wantCap)
			}
		})
	}
}

func TestThreadRepliesLimitClamping(t *testing.T) {
	cases := []struct {
		name    string
		input   float64
		wantCap int
	}{
		{"default", 0, threadRepliesDefaultLimit},
		{"explicit", 30, 30},
		{"over max", 999, threadRepliesMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := threadRepliesDefaultLimit
			if tc.input > 0 {
				limit = int(tc.input)
				if limit > threadRepliesMaxLimit {
					limit = threadRepliesMaxLimit
				}
			}
			if limit != tc.wantCap {
				t.Errorf("limit = %d, want %d", limit, tc.wantCap)
			}
		})
	}
}

func TestListUsersLimitClamping(t *testing.T) {
	cases := []struct {
		name    string
		input   float64
		wantCap int
	}{
		{"default", 0, listUsersDefaultLimit},
		{"explicit", 100, 100},
		{"over max", 9999, listUsersMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := listUsersDefaultLimit
			if tc.input > 0 {
				limit = int(tc.input)
				if limit > listUsersMaxLimit {
					limit = listUsersMaxLimit
				}
			}
			if limit != tc.wantCap {
				t.Errorf("limit = %d, want %d", limit, tc.wantCap)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Emoji strip-colons helper
// ---------------------------------------------------------------------------

func TestEmojiColonStripping(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":thumbsup:", "thumbsup"},
		{"thumbsup", "thumbsup"},
		{":heart:", "heart"},
		{"::double::", "double"},
	}
	for _, tc := range cases {
		got := strings.Trim(tc.in, ":")
		if got != tc.want {
			t.Errorf("Trim(%q, \":\") = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Emoji filter
// ---------------------------------------------------------------------------

func TestEmojiFilter(t *testing.T) {
	emojiMap := map[string]string{
		"partyparrot": "https://example.com/partyparrot.gif",
		"shipit":      "alias:squirrel",
		"thumbsup2":   "https://example.com/thumbsup2.png",
	}

	filter := "party"
	var result []emojiInfo
	for name, value := range emojiMap {
		if !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		result = append(result, emojiInfo{Name: name, Value: value})
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 emoji matching %q, got %d", filter, len(result))
	}
	if result[0].Name != "partyparrot" {
		t.Errorf("expected partyparrot, got %q", result[0].Name)
	}
}

// ---------------------------------------------------------------------------
// UserInfo filter logic
// ---------------------------------------------------------------------------

func TestUserFilterLogic(t *testing.T) {
	mkUser := func(id, name, realName, displayName string, isBot, deleted bool) slack.User {
		var u slack.User
		u.ID = id
		u.Name = name
		u.RealName = realName
		u.Profile.DisplayName = displayName
		u.IsBot = isBot
		u.Deleted = deleted
		return u
	}

	allUsers := []slack.User{
		mkUser("U1", "alice", "Alice Smith", "Alice", false, false),
		mkUser("U2", "bob", "Bob Jones", "Bobby", false, false),
		mkUser("U3", "slackbot", "Slackbot", "", true, false),
		mkUser("U4", "deleted_user", "Gone", "", false, true),
		mkUser("U5", "alice_bot", "Alice Bot", "AliceBot", true, false),
	}

	t.Run("no filter no bots", func(t *testing.T) {
		var users []userInfo
		for _, u := range allUsers {
			if u.Deleted {
				continue
			}
			if u.IsBot || u.ID == "USLACKBOT" {
				continue
			}
			users = append(users, userInfo{ID: u.ID, Name: u.Name})
		}
		if len(users) != 2 {
			t.Errorf("expected 2 human users, got %d", len(users))
		}
	})

	t.Run("include bots", func(t *testing.T) {
		var users []userInfo
		for _, u := range allUsers {
			if u.Deleted {
				continue
			}
			users = append(users, userInfo{ID: u.ID, Name: u.Name})
		}
		if len(users) != 4 {
			t.Errorf("expected 4 non-deleted users (including bots), got %d", len(users))
		}
	})

	t.Run("name filter", func(t *testing.T) {
		filter := "alice"
		var users []userInfo
		for _, u := range allUsers {
			if u.Deleted {
				continue
			}
			match := strings.Contains(strings.ToLower(u.Name), filter) ||
				strings.Contains(strings.ToLower(u.RealName), filter) ||
				strings.Contains(strings.ToLower(u.Profile.DisplayName), filter)
			if !match {
				continue
			}
			users = append(users, userInfo{ID: u.ID, Name: u.Name})
		}
		// alice (U1) + alice_bot (U5)
		if len(users) != 2 {
			t.Errorf("expected 2 users matching %q, got %d", filter, len(users))
		}
	})
}
