package agent

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	indexDir     = "index"
	indexDBFile  = "memory.db"
	maxResults   = 10
	maxContextLen = 4000
)

// MemoryIndex provides FTS5-based keyword search across agent memory.
type MemoryIndex struct {
	mu     sync.Mutex
	db     *sql.DB
	logger *slog.Logger
}

// OpenMemoryIndex opens or creates the FTS5 index for an agent.
func OpenMemoryIndex(agentID string, logger *slog.Logger) (*MemoryIndex, error) {
	dir := filepath.Join(agentDir(agentID), indexDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}

	dbPath := filepath.Join(dir, indexDBFile)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open index db: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	// Create FTS5 virtual table
	if _, err := db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
			source,
			content,
			timestamp,
			tokenize='unicode61'
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create FTS table: %w", err)
	}

	// Track last index time per source
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS index_meta (
			source TEXT PRIMARY KEY,
			indexed_at TEXT NOT NULL
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create meta table: %w", err)
	}

	return &MemoryIndex{db: db, logger: logger}, nil
}

// Close closes the database.
func (idx *MemoryIndex) Close() error {
	if idx.db == nil {
		return nil
	}
	return idx.db.Close()
}

// IndexMessages indexes messages from the JSONL transcript.
func (idx *MemoryIndex) IndexMessages(agentID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	msgs, err := loadMessages(agentID, 0) // load all
	if err != nil {
		return err
	}

	// Clear existing message entries and re-index
	if _, err := idx.db.Exec("DELETE FROM memory_fts WHERE source = 'message'"); err != nil {
		return err
	}

	stmt, err := idx.db.Prepare("INSERT INTO memory_fts(source, content, timestamp) VALUES(?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		text := msg.Role + ": " + msg.Content
		if _, err := stmt.Exec("message", text, msg.Timestamp); err != nil {
			idx.logger.Debug("failed to index message", "id", msg.ID, "err", err)
		}
	}

	// Update message_count to stay in sync with incremental indexing
	idx.db.Exec(`INSERT OR REPLACE INTO index_meta(source, indexed_at) VALUES(?, ?)`, "message_count", fmt.Sprintf("%d", len(msgs)))
	idx.updateMeta("message")
	return nil
}

// IndexFiles indexes MEMORY.md and daily notes.
func (idx *MemoryIndex) IndexFiles(agentID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	dir := agentDir(agentID)

	// Clear existing file entries
	if _, err := idx.db.Exec("DELETE FROM memory_fts WHERE source IN ('memory', 'daily')"); err != nil {
		return err
	}

	stmt, err := idx.db.Prepare("INSERT INTO memory_fts(source, content, timestamp) VALUES(?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	// Index MEMORY.md
	memoryPath := filepath.Join(dir, "MEMORY.md")
	if data, err := os.ReadFile(memoryPath); err == nil && len(data) > 0 {
		// Split by sections for better granularity
		sections := splitSections(string(data))
		now := time.Now().UTC().Format(time.RFC3339)
		for _, section := range sections {
			if strings.TrimSpace(section) == "" {
				continue
			}
			stmt.Exec("memory", section, now)
		}
	}

	// Index daily notes
	memDir := filepath.Join(dir, "memory")
	entries, err := os.ReadDir(memDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(memDir, entry.Name()))
			if err != nil || len(data) == 0 {
				continue
			}
			// Use filename date as timestamp
			date := strings.TrimSuffix(entry.Name(), ".md")
			stmt.Exec("daily", string(data), date)
		}
	}

	idx.updateMeta("memory")
	idx.updateMeta("daily")
	return nil
}

// Reindex re-indexes all sources for an agent (full rebuild).
func (idx *MemoryIndex) Reindex(agentID string) error {
	if err := idx.IndexMessages(agentID); err != nil {
		idx.logger.Warn("failed to index messages", "err", err)
	}
	if err := idx.IndexFiles(agentID); err != nil {
		idx.logger.Warn("failed to index files", "err", err)
	}
	return nil
}

// IndexNewMessages incrementally indexes only new messages since last index.
func (idx *MemoryIndex) IndexNewMessages(agentID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Get total message count last processed (stored in index_meta)
	var lastCount int
	hasMeta := true
	if err := idx.db.QueryRow("SELECT CAST(indexed_at AS INTEGER) FROM index_meta WHERE source = 'message_count'").Scan(&lastCount); err != nil {
		lastCount = 0
		hasMeta = false
	}

	msgs, err := loadMessages(agentID, 0)
	if err != nil {
		return
	}

	// Migration: if message_count meta doesn't exist but FTS already has rows,
	// use FTS row count as baseline to avoid re-inserting existing data.
	if !hasMeta {
		var ftsCount int
		if err := idx.db.QueryRow("SELECT COUNT(*) FROM memory_fts WHERE source = 'message'").Scan(&ftsCount); err == nil && ftsCount > 0 {
			// Existing DB without message_count tracking. Save current total and skip.
			idx.db.Exec(`INSERT OR REPLACE INTO index_meta(source, indexed_at) VALUES(?, ?)`, "message_count", fmt.Sprintf("%d", len(msgs)))
			return
		}
	}

	// If messages were truncated/rebuilt (count shrank), do a full reindex
	if len(msgs) < lastCount {
		idx.db.Exec("DELETE FROM memory_fts WHERE source = 'message'")
		lastCount = 0
		// Update count even if no messages remain
		if len(msgs) == 0 {
			idx.db.Exec(`INSERT OR REPLACE INTO index_meta(source, indexed_at) VALUES(?, ?)`, "message_count", "0")
			return
		}
	}

	if len(msgs) <= lastCount {
		return // no new messages
	}

	// Only insert new messages (after the already-processed ones)
	stmt, err := idx.db.Prepare("INSERT INTO memory_fts(source, content, timestamp) VALUES(?, ?, ?)")
	if err != nil {
		return
	}
	defer stmt.Close()

	for _, msg := range msgs[lastCount:] {
		if msg.Content == "" {
			continue
		}
		text := msg.Role + ": " + msg.Content
		stmt.Exec("message", text, msg.Timestamp)
	}

	// Store total message count (not indexed count) to avoid offset drift
	idx.db.Exec(`INSERT OR REPLACE INTO index_meta(source, indexed_at) VALUES(?, ?)`, "message_count", fmt.Sprintf("%d", len(msgs)))
	idx.updateMeta("message")
}

// IndexFilesIfStale re-indexes files only if they've changed since last index.
func (idx *MemoryIndex) IndexFilesIfStale(agentID string) {
	if !idx.filesStale(agentID) {
		return
	}
	idx.IndexFiles(agentID)
}

// filesStale checks if any memory files have been modified since the last index.
func (idx *MemoryIndex) filesStale(agentID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var lastIndexed string
	idx.db.QueryRow("SELECT indexed_at FROM index_meta WHERE source = 'memory'").Scan(&lastIndexed)
	if lastIndexed == "" {
		return true
	}

	lastTime, err := time.Parse(time.RFC3339, lastIndexed)
	if err != nil {
		return true
	}

	dir := agentDir(agentID)
	if info, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err == nil && info.ModTime().After(lastTime) {
		return true
	}

	memDir := filepath.Join(dir, "memory")
	if entries, err := os.ReadDir(memDir); err == nil {
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && info.ModTime().After(lastTime) {
				return true
			}
		}
	}

	return false
}

// Search performs a FTS5 search and returns relevant context snippets.
func (idx *MemoryIndex) Search(query string, limit int) ([]SearchResult, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if limit <= 0 {
		limit = maxResults
	}

	// Sanitize query for FTS5
	query = sanitizeFTSQuery(query)
	if query == "" {
		return nil, nil
	}

	rows, err := idx.db.Query(`
		SELECT source, snippet(memory_fts, 1, '', '', '...', 64), timestamp,
			   rank
		FROM memory_fts
		WHERE memory_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Source, &r.Snippet, &r.Timestamp, &r.Score); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// SearchResult represents a single search hit.
type SearchResult struct {
	Source    string  `json:"source"`    // "message", "memory", "daily"
	Snippet  string  `json:"snippet"`   // text snippet with context
	Timestamp string `json:"timestamp"`
	Score     float64 `json:"score"`
}

// BuildContextFromQuery searches the index and returns formatted context for injection into system prompt.
func (idx *MemoryIndex) BuildContextFromQuery(query string) string {
	results, err := idx.Search(query, maxResults)
	if err != nil || len(results) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# Relevant Memory (search results)\n\n")

	totalLen := 0
	for _, r := range results {
		entry := fmt.Sprintf("- [%s] %s\n", r.Source, r.Snippet)
		if totalLen+len(entry) > maxContextLen {
			break
		}
		sb.WriteString(entry)
		totalLen += len(entry)
	}

	return sb.String()
}

func (idx *MemoryIndex) updateMeta(source string) {
	now := time.Now().UTC().Format(time.RFC3339)
	idx.db.Exec(`INSERT OR REPLACE INTO index_meta(source, indexed_at) VALUES(?, ?)`, source, now)
}

// splitSections splits markdown text by ## headings for granular indexing.
func splitSections(text string) []string {
	lines := strings.Split(text, "\n")
	var sections []string
	var current strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") && current.Len() > 0 {
			sections = append(sections, current.String())
			current.Reset()
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		sections = append(sections, current.String())
	}
	return sections
}

// sanitizeFTSQuery converts user input into a safe FTS5 query.
// Wraps each word in quotes to prevent syntax errors from special characters.
func sanitizeFTSQuery(query string) string {
	words := strings.Fields(query)
	if len(words) == 0 {
		return ""
	}
	var parts []string
	for _, w := range words {
		// Remove FTS5 special characters
		w = strings.NewReplacer(
			"\"", "",
			"*", "",
			"(", "",
			")", "",
			":", "",
			"^", "",
			"+", "",
			"-", "",
		).Replace(w)
		w = strings.TrimSpace(w)
		if w != "" {
			parts = append(parts, "\""+w+"\"")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}
