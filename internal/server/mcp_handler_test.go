package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// clampLimit (shared by history, thread replies, and list_users handlers)
// ---------------------------------------------------------------------------

func TestClampLimit(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		def  int
		max  int
		want int
	}{
		{"missing arg uses default", nil, 20, 100, 20},
		{"wrong type uses default", "50", 20, 100, 20},
		{"zero uses default", float64(0), 20, 100, 20},
		{"negative uses default", float64(-5), 20, 100, 20},
		{"fractional below 1 uses default", float64(0.5), 20, 100, 20},
		{"explicit honored", float64(50), 20, 100, 50},
		{"explicit one honored", float64(1), 20, 100, 1},
		{"explicit fractional rounds down to int", float64(50.7), 20, 100, 50},
		{"over max clamps", float64(999), 20, 100, 100},
		{"history defaults", nil, historyDefaultLimit, historyMaxLimit, historyDefaultLimit},
		{"history over max", float64(999), historyDefaultLimit, historyMaxLimit, historyMaxLimit},
		{"thread defaults", nil, threadRepliesDefaultLimit, threadRepliesMaxLimit, threadRepliesDefaultLimit},
		{"thread over max", float64(9999), threadRepliesDefaultLimit, threadRepliesMaxLimit, threadRepliesMaxLimit},
		{"users defaults", nil, listUsersDefaultLimit, listUsersMaxLimit, listUsersDefaultLimit},
		{"users over max", float64(9999), listUsersDefaultLimit, listUsersMaxLimit, listUsersMaxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLimit(tc.raw, tc.def, tc.max); got != tc.want {
				t.Errorf("clampLimit(%v, %d, %d) = %d, want %d", tc.raw, tc.def, tc.max, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// filterEmoji
// ---------------------------------------------------------------------------

func TestFilterEmoji(t *testing.T) {
	emojiMap := map[string]string{
		"partyparrot": "https://example.com/partyparrot.gif",
		"shipit":      "alias:squirrel",
		"thumbsup2":   "https://example.com/thumbsup2.png",
		"PartyHat":    "https://example.com/partyhat.gif",
	}

	t.Run("substring match (case-insensitive)", func(t *testing.T) {
		got := filterEmoji(emojiMap, "PARTY")
		if len(got) != 2 {
			t.Fatalf("expected 2 emoji matching 'PARTY', got %d (%+v)", len(got), got)
		}
		names := map[string]bool{}
		for _, e := range got {
			names[e.Name] = true
		}
		if !names["partyparrot"] || !names["PartyHat"] {
			t.Errorf("expected partyparrot+PartyHat, got %+v", got)
		}
	})

	t.Run("empty filter returns all", func(t *testing.T) {
		got := filterEmoji(emojiMap, "")
		if len(got) != len(emojiMap) {
			t.Errorf("expected %d emoji, got %d", len(emojiMap), len(got))
		}
	})

	t.Run("no match returns nil", func(t *testing.T) {
		got := filterEmoji(emojiMap, "nonexistent")
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("preserves value (URL or alias)", func(t *testing.T) {
		got := filterEmoji(emojiMap, "shipit")
		if len(got) != 1 || got[0].Value != "alias:squirrel" {
			t.Errorf("expected alias:squirrel, got %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// matchUser
// ---------------------------------------------------------------------------

func TestMatchUser(t *testing.T) {
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

	alice := mkUser("U1", "alice", "Alice Smith", "Alice", false, false)
	bob := mkUser("U2", "bob", "Bob Jones", "Bobby", false, false)
	slackbot := mkUser("USLACKBOT", "slackbot", "Slackbot", "", false, false)
	gone := mkUser("U4", "alice_zombie", "Gone Alice", "", false, true)
	aliceBot := mkUser("U5", "alice_bot", "Alice Bot", "AliceBot", true, false)

	t.Run("deleted always excluded", func(t *testing.T) {
		if _, ok := matchUser(gone, "alice", true); ok {
			t.Error("deleted user should be excluded even with includeBots=true and matching name")
		}
	})

	t.Run("bots excluded by default", func(t *testing.T) {
		if _, ok := matchUser(aliceBot, "", false); ok {
			t.Error("bot user should be excluded when includeBots=false")
		}
		if _, ok := matchUser(slackbot, "", false); ok {
			t.Error("USLACKBOT should be excluded by ID even when IsBot=false")
		}
	})

	t.Run("bots included when requested", func(t *testing.T) {
		if _, ok := matchUser(aliceBot, "", true); !ok {
			t.Error("bot user should pass when includeBots=true")
		}
	})

	t.Run("name filter matches Name/RealName/DisplayName", func(t *testing.T) {
		if _, ok := matchUser(alice, "alice", false); !ok {
			t.Error("alice should match by Name")
		}
		if _, ok := matchUser(bob, "jones", false); !ok {
			t.Error("bob should match by RealName 'Bob Jones'")
		}
		if _, ok := matchUser(bob, "bobby", false); !ok {
			t.Error("bob should match by DisplayName 'Bobby'")
		}
		if _, ok := matchUser(bob, "alice", false); ok {
			t.Error("bob should not match 'alice'")
		}
	})

	t.Run("name filter is case-insensitive without caller lowercasing", func(t *testing.T) {
		// matchUser must lowercase the needle internally so callers can
		// pass the user's raw input.
		if _, ok := matchUser(alice, "ALICE", false); !ok {
			t.Error("alice should match 'ALICE' (helper must lowercase)")
		}
		if _, ok := matchUser(bob, "JoNeS", false); !ok {
			t.Error("bob should match 'JoNeS'")
		}
	})

	t.Run("info populated correctly", func(t *testing.T) {
		got, ok := matchUser(alice, "", false)
		if !ok {
			t.Fatal("expected match")
		}
		want := userInfo{
			ID: "U1", Name: "alice", RealName: "Alice Smith", DisplayName: "Alice",
		}
		if got != want {
			t.Errorf("info = %+v, want %+v", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// openUploadPath
// ---------------------------------------------------------------------------

func TestOpenUploadPath(t *testing.T) {
	// Redirect uploadDir to a per-test directory.
	tmpRoot := t.TempDir()
	uploadRoot := filepath.Join(tmpRoot, "kojo-upload")
	if err := os.MkdirAll(uploadRoot, 0o755); err != nil {
		t.Fatalf("mkdir upload root: %v", err)
	}
	orig := uploadDir
	uploadDir = uploadRoot
	t.Cleanup(func() { uploadDir = orig })

	t.Run("regular file inside upload dir is opened", func(t *testing.T) {
		p := filepath.Join(uploadRoot, "ok.txt")
		if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, size, kind := openUploadPath(p)
		if kind != "" {
			t.Fatalf("kind = %q, want empty", kind)
		}
		if f == nil {
			t.Fatal("expected non-nil file")
		}
		t.Cleanup(func() { f.Close() })
		if size != 5 {
			t.Errorf("size = %d, want 5", size)
		}
		// Read through the fd to confirm the data really comes from the
		// validated inode.
		buf := make([]byte, 5)
		if _, err := f.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != "hello" {
			t.Errorf("data = %q, want %q", buf, "hello")
		}
	})

	t.Run("empty path rejected", func(t *testing.T) {
		f, _, kind := openUploadPath("")
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrEmpty {
			t.Errorf("kind = %q, want %q", kind, uploadErrEmpty)
		}
	})

	t.Run("path outside upload dir rejected", func(t *testing.T) {
		outside := filepath.Join(tmpRoot, "outside.txt")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(outside)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		dir := filepath.Join(uploadRoot, "subdir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, _, kind := openUploadPath(dir)
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrIsDir {
			t.Errorf("kind = %q, want %q", kind, uploadErrIsDir)
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		f, _, kind := openUploadPath(filepath.Join(uploadRoot, "does-not-exist"))
		if f != nil {
			f.Close()
			t.Error("expected nil file")
		}
		if kind != uploadErrNotFound {
			t.Errorf("kind = %q, want %q", kind, uploadErrNotFound)
		}
	})

	t.Run("symlink to outside upload dir rejected", func(t *testing.T) {
		secret := filepath.Join(tmpRoot, "secret.txt")
		if err := os.WriteFile(secret, []byte("s3cr3t"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(uploadRoot, "evil-link")
		if err := os.Symlink(secret, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		f, _, kind := openUploadPath(link)
		if f != nil {
			f.Close()
			t.Error("symlink escape: expected rejection but got opened file")
		}
		if kind != uploadErrOutside {
			t.Errorf("kind = %q, want %q", kind, uploadErrOutside)
		}
	})

	t.Run("user message never includes path", func(t *testing.T) {
		// Sanity check: the user-facing strings are fixed and contain
		// no path interpolation.
		for _, kind := range []string{
			uploadErrEmpty, uploadErrInvalid, uploadErrNotFound,
			uploadErrOutside, uploadErrIsDir, uploadErrNotFile,
			uploadErrOpenFail, uploadErrStatFail, uploadErrSwapped,
		} {
			msg := uploadPathUserMessage(kind)
			if strings.Contains(msg, "/") || strings.Contains(msg, `\`) {
				t.Errorf("kind %q produced message containing path separator: %q", kind, msg)
			}
			if msg == "" {
				t.Errorf("kind %q produced empty message", kind)
			}
		}
	})
}

func TestToMessageInfo(t *testing.T) {
	t.Run("minimal user message preserves user/text keys", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U123",
			Text:      "hello",
			Timestamp: "1700000000.000100",
		}, nil)
		if got.User != "U123" || got.Text != "hello" || got.Timestamp != "1700000000.000100" {
			t.Fatalf("basic fields lost: %+v", got)
		}
		// Confirm JSON shape: "user" and "text" must always be present
		// (legacy consumers may rely on these keys existing) while richer
		// optional fields stay omitted.
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, mustHave := range []string{`"user":"U123"`, `"text":"hello"`, `"timestamp":"1700000000.000100"`} {
			if !strings.Contains(s, mustHave) {
				t.Errorf("expected %q in %s", mustHave, s)
			}
		}
		for _, mustNot := range []string{`"files"`, `"reactions"`, `"attachments"`, `"blocks"`, `"metadata"`, `"edited"`, `"bot_id"`, `"username"`, `"app_id"`} {
			if strings.Contains(s, mustNot) {
				t.Errorf("did not expect %q in %s", mustNot, s)
			}
		}
	})

	t.Run("bot message exposes bot_id/username/app_id", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			Type:      "message",
			SubType:   "bot_message",
			BotID:     "B123",
			Username:  "ReleaseBot",
			Text:      "",
			Timestamp: "1700000001.000200",
			BotProfile: &slack.BotProfile{
				AppID: "A999",
			},
		}, nil)
		if got.BotID != "B123" || got.Username != "ReleaseBot" || got.AppID != "A999" {
			t.Fatalf("bot fields lost: %+v", got)
		}
		if got.SubType != "bot_message" {
			t.Errorf("subtype lost")
		}
		// User key still emitted as empty string for compatibility.
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), `"user":""`) {
			t.Errorf("expected empty user key in %s", raw)
		}
	})

	t.Run("files attachments reactions edited preserved", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "see file",
			Timestamp: "1700000002.000300",
			Edited:    &slack.Edited{User: "U1", Timestamp: "1700000003.000400"},
			Files: []slack.File{{
				ID:                 "F1",
				Name:               "report.pdf",
				Title:              "Q4 report",
				Mimetype:           "application/pdf",
				Filetype:           "pdf",
				PrettyType:         "PDF",
				Size:               4096,
				URLPrivate:         "https://files.slack.com/private/F1",
				URLPrivateDownload: "https://files.slack.com/private/F1/download",
				Permalink:          "https://workspace.slack.com/files/U1/F1",
				PermalinkPublic:    "https://slack-files.com/pub/F1",
				User:               "U1",
			}},
			Reactions: []slack.ItemReaction{{
				Name:  "thumbsup",
				Count: 2,
				Users: []string{"U2", "U3"},
			}},
			Attachments: []slack.Attachment{{
				Color:       "good",
				Fallback:    "fb",
				Title:       "Build #42",
				TitleLink:   "https://ci.example.com/42",
				Pretext:     "ok",
				Text:        "passed",
				AuthorName:  "ci",
				AuthorLink:  "https://ci.example.com",
				ServiceName: "CI",
				FromURL:     "https://ci.example.com/42",
				ImageURL:    "https://ci.example.com/42.png",
				ThumbURL:    "https://ci.example.com/42-thumb.png",
				Footer:      "ci-bot",
				Fields: []slack.AttachmentField{
					{Title: "branch", Value: "main", Short: true},
				},
			}},
		}, nil)
		if got.Edited == nil || got.Edited.User != "U1" || got.Edited.Timestamp != "1700000003.000400" {
			t.Errorf("edited info lost: %+v", got.Edited)
		}
		if len(got.Files) != 1 || got.Files[0].ID != "F1" || got.Files[0].URLPrivate == "" || got.Files[0].Permalink == "" {
			t.Errorf("files lost: %+v", got.Files)
		}
		if len(got.Reactions) != 1 || got.Reactions[0].Name != "thumbsup" || got.Reactions[0].Count != 2 || !reflect.DeepEqual(got.Reactions[0].Users, []string{"U2", "U3"}) {
			t.Errorf("reactions lost: %+v", got.Reactions)
		}
		if len(got.Attachments) != 1 {
			t.Fatalf("attachments lost: %+v", got.Attachments)
		}
		att := got.Attachments[0]
		if att.Title != "Build #42" || att.Color != "good" || att.Footer != "ci-bot" || att.ImageURL == "" {
			t.Errorf("attachment scalar fields lost: %+v", att)
		}
		if len(att.Fields) != 1 || att.Fields[0].Title != "branch" || att.Fields[0].Value != "main" || !att.Fields[0].Short {
			t.Errorf("attachment fields lost: %+v", att.Fields)
		}
	})

	t.Run("block kit blocks are re-encoded as JSON", func(t *testing.T) {
		section := slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*hello*", false, false),
			nil, nil,
		)
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "fallback text",
			Timestamp: "1700000004.000500",
			Blocks:    slack.Blocks{BlockSet: []slack.Block{section}},
		}, nil)
		if len(got.Blocks) == 0 {
			t.Fatalf("blocks dropped")
		}
		var decoded []map[string]any
		if err := json.Unmarshal(got.Blocks, &decoded); err != nil {
			t.Fatalf("blocks not valid JSON: %v (%s)", err, got.Blocks)
		}
		if len(decoded) != 1 || decoded[0]["type"] != "section" {
			t.Fatalf("expected single section block, got %s", got.Blocks)
		}
		raw := string(got.Blocks)
		if !strings.Contains(raw, `*hello*`) {
			t.Errorf("section block content missing: %s", raw)
		}
	})

	t.Run("metadata passed through when present", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "evt",
			Timestamp: "1700000005.000600",
			Metadata: slack.SlackMetadata{
				EventType:    "deploy_finished",
				EventPayload: map[string]any{"service": "kojo", "ok": true},
			},
		}, nil)
		if got.Metadata == nil || got.Metadata.EventType != "deploy_finished" {
			t.Fatalf("metadata lost: %+v", got.Metadata)
		}
		if v, ok := got.Metadata.EventPayload["service"].(string); !ok || v != "kojo" {
			t.Errorf("event payload lost: %+v", got.Metadata.EventPayload)
		}
	})

	t.Run("absent metadata stays nil", func(t *testing.T) {
		got := toMessageInfo(slack.Msg{
			User:      "U1",
			Text:      "no meta",
			Timestamp: "1700000006.000700",
		}, nil)
		if got.Metadata != nil {
			t.Errorf("expected nil metadata, got %+v", got.Metadata)
		}
	})
}
