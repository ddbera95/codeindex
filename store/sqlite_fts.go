package store

import (
	"bufio"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxConfigBytes = 50 * 1024 // 50 KB — larger config files are skipped
	ftsQueryLimit  = 50
)

// sourceExts are always indexed.
var sourceExts = map[string]bool{
	".go": true,
}

// configExts are indexed only when includeConfig=true and file size ≤ maxConfigBytes.
var configExts = map[string]bool{
	".yaml": true, ".yml": true, ".toml": true,
	".json": true, ".env": true, ".ini": true,
}

// generatedSuffixes are skipped even if they have a source extension.
var generatedSuffixes = []string{
	"_gen.go", ".pb.go", "_mock.go",
}

// ftsSkipDirs are directory names never indexed.
var ftsSkipDirs = map[string]bool{
	"vendor": true, ".git": true, "node_modules": true, "testdata": true,
}

// FTSResult is one matching line returned by Search.
type FTSResult struct {
	File    string
	Line    int
	Content string // raw line
	Snippet string // highlighted snippet from FTS5
}

// FTSStore is a SQLite-backed full-text search index using the FTS5 extension.
//
// Schema:
//
//	fts        — FTS5 virtual table (content, file UNINDEXED, line UNINDEXED)
//	file_meta  — tracks mtime per file so incremental updates skip unchanged files
type FTSStore struct {
	db   *sql.DB
	path string
}

func NewFTSStore(path string) (*FTSStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open fts db: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &FTSStore{db: db, path: path}
	return s, s.migrate()
}

func (s *FTSStore) Close() error { return s.db.Close() }
func (s *FTSStore) Path() string { return s.path }

func (s *FTSStore) migrate() error {
	_, err := s.db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous  = NORMAL;

		-- FTS5 virtual table. 'file' and 'line' are stored but NOT indexed.
		-- tokenize=unicode61 handles identifiers; separators=' ' keeps underscores together.
		CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
			content,
			file      UNINDEXED,
			line      UNINDEXED,
			tokenize  = 'unicode61'
		);

		-- Tracks last-modified time so we can skip unchanged files on re-index.
		CREATE TABLE IF NOT EXISTS file_meta (
			path  TEXT    PRIMARY KEY,
			mtime INTEGER NOT NULL,
			lines INTEGER NOT NULL
		);
	`)
	return err
}

// ── Indexing ──────────────────────────────────────────────────────────────

// IndexDir walks dir and (incrementally) indexes source files into FTS5.
// Pass includeConfig=true to also index small config files.
// Returns counts of indexed, skipped-unchanged, and removed files.
func (s *FTSStore) IndexDir(dir string, includeConfig bool) (indexed, unchanged, removed int, err error) {
	// Step 1: walk disk → collect files we want to index.
	type fileInfo struct {
		path  string
		mtime int64
	}
	currentFiles := map[string]fileInfo{}

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && (ftsSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		name := d.Name()

		// Always skip generated files.
		for _, suf := range generatedSuffixes {
			if strings.HasSuffix(name, suf) {
				return nil
			}
		}

		if sourceExts[ext] {
			info, e := d.Info()
			if e != nil {
				return nil
			}
			currentFiles[path] = fileInfo{path, info.ModTime().Unix()}
			return nil
		}

		if includeConfig && configExts[ext] {
			info, e := d.Info()
			if e != nil {
				return nil
			}
			if info.Size() <= maxConfigBytes {
				currentFiles[path] = fileInfo{path, info.ModTime().Unix()}
			}
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, 0, walkErr
	}

	// Step 2: load stored mtimes from file_meta.
	storedMtimes := map[string]int64{}
	rows, err := s.db.Query(`SELECT path, mtime FROM file_meta`)
	if err != nil {
		return 0, 0, 0, err
	}
	for rows.Next() {
		var path string
		var mtime int64
		rows.Scan(&path, &mtime)
		storedMtimes[path] = mtime
	}
	rows.Close()

	// Step 3: remove files that no longer exist on disk.
	for storedPath := range storedMtimes {
		if _, exists := currentFiles[storedPath]; !exists {
			if e := s.removeFile(storedPath); e == nil {
				removed++
			}
		}
	}

	// Step 4: index new or changed files.
	for _, fi := range currentFiles {
		storedMtime, known := storedMtimes[fi.path]
		if known && storedMtime == fi.mtime {
			unchanged++
			continue
		}
		if e := s.indexFile(fi.path, fi.mtime); e != nil {
			fmt.Fprintf(os.Stderr, "warn: fts %s: %v\n", fi.path, e)
			continue
		}
		indexed++
	}

	return indexed, unchanged, removed, nil
}

// indexFile removes any existing entries for path then re-inserts all lines.
func (s *FTSStore) indexFile(path string, mtime int64) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Remove old content for this file.
	tx.Exec(`DELETE FROM fts WHERE file = ?`, path)
	tx.Exec(`DELETE FROM file_meta WHERE path = ?`, path)

	// Insert new lines.
	stmt, err := tx.Prepare(`INSERT INTO fts(content, file, line) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	scanner := bufio.NewScanner(src)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue // skip blank lines
		}
		if _, err := stmt.Exec(line, path, lineNum); err != nil {
			return err
		}
	}

	tx.Exec(`INSERT INTO file_meta(path, mtime, lines) VALUES (?, ?, ?)`, path, mtime, lineNum)
	return tx.Commit()
}

// removeFile deletes all FTS rows and the meta entry for a file.
func (s *FTSStore) removeFile(path string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM fts WHERE file = ?`, path)
	tx.Exec(`DELETE FROM file_meta WHERE path = ?`, path)
	return tx.Commit()
}

// ── Querying ──────────────────────────────────────────────────────────────

// Search runs a FTS5 MATCH query and returns up to limit results.
//
// Query syntax (passed directly to FTS5):
//
//	word            — single word
//	"phrase here"   — exact phrase
//	word1 word2     — both words anywhere on same line
//	word1 OR word2  — either word
//	word*           — prefix match (e.g. "Get*")
func (s *FTSStore) Search(query string, limit int) ([]FTSResult, error) {
	if limit <= 0 {
		limit = ftsQueryLimit
	}

	rows, err := s.db.Query(`
		SELECT
			file,
			line,
			content,
			snippet(fts, 0, '>>>', '<<<', '...', 12)
		FROM fts
		WHERE fts MATCH ?
		ORDER BY rank
		LIMIT ?`,
		query, limit,
	)
	if err != nil {
		// FTS5 query syntax errors surface here — return friendly message.
		if strings.Contains(err.Error(), "fts5") {
			return nil, fmt.Errorf("invalid search query %q — use quotes for phrases, * for prefix", query)
		}
		return nil, err
	}
	defer rows.Close()

	var results []FTSResult
	for rows.Next() {
		var r FTSResult
		if err := rows.Scan(&r.File, &r.Line, &r.Content, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// IndexFile re-indexes a single file (exported for MCP file watcher).
func (s *FTSStore) IndexFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return s.indexFile(path, info.ModTime().Unix())
}

// RemoveFile removes a single file from the FTS index (exported for MCP file watcher).
func (s *FTSStore) RemoveFile(path string) error {
	return s.removeFile(path)
}

// Stats returns the number of indexed files and total lines.
func (s *FTSStore) Stats() (files, lines int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), SUM(lines) FROM file_meta`).Scan(&files, &lines)
	return
}

// LastIndexed returns when the most recent file was indexed.
func (s *FTSStore) LastIndexed() (time.Time, error) {
	var mtime int64
	err := s.db.QueryRow(`SELECT MAX(mtime) FROM file_meta`).Scan(&mtime)
	if err != nil || mtime == 0 {
		return time.Time{}, err
	}
	return time.Unix(mtime, 0), nil
}
