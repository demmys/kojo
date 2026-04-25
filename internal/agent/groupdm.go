package agent

import (
	"os"
	"path/filepath"
	"time"
)

// GroupDM represents a group conversation between agents.
// GroupDMStyle controls the communication style for a group conversation.
// "efficient" (default): concise, token-saving, no pleasantries.
// "expressive": human-like chat with greetings and conversational filler.
type GroupDMStyle string

const (
	GroupDMStyleEfficient  GroupDMStyle = "efficient"
	GroupDMStyleExpressive GroupDMStyle = "expressive"
)

// ValidGroupDMStyles is the set of accepted style values.
var ValidGroupDMStyles = map[GroupDMStyle]bool{
	GroupDMStyleEfficient:  true,
	GroupDMStyleExpressive: true,
}

// NotifyMode controls how a specific member receives group-DM notifications.
//
//   - "realtime" (default): notify as soon as the group-level cooldown allows.
//   - "digest":  collect messages for up to DigestWindow seconds (or the group
//     cooldown, whichever is larger) before delivering a single batched turn.
//   - "muted":   do not notify this member at all. The member can still read
//     messages via the API on their own initiative.
type NotifyMode string

const (
	NotifyRealtime NotifyMode = "realtime"
	NotifyDigest   NotifyMode = "digest"
	NotifyMuted    NotifyMode = "muted"
)

// ValidNotifyModes is the set of accepted notify-mode values.
var ValidNotifyModes = map[NotifyMode]bool{
	NotifyRealtime: true,
	NotifyDigest:   true,
	NotifyMuted:    true,
}

// defaultDigestWindow is the fallback digest window when a member opts into
// "digest" mode without specifying DigestWindow explicitly.
const defaultDigestWindow = 300 // 5 minutes

// maxDigestWindow caps the digest window to 1 hour.
const maxDigestWindow = 3600

// GroupDMVenue is the physical/virtual setting that hosts the conversation.
// Agents use this hint to calibrate speech style: a co-located venue invites
// references to shared surroundings and gestures, while a chat room
// constrains everything to the text channel.
//
//   - "chatroom" (default): closed online chat room. Members are not
//     physically together; the only shared context is what is sent here.
//   - "colocated": same physical space. Members are co-present in real
//     time and may reference ambient cues, gestures, deictic ('this',
//     'over there') language.
type GroupDMVenue string

const (
	GroupDMVenueChatroom  GroupDMVenue = "chatroom"
	GroupDMVenueColocated GroupDMVenue = "colocated"
)

// ValidGroupDMVenues is the set of accepted venue values.
var ValidGroupDMVenues = map[GroupDMVenue]bool{
	GroupDMVenueChatroom:  true,
	GroupDMVenueColocated: true,
}

// defaultGroupDMVenue is what gets stamped onto a group when the field is
// empty (legacy data, callers omitting the parameter, etc.). We default to
// chatroom because that matches the existing token-saving DM design — a
// co-located venue is opt-in.
const defaultGroupDMVenue = GroupDMVenueChatroom

type GroupDM struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Members  []GroupMember `json:"members"`
	Cooldown int           `json:"cooldown"` // notification cooldown in seconds (0 = use default)
	Style    GroupDMStyle  `json:"style"`    // communication style: "efficient" or "expressive"
	// Venue is the physical setting hint. "chatroom" (default) for a closed
	// online chat, "colocated" when members are co-present in real space.
	Venue     GroupDMVenue `json:"venue,omitempty"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
}

// GroupMember is a participant in a group DM.
type GroupMember struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	// NotifyMode is the per-member delivery mode. Empty string is treated as
	// NotifyRealtime on read but omitted from JSON to keep legacy groups small.
	NotifyMode NotifyMode `json:"notifyMode,omitempty"`
	// DigestWindow is the digest-batching window in seconds. Only meaningful
	// when NotifyMode == NotifyDigest. 0 means "use defaultDigestWindow".
	DigestWindow int `json:"digestWindow,omitempty"`
}

// GroupMessage is a single message in a group DM transcript.
type GroupMessage struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func generateGroupID() string {
	return generatePrefixedID("gd_")
}

func generateGroupMessageID() string {
	return generatePrefixedID("gm_")
}

// groupdmsDir returns the base directory for group DM data.
func groupdmsDir() string {
	return filepath.Join(agentsDir(), "groupdms")
}

// groupDir returns the directory for a specific group.
func groupDir(groupID string) string {
	return filepath.Join(groupdmsDir(), groupID)
}

// appendGroupMessage appends a message to a group's JSONL transcript.
func appendGroupMessage(groupID string, msg *GroupMessage) error {
	dir := groupDir(groupID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return jsonlAppend(filepath.Join(dir, messagesFile), msg)
}

// loadGroupMessages reads messages from a group's JSONL transcript with
// pagination, plus the current head ID derived from the *same* on-disk
// read. Returning the head from the same snapshot is what lets the
// HTTP-level GET expose a `latestMessageId` that is guaranteed to be
// consistent with the `messages` slice — without that, a concurrent
// PostMessage between two separate reads could surface a head that is
// not represented in either `messages` or the cache.
//
// head is "" when the transcript is empty or missing.
func loadGroupMessages(groupID string, limit int, before string) ([]*GroupMessage, bool, string, error) {
	path := filepath.Join(groupDir(groupID), messagesFile)
	all, _, err := jsonlLoadPaginated(path, 0, "", func(m *GroupMessage) string { return m.ID })
	if err != nil {
		return nil, false, "", err
	}
	for _, m := range all {
		m.Timestamp = normalizeTimestamp(m.Timestamp)
	}

	head := ""
	if len(all) > 0 {
		head = all[len(all)-1].ID
	}

	// Apply `before` cursor: keep only entries strictly older than the
	// given ID. Mirrors the original jsonlLoadPaginated behaviour.
	if before != "" {
		idx := -1
		for i, v := range all {
			if v.ID == before {
				idx = i
				break
			}
		}
		if idx >= 0 {
			all = all[:idx]
		}
	}

	hasMore := false
	if limit > 0 && len(all) > limit {
		hasMore = true
		all = all[len(all)-limit:]
	}
	return all, hasMore, head, nil
}

// loadLatestGroupMessageID returns the ID of the newest message in a group's
// transcript. Returns ("", nil) if the file does not exist or is empty —
// callers treat that as "no head yet" rather than an error so brand-new
// groups stay consistent with on-disk state.
func loadLatestGroupMessageID(groupID string) (string, error) {
	_, _, head, err := loadGroupMessages(groupID, 0, "")
	return head, err
}

// loadGroupMessagesAfter returns messages strictly newer than afterID, capped
// to the newest `limit` entries. hasMore is true when older diff entries had
// to be dropped to fit the cap (so the caller can hint that the full
// transcript is needed).
//
// If afterID is empty, returns the newest `limit` messages from the
// transcript. If afterID is not found in the transcript, the caller-supplied
// cursor is treated as stale: the function returns the newest `limit`
// messages with hasMore=true so the caller can render "we couldn't locate
// your cursor, here's the latest state."
func loadGroupMessagesAfter(groupID, afterID string, limit int) ([]*GroupMessage, bool, error) {
	path := filepath.Join(groupDir(groupID), messagesFile)
	all, _, err := jsonlLoadPaginated(path, 0, "", func(m *GroupMessage) string { return m.ID })
	if err != nil {
		return nil, false, err
	}
	for _, m := range all {
		m.Timestamp = normalizeTimestamp(m.Timestamp)
	}

	if afterID == "" {
		hasMore := false
		if limit > 0 && len(all) > limit {
			hasMore = true
			all = all[len(all)-limit:]
		}
		return all, hasMore, nil
	}

	idx := -1
	for i, m := range all {
		if m.ID == afterID {
			idx = i
			break
		}
	}
	var diff []*GroupMessage
	hasMore := false
	if idx < 0 {
		// afterID is unknown (cursor older than the file remembers, or just
		// wrong). Fall back to the newest `limit` and flag hasMore.
		diff = all
		hasMore = true
	} else {
		diff = all[idx+1:]
	}
	if limit > 0 && len(diff) > limit {
		hasMore = true
		diff = diff[len(diff)-limit:]
	}
	return diff, hasMore, nil
}

func newGroupMessage(agentID, agentName, content string) *GroupMessage {
	return &GroupMessage{
		ID:        generateGroupMessageID(),
		AgentID:   agentID,
		AgentName: agentName,
		Content:   content,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}
