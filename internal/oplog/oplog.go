// Package oplog defines the bounded per-agent operation log record
// format described in docs/multi-device-storage.md §3.13.1.
//
// The op-log captures agent-runtime writes when the peer cannot reach
// the Hub. Only the on-the-wire Entry record format lives here; the
// HTTP flush handler (internal/server/oplog_handler.go) consumes it.
package oplog

import "encoding/json"

// Entry is the JSON-Lines record format for one op-log entry. The
// serialization is canonical (lower-camel keys) so a future Hub-side
// re-parser produces byte-identical bodies even when the producer
// language differs.
type Entry struct {
	OpID         string          `json:"op_id"`
	AgentID      string          `json:"agent_id"`
	FencingToken int64           `json:"fencing_token"`
	Seq          int64           `json:"seq"`
	Table        string          `json:"table"`
	Op           string          `json:"op"` // insert | update | delete
	Body         json.RawMessage `json:"body"`
	ClientTS     int64           `json:"client_ts"` // unix millis
}
