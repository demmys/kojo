package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// geminiThoughtPollInterval is how often the session file is re-read for
// newly completed model steps while a turn runs.
const geminiThoughtPollInterval = 300 * time.Millisecond

// geminiThoughtTail surfaces the model's thought summaries, which
// gemini-cli's stream-json output never carries (its event types are
// init/message/tool_use/tool_result/error/result only). The CLI does,
// however, append every completed model step to the session JSONL
// ($GEMINI_CLI_HOME/.gemini/tmp/<cwd>/chats/session-*-<id8>.jsonl) as
//
//	{"id":..., "timestamp":..., "type":"gemini", "content":..., "thoughts":[{"subject","description",...}], ...}
//
// and re-appends the same id when it updates the record (e.g. adding
// toolCalls), interleaved with {"$set":{...}} metadata lines. The tail
// reads the file incrementally, keeps an unterminated last line for the
// next read, and emits each thought once per record id.
//
// Safe for concurrent use: the poll goroutine and the final drain share
// the state under mu.
type geminiThoughtTail struct {
	mu        sync.Mutex
	dir       string
	since     time.Time // records stamped before this belong to earlier turns
	sessionID string
	path      string
	fileInfo  os.FileInfo // identity of the file offset refers to
	offset    int64
	partial   []byte
	emitted   map[string]int // record id → thoughts already emitted
	buf       strings.Builder
}

// newGeminiThoughtTail prepares a tail for a turn starting at since. For a
// resumed session the existing file's current size becomes the start
// offset, so earlier turns' thoughts are never re-read.
func newGeminiThoughtTail(dir, resumeID string, since time.Time) *geminiThoughtTail {
	t := &geminiThoughtTail{dir: dir, since: since, emitted: map[string]int{}}
	if resumeID != "" {
		t.sessionID = resumeID
		if files := geminiSessionFiles(dir, resumeID); len(files) > 0 {
			t.path = files[0]
			if fi, err := os.Stat(t.path); err == nil {
				t.fileInfo = fi
				t.offset = geminiLastLineEnd(t.path, fi.Size())
			}
		}
	}
	return t
}

// setSession records the session id announced by the init event.
func (t *geminiThoughtTail) setSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id != t.sessionID {
		t.sessionID, t.path, t.fileInfo, t.offset, t.partial = id, "", nil, 0, nil
	}
}

// thinking returns everything emitted so far, for the final message.
func (t *geminiThoughtTail) thinking() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

type geminiSessionRecord struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Thoughts  []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	} `json:"thoughts"`
}

// poll reads whatever was appended since the last call and emits new
// thoughts through send. Errors (file not created yet, transient read
// failure) are silently retried on the next poll.
func (t *geminiThoughtTail) poll(send func(ChatEvent) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessionID == "" {
		return
	}
	if t.path == "" {
		files := geminiSessionFiles(t.dir, t.sessionID)
		if len(files) == 0 {
			return
		}
		t.path = files[0]
	}
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		if fi.Size() < t.offset || (t.fileInfo != nil && !os.SameFile(fi, t.fileInfo)) {
			// Truncated or replaced: start over. Per-id dedup and the
			// timestamp floor keep already-shown thoughts from repeating.
			t.offset, t.partial = 0, nil
		}
		t.fileInfo = fi
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return
	}
	t.offset += int64(len(data))
	if len(t.partial) > 0 {
		data = append(t.partial, data...)
		t.partial = nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	if last := lines[len(lines)-1]; len(last) > 0 {
		t.partial = append([]byte(nil), last...)
	}
	for _, line := range lines[:len(lines)-1] {
		t.handleLine(line, send)
	}
}

func (t *geminiThoughtTail) handleLine(line []byte, send func(ChatEvent) bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.Contains(line, []byte(`"thoughts"`)) {
		return
	}
	var rec geminiSessionRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type != "gemini" || rec.ID == "" {
		return
	}
	if ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && ts.Before(t.since) {
		return
	}
	for i := t.emitted[rec.ID]; i < len(rec.Thoughts); i++ {
		th := rec.Thoughts[i]
		subject := strings.TrimSpace(th.Subject)
		desc := strings.TrimSpace(th.Description)
		var delta string
		switch {
		case subject != "" && desc != "":
			delta = "**" + subject + "**\n" + desc + "\n\n"
		case subject != "":
			delta = "**" + subject + "**\n\n"
		case desc != "":
			delta = desc + "\n\n"
		default:
			continue
		}
		t.buf.WriteString(delta)
		send(ChatEvent{Type: "thinking", Delta: delta})
	}
	if len(rec.Thoughts) > t.emitted[rec.ID] {
		t.emitted[rec.ID] = len(rec.Thoughts)
	}
}

// run polls until stop is closed. The caller must wait for done before
// the final drain / done event so no thinking event trails "done".
func (t *geminiThoughtTail) run(stop <-chan struct{}, done chan<- struct{}, send func(ChatEvent) bool) {
	defer close(done)
	ticker := time.NewTicker(geminiThoughtPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			t.poll(send)
		}
	}
}

// geminiLastLineEnd returns the offset just past the last newline in the
// first size bytes of path (0 if there is none), so a resumed tail starts
// on a line boundary even if the previous run left an unterminated line
// behind. Scans backward in chunks; falls back to size on read errors.
func geminiLastLineEnd(path string, size int64) int64 {
	const chunk = 64 << 10
	f, err := os.Open(path)
	if err != nil {
		return size
	}
	defer f.Close()
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		b := buf[:end-start]
		if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
			return size
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return start + int64(i) + 1
		}
		end = start
	}
	return 0
}
