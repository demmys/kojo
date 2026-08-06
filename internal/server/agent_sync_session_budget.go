package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/loppo-llc/kojo/internal/agent"
)

// fitAgentSyncSessions keeps every non-session row intact and fills the
// single-shot decompressed JSON budget with native session artifacts in the
// order supplied by the backend readers (newest activity first). If the
// history/metadata portion already exceeds the cap, all session artifacts are
// omitted and the caller will use chunked sync for the non-droppable rows.
func fitAgentSyncSessions(req *peerAgentSyncRequest, maxRawBytes int64) (kept, skipped int, err error) {
	if req == nil || maxRawBytes <= 0 {
		return 0, 0, nil
	}

	type artifact struct {
		kind    string
		path    string
		size    int64
		claude  claudeSessionWire
		codex   codexThreadWire
		grok    grokSessionWire
		hasGrok bool
	}
	artifacts := make([]artifact, 0, len(req.ClaudeSessions))
	for i := range req.ClaudeSessions {
		v := req.ClaudeSessions[i]
		artifacts = append(artifacts, artifact{
			kind: "claude", path: v.SessionID + ".jsonl",
			size: int64(base64.StdEncoding.DecodedLen(len(v.ContentB64))), claude: v,
		})
	}
	if req.CodexSession != nil {
		for i := range req.CodexSession.Threads {
			v := req.CodexSession.Threads[i]
			artifacts = append(artifacts, artifact{
				kind: "codex", path: v.RolloutRelPath,
				size: int64(base64.StdEncoding.DecodedLen(len(v.RolloutContentB64))), codex: v,
			})
		}
	}
	if req.GrokSession != nil {
		var size int64
		for _, f := range req.GrokSession.Files {
			size += int64(base64.StdEncoding.DecodedLen(len(f.ContentB64)))
		}
		v := *req.GrokSession
		artifacts = append(artifacts, artifact{
			kind: "grok", path: "grok-session/" + v.SessionID, size: size, grok: v, hasGrok: true,
		})
	}
	if len(artifacts) == 0 {
		return 0, 0, nil
	}

	// Rebuild from the selected prefix/subset so every estimate includes the
	// exact JSON/base64 overhead rather than guessing from on-disk sizes.
	req.ClaudeSessions = nil
	req.CodexSession = nil
	req.GrokSession = nil
	selected := make([]artifact, 0, len(artifacts))
	rejected := make([]artifact, 0)
	apply := func(items []artifact) {
		req.ClaudeSessions = nil
		req.CodexSession = nil
		req.GrokSession = nil
		for _, a := range items {
			switch a.kind {
			case "claude":
				req.ClaudeSessions = append(req.ClaudeSessions, a.claude)
			case "codex":
				if req.CodexSession == nil {
					req.CodexSession = &codexSessionWire{}
				}
				req.CodexSession.Threads = append(req.CodexSession.Threads, a.codex)
			case "grok":
				if a.hasGrok {
					v := a.grok
					req.GrokSession = &v
				}
			}
		}
	}

	// Measure the non-session request exactly, one row at a time. Building the
	// complete canonical-history JSON here would briefly allocate a second
	// 128+ MiB buffer precisely when the caller needs to choose chunked sync.
	baseSize, err := exactAgentSyncNonSessionJSONSize(req)
	if err != nil {
		return 0, 0, fmt.Errorf("measure non-session agent-sync payload: %w", err)
	}
	originalSkipCount := len(req.TransferSkips)
	type measuredArtifact struct {
		artifact
		sessionJSONBytes int64
		skip             agent.SkippedSessionFile
		skipJSONBytes    int64
	}
	measured := make([]measuredArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		var encoded []byte
		var e error
		switch a.kind {
		case "claude":
			encoded, e = json.Marshal(a.claude)
		case "codex":
			encoded, e = json.Marshal(a.codex)
		case "grok":
			encoded, e = json.Marshal(a.grok)
		}
		if e != nil {
			return 0, 0, fmt.Errorf("measure session artifact %s: %w", a.path, e)
		}
		skip := agent.SkippedSessionFile{Path: a.path, Reason: "capacity", SizeBytes: a.size}
		skipJSON, e := json.Marshal(skip)
		if e != nil {
			return 0, 0, fmt.Errorf("measure session skip %s: %w", a.path, e)
		}
		measured = append(measured, measuredArtifact{
			artifact: a, sessionJSONBytes: int64(len(encoded)), skip: skip, skipJSONBytes: int64(len(skipJSON)),
		})
	}

	// Start with every artifact represented as a capacity skip. Selecting one
	// swaps that small skip element for its native-session JSON. This reserves
	// skip metadata up front and makes the greedy newest-first decision exact.
	currentSize := baseSize
	remainingSkips := len(measured)
	if remainingSkips > 0 {
		if originalSkipCount == 0 {
			currentSize += int64(len(`,"transfer_skips":[`) + 1) // field prefix + closing ]
		}
		for _, a := range measured {
			currentSize += a.skipJSONBytes + 1 // comma (or first-element allowance)
		}
		if originalSkipCount == 0 {
			currentSize-- // first element has no comma
		}
	}
	selectedByKind := map[string]int{}
	for _, a := range measured {
		removeSkipBytes := a.skipJSONBytes + 1
		if originalSkipCount == 0 && remainingSkips == 1 {
			removeSkipBytes = int64(len(`,"transfer_skips":[`)+1) + a.skipJSONBytes
		}
		var addSessionBytes int64
		switch a.kind {
		case "claude":
			if selectedByKind[a.kind] == 0 {
				addSessionBytes = int64(len(`,"claude_sessions":[`)+1) + a.sessionJSONBytes
			} else {
				addSessionBytes = 1 + a.sessionJSONBytes
			}
		case "codex":
			if selectedByKind[a.kind] == 0 {
				addSessionBytes = int64(len(`,"codex_session":{"threads":[`)+2) + a.sessionJSONBytes
			} else {
				addSessionBytes = 1 + a.sessionJSONBytes
			}
		case "grok":
			addSessionBytes = int64(len(`,"grok_session":`)) + a.sessionJSONBytes
		}
		if baseSize < maxRawBytes && currentSize-removeSkipBytes+addSessionBytes <= maxRawBytes {
			selected = append(selected, a.artifact)
			selectedByKind[a.kind]++
			currentSize = currentSize - removeSkipBytes + addSessionBytes
			remainingSkips--
			continue
		}
		rejected = append(rejected, a.artifact)
		req.TransferSkips = append(req.TransferSkips, a.skip)
	}
	apply(selected)
	return len(selected), len(rejected), nil
}

// exactAgentSyncNonSessionJSONSize returns the exact encoding/json byte length
// of req with all native-session fields absent, without materialising the full
// top-level object or any large history array. Its inclusion checks mirror the
// peerAgentSyncRequest json tags (three required fields, then omitempty).
func exactAgentSyncNonSessionJSONSize(req *peerAgentSyncRequest) (int64, error) {
	if req == nil {
		return 0, fmt.Errorf("nil request")
	}
	total := int64(2) // {}
	fieldCount := 0
	addFieldBytes := func(name string, valueBytes int64) {
		if fieldCount > 0 {
			total++ // comma between top-level fields
		}
		total += int64(len(name)+3) + valueBytes // quoted ASCII name + colon
		fieldCount++
	}
	addValue := func(name string, value any) error {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		addFieldBytes(name, int64(len(b)))
		return nil
	}
	addArray := func(name string, n int, marshalAt func(int) ([]byte, error)) error {
		arrayBytes := int64(2) // []
		for i := 0; i < n; i++ {
			b, err := marshalAt(i)
			if err != nil {
				return err
			}
			arrayBytes += int64(len(b))
			if i > 0 {
				arrayBytes++
			}
		}
		addFieldBytes(name, arrayBytes)
		return nil
	}

	if err := addValue("source_device_id", req.SourceDeviceID); err != nil {
		return 0, err
	}
	if err := addValue("op_id", req.OpID); err != nil {
		return 0, err
	}
	if err := addValue("agent", req.Agent); err != nil {
		return 0, err
	}
	if req.Persona != nil {
		if err := addValue("persona", req.Persona); err != nil {
			return 0, err
		}
	}
	if req.Memory != nil {
		if err := addValue("memory", req.Memory); err != nil {
			return 0, err
		}
	}
	if len(req.Messages) > 0 {
		if err := addArray("messages", len(req.Messages), func(i int) ([]byte, error) { return json.Marshal(req.Messages[i]) }); err != nil {
			return 0, err
		}
	}
	if len(req.MemoryEntries) > 0 {
		if err := addArray("memory_entries", len(req.MemoryEntries), func(i int) ([]byte, error) { return json.Marshal(req.MemoryEntries[i]) }); err != nil {
			return 0, err
		}
	}
	if len(req.WorkspaceFiles) > 0 {
		if err := addArray("workspace_files", len(req.WorkspaceFiles), func(i int) ([]byte, error) { return json.Marshal(req.WorkspaceFiles[i]) }); err != nil {
			return 0, err
		}
	}
	if len(req.Tasks) > 0 {
		if err := addArray("tasks", len(req.Tasks), func(i int) ([]byte, error) { return json.Marshal(req.Tasks[i]) }); err != nil {
			return 0, err
		}
	}
	if req.AgentToken != "" {
		if err := addValue("agent_token", req.AgentToken); err != nil {
			return 0, err
		}
	}
	if req.SinceMessageSeq != 0 {
		if err := addValue("since_message_seq", req.SinceMessageSeq); err != nil {
			return 0, err
		}
	}
	if req.SinceMemoryEntrySeq != 0 {
		if err := addValue("since_memory_entry_seq", req.SinceMemoryEntrySeq); err != nil {
			return 0, err
		}
	}
	if req.SinceMemoryEntryUpdatedAt != 0 {
		if err := addValue("since_memory_entry_updated_at", req.SinceMemoryEntryUpdatedAt); err != nil {
			return 0, err
		}
	}
	if req.Credentials != nil {
		// Marshal the pointer itself so a non-nil pointer to a nil slice retains
		// encoding/json's `null` representation rather than being counted as [].
		// Credential sets are intentionally tiny, so this does not affect the
		// large-transcript allocation bound.
		if err := addValue("credentials", req.Credentials); err != nil {
			return 0, err
		}
	}
	if len(req.DegradedFlushes) > 0 {
		if err := addArray("degraded_flushes", len(req.DegradedFlushes), func(i int) ([]byte, error) { return json.Marshal(req.DegradedFlushes[i]) }); err != nil {
			return 0, err
		}
	}
	if len(req.TransferSkips) > 0 {
		if err := addArray("transfer_skips", len(req.TransferSkips), func(i int) ([]byte, error) { return json.Marshal(req.TransferSkips[i]) }); err != nil {
			return 0, err
		}
	}
	return total, nil
}
