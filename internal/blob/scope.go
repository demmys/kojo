// Package blob is the native blob store described in
// docs/multi-device-storage.md §2.4 and §4.2. v1 phase 3 slice 1 ships a
// filesystem-only implementation: Get / Head / Put / Delete / List with
// atomic publish (temp + fsync + rename + parent dir fsync) plus content
// digest verification on Put. The blob_refs DB cache and the HTTP
// transport are deliberately not wired here — slice 2 (DB integration)
// and slice 3 (HTTP handler) layer on top of this package.
package blob

import (
	"errors"
	"path/filepath"
)

// Scope partitions blobs by placement / sharing policy. The three
// values map 1-1 to the on-disk subtrees of the kojo config dir:
//
//	<root>/global/   — shareable across peers; the holder serves reads
//	                   on demand (live_read read-through), not replicated
//	<root>/local/    — peer-local only (FTS index, transient temp)
//	<root>/machine/  — never leaves the host (credentials, machine secrets)
//
// MISTAKE: `cas` (content-addressed storage). It was added to the
// blob_refs CHECK in migrations/0001_initial.sql on the speculation
// that large files (models / datasets) would one day be partially
// replicated by content hash. That feature was never built and is not
// planned — the multi-device design settled on "the holder serves
// reads on demand" (proxy / live_read), so nothing is content-addressed
// and nothing is partially replicated. Admitting a value for a
// non-existent code path was the error: it implied a capability the
// stack does not have.
//
// It is intentionally NOT a member of this enum and Valid() rejects it,
// so every write path (BuildURI → Valid) refuses scope='cas'. No row
// with scope='cas' can ever be created; the CHECK literal is therefore
// dead and unreachable.
//
// It is left in the CHECK rather than removed because removing it buys
// nothing and costs real risk: SQLite cannot ALTER a CHECK, so dropping
// the literal needs a full blob_refs table-rebuild migration (create a
// twin table, copy every row, drop, rename, recreate indexes) on a core
// table — pure churn for a value already proven unreachable in code.
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeLocal   Scope = "local"
	ScopeMachine Scope = "machine"
)

// ErrInvalidScope is returned when a caller passes a Scope outside the
// three valid values.
var ErrInvalidScope = errors.New("blob: invalid scope")

// Valid reports whether s is one of the three accepted scopes.
func (s Scope) Valid() bool {
	switch s {
	case ScopeGlobal, ScopeLocal, ScopeMachine:
		return true
	}
	return false
}

// resolveDir joins root with the scope's on-disk subdir name. Callers
// are expected to have validated `s` first; an invalid scope here
// returns "" so a downstream filepath.Join can't accidentally produce a
// path under root and write outside the scope sandbox.
func resolveDir(root string, s Scope) string {
	if !s.Valid() {
		return ""
	}
	return filepath.Join(root, string(s))
}
