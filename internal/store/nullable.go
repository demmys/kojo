package store

import (
	"database/sql"
	"encoding/json"
)

// This file collects the small NULL-coercion helpers the store's row
// writers share. Each converts a Go zero value ("" / 0 / nil / empty
// bytes) into the database/sql value that persists as SQL NULL, so a
// "missing" field round-trips as NULL rather than the column's zero
// literal — a distinction the canonical etag contract depends on. The
// helpers keep distinct signatures on purpose (plain vs pointer, text
// vs int vs JSON) because their zero-vs-NULL rules differ per column.

// nullableText emits SQL NULL for the empty string and the string value
// otherwise.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableTextPtr converts *string to a database/sql NULL-aware value.
// Distinct from nullableText (which takes a plain string and emits NULL
// for ""): a session with AgentID=nil legitimately means "detached",
// while a session with AgentID=&"" would be a malformed row that we
// surface as NULL too rather than persisting an empty-string FK target.
func nullableTextPtr(p *string) any {
	if p == nil || *p == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

// nullableInt64 returns SQL NULL for zero so the column stores NULL
// rather than 0 — preserves the schema's "missing" semantic for
// last_seen_ok / marked_for_gc_at.
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableInt64Ptr converts an optional *int64 into the form database/sql
// understands. Returns sql.NullInt64{Valid:false} when the pointer is nil
// so the column is written as SQL NULL rather than 0.
func nullableInt64Ptr(p *int64) any {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// nullableBool returns SQL NULL for false and 1 for true — preserves the
// schema's "missing" semantic for flag columns that are NULL on rows
// written before the column existed (groupdm_messages.interrupted).
func nullableBool(v bool) any {
	if !v {
		return nil
	}
	return 1
}

// nullableJSON converts an empty / nil json.RawMessage to a SQL NULL.
// Mirrors nullableText for raw-JSON columns (tool_uses, attachments,
// usage on agent_messages) so a missing field round-trips as NULL
// instead of the literal string "null" — distinguishing "no row data"
// from "explicit JSON null" is part of the canonical etag contract.
func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return string(v)
}

// nullableRaw converts an empty / nil json.RawMessage to SQL NULL,
// passing the bytes through as []byte otherwise.
func nullableRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// jsonOrNil は空 / nil バイト列を SQL NULL に落とす。`{}` や `[]` 等の
// 「空だが有効な JSON」はそのまま保存する (canonical 側と同じく
// 「絶対に空でない」JSON は touch しない方針)。
func jsonOrNil(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
