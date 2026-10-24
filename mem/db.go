package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// schema creates the one table we index into plus the FTS5 search index.
//
// The FTS5 table is an "external content" index: files_fts stores only an
// inverted index of words, not a second copy of the text. That keeps the
// database small and means the two can never disagree about what a document
// says. Triggers keep them in lockstep.
const schema = `
CREATE TABLE IF NOT EXISTS files (
  id           INTEGER PRIMARY KEY,
  path         TEXT    NOT NULL UNIQUE,
  filename     TEXT    NOT NULL,
  extension    TEXT,
  dir          TEXT,
  size         INTEGER,
  modified_at  INTEGER,
  indexed_at   INTEGER,
  content      TEXT,
  content_type TEXT,
  category     TEXT,
  sha256       TEXT
);

CREATE INDEX IF NOT EXISTS files_category ON files(category);
CREATE INDEX IF NOT EXISTS files_modified ON files(modified_at);

CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
  filename, path, category, content,
  content='files', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);

-- Mirror inserts, deletes and updates into the search index automatically,
-- so no code path can index a file and forget to make it searchable.
CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, filename, path, category, content)
  VALUES (new.id, new.filename, new.path, new.category, new.content);
END;

CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, filename, path, category, content)
  VALUES ('delete', old.id, old.filename, old.path, old.category, old.content);
END;

CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, filename, path, category, content)
  VALUES ('delete', old.id, old.filename, old.path, old.category, old.content);
  INSERT INTO files_fts(rowid, filename, path, category, content)
  VALUES (new.id, new.filename, new.path, new.category, new.content);
END;
`

// Store wraps the database handle.
type Store struct {
	db   *sql.DB
	path string
}

// DefaultDBPath is where the index lives. Kept out of the user's working
// directory so `mem` never litters the folders it is scanning.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mem", "index.db"), nil
}

// OpenStore opens (creating if needed) the database and applies the schema.
// Running the schema on every launch means an old index file can never crash
// a newer build of the program.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// busy_timeout stops a second `mem` invocation failing instantly while the
	// first one still holds the write lock.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite is happiest with one writer; this keeps concurrent reads safe.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Path is where this store lives on disk.
func (s *Store) Path() string { return s.path }

// existingRecord is the minimal info needed to decide whether a file needs
// re-reading. Deliberately tiny: the whole point is to avoid opening files.
type existingRecord struct {
	ID       int64
	Size     int64
	Modified int64
	HasText  bool
}

// Lookup returns what we already know about a path, if anything.
func (s *Store) Lookup(path string) (existingRecord, bool, error) {
	var r existingRecord
	var contentType sql.NullString
	err := s.db.QueryRow(
		`SELECT id, size, modified_at, content_type FROM files WHERE path = ?`, path,
	).Scan(&r.ID, &r.Size, &r.Modified, &contentType)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.HasText = contentType.String != "" && contentType.String != "meta"
	return r, true, nil
}

// Upsert inserts a file or refreshes it. It returns whether the stored row
// actually changed, which drives the "unchanged files skipped" counter.
func (s *Store) Upsert(fi FileInfo, content, contentType, category, sha string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO files
			(path, filename, extension, dir, size, modified_at, indexed_at,
			 content, content_type, category, sha256)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			filename    = excluded.filename,
			extension   = excluded.extension,
			dir         = excluded.dir,
			size        = excluded.size,
			modified_at = excluded.modified_at,
			indexed_at  = excluded.indexed_at,
			content     = excluded.content,
			content_type= excluded.content_type,
			category    = excluded.category,
			sha256      = excluded.sha256`,
		fi.Path, fi.Filename, fi.Ext, filepath.Dir(fi.Path), fi.Size, fi.ModifiedAt, now,
		content, contentType, category, sha,
	)
	return err
}

// Forget removes a row. Only used when a file has genuinely vanished, and it
// only deletes the database row — never the user's file.
func (s *Store) Forget(id int64) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

// PruneMissing deletes index rows for files under root that no longer exist,
// returning the paths it removed.
//
// Why this exists: without it, deleting a file leaves its row behind forever.
// mem search would keep returning it at 99% confidence, pointing at a path
// that no longer resolves. Re-scanning did not help, because a walk only ever
// adds and updates rows — nothing in the pipeline could remove one. Forget was
// written for this and never wired up, so the tool could confidently cite a
// file the user had thrown away.
//
// Scoping is the part that must not be wrong. The index is shared across every
// folder ever scanned, keyed by absolute path, so scanning ~/Downloads must
// never disturb a row from ~/Pictures. Only rows under root are candidates.
//
// A row is removed only when two conditions hold:
//
//  1. the walk did not see the path this pass, and
//  2. os.Stat reports the file is gone.
//
// The second is not redundant. The walk deliberately skips files over 64 MiB,
// hidden files, and anything under a hidden directory, so "not seen" alone
// would delete rows for files still sitting on disk. The stat is the
// tie-breaker, and it only runs on rows that already failed the first test, so
// the common case costs nothing.
func (s *Store) PruneMissing(root string, seen map[string]bool) ([]string, error) {
	// substr, not LIKE: LIKE treats "_" as a single-character wildcard, and
	// any real path may contain one. This compares a fixed-length prefix
	// instead, so "/a/b" cannot accidentally match "/a/b_c".
	prefix := root + string(filepath.Separator)
	rows, err := s.db.Query(
		`SELECT id, path FROM files WHERE path = ? OR substr(path, 1, ?) = ?`,
		root, len(prefix), prefix,
	)
	if err != nil {
		return nil, err
	}

	var ids []int64
	var gone []string
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return nil, err
		}
		if seen[p] {
			continue
		}
		// Only "does not exist" counts. A stat that fails for any other
		// reason (permissions, a vanished network mount) leaves the row
		// alone, because we have not proved the file is gone.
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			continue
		}
		ids = append(ids, id)
		gone = append(gone, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// One transaction, so a failure part-way leaves the index as it was rather
	// than half-pruned. The DELETE fires files_ad, which drops the FTS entry
	// in the same breath — there is no second thing to remember to do.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	stmt, err := tx.Prepare(`DELETE FROM files WHERE id = ?`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.Exec(id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return gone, nil
}

// Count is how many files we currently know about.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	return n, err
}

// Content returns the stored extracted text for one indexed file. It is what
// the detail view shows when a result is opened. An empty string means the
// file has no searchable text (a scanned PDF, a photo with no words).
func (s *Store) Content(id int64) (string, error) {
	var content sql.NullString
	err := s.db.QueryRow(`SELECT content FROM files WHERE id = ?`, id).Scan(&content)
	return content.String, err
}

// CountUnsearchable is how many indexed files we hold metadata for but no text:
// scanned PDFs, images where OCR found nothing worth indexing, and formats we
// cannot read at all.
//
// This is the population that makes a correct search look broken. Someone asks
// for "dragon" in a folder of photographs and gets an empty result, which is
// accurate — none of those files contain the word — but explains nothing. The
// count turns a dead end into a diagnosis.
func (s *Store) CountUnsearchable() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM files WHERE content IS NULL OR TRIM(content) = ''`,
	).Scan(&n)
	return n, err
}

// CategoryStat is one row of the "by category" breakdown.
type CategoryStat struct {
	Name  string
	Count int
	Bytes int64
}

// ExtStat is one row of the "by file type" breakdown.
type ExtStat struct {
	Ext   string
	Count int
}

// StatsData is a summary of what the index holds, as data rather than text so
// that both the CLI and the TUI can present it.
type StatsData struct {
	Total      int
	TotalBytes int64
	Path       string
	Categories []CategoryStat
	Extensions []ExtStat
}

// Stats summarises the index: totals and a breakdown by category and by file
// type. Empty categories and extensions are normalised for display here, not
// at the call site, so every consumer shows the same label.
func (s *Store) Stats() (StatsData, error) {
	st := StatsData{Path: s.path}
	n, err := s.Count()
	if err != nil {
		return st, err
	}
	st.Total = n
	if n == 0 {
		return st, nil
	}

	_ = s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM files`).Scan(&st.TotalBytes)

	catRows, err := s.db.Query(`SELECT category, COUNT(*), SUM(size)
	                             FROM files GROUP BY category ORDER BY 2 DESC`)
	if err != nil {
		return st, err
	}
	for catRows.Next() {
		var cat string
		var c int
		var sz int64
		catRows.Scan(&cat, &c, &sz)
		if cat == "" {
			cat = "Uncategorised"
		}
		st.Categories = append(st.Categories, CategoryStat{Name: cat, Count: c, Bytes: sz})
	}
	catRows.Close()

	extRows, err := s.db.Query(`SELECT extension, COUNT(*)
	                            FROM files GROUP BY extension ORDER BY 2 DESC LIMIT 12`)
	if err != nil {
		return st, err
	}
	for extRows.Next() {
		var ext string
		var c int
		extRows.Scan(&ext, &c)
		if ext == "" {
			ext = "(none)"
		}
		st.Extensions = append(st.Extensions, ExtStat{Ext: ext, Count: c})
	}
	extRows.Close()
	return st, nil
}

// RelatedData is the result of a related lookup: the file we compared
// against, the distinctive words that were used to compare, and the
// documents that share them.
//
// It is deliberately a value rather than a printed table so that the CLI and
// the TUI can render the same answer their own way. Terms is what
// distinguishes "this file has no text to compare on" from "nothing else
// shares vocabulary": the first has no Terms at all, the second has Terms
// but an empty Hits slice.
type RelatedData struct {
	Target result
	Terms  []string
	Hits   []result
}

// Related finds memories that overlap with a given file.
//
// The idea: pull the words that define a document, then look for other
// documents containing many of the same words. Overlapping vocabulary is a
// good enough proxy for "these two are about the same thing" without
// needing embeddings or a vector database.
func (s *Store) Related(query string) (RelatedData, error) {
	var data RelatedData
	var target result
	err := s.db.QueryRow(`
		SELECT id, filename, path, category, extension, size, modified_at, content_type
		FROM files
		WHERE filename LIKE ? OR path = ?
		ORDER BY LENGTH(filename) LIMIT 1`, "%"+query+"%", expandHome(query)).
		Scan(&target.ID, &target.Filename, &target.Path, &target.Category,
			&target.Extension, &target.Size, &target.Modified, &target.ContentTy)
	if err == sql.ErrNoRows {
		return data, fmt.Errorf("%q is not in the index; run 'mem scan <dir>' first", query)
	}
	if err != nil {
		return data, err
	}
	data.Target = target

	// Take a sample of the target's own text, keep the words that are rare
	// across the whole index, and find documents sharing those words.
	terms := distinctiveTerms(s, target.ID)
	data.Terms = terms
	if len(terms) == 0 {
		return data, nil
	}

	rows, err := s.db.Query(`
		SELECT f.id, f.filename, f.path, f.extension, f.size, f.modified_at
		FROM files_fts
		JOIN files f ON f.id = files_fts.rowid
		WHERE files_fts MATCH ?
		ORDER BY bm25(files_fts, 4.0, 1.0, 2.0, 3.0)
		LIMIT 12`, ftsQuery(terms))
	if err != nil {
		return data, err
	}
	defer rows.Close()

	for rows.Next() {
		var r result
		if err := rows.Scan(&r.ID, &r.Filename, &r.Path, &r.Extension,
			&r.Size, &r.Modified); err != nil {
			return data, err
		}
		if r.ID == target.ID {
			continue
		}
		data.Hits = append(data.Hits, r)
	}
	if err := rows.Err(); err != nil {
		return data, err
	}
	return data, nil
}

// distinctiveTerms picks the words that best characterise one document.
//
// The trick is frequency across the whole index: a word appearing in nearly
// every file ("document", "pdf", "the") tells us nothing about this file,
// whereas a word appearing in one or two is exactly what makes it distinct.
// We sample the document's text, count each word's index-wide frequency, and
// keep the rarest ones.
func distinctiveTerms(s *Store, id int64) []string {
	var content sql.NullString
	if err := s.db.QueryRow(`SELECT content FROM files WHERE id = ?`, id).
		Scan(&content); err != nil || !content.Valid || len(content.String) == 0 {
		return nil
	}

	// Sample the opening of the document: the title page and contents page
	// are the most identifying part, and this keeps the work bounded.
	sample := content.String
	if len(sample) > 20000 {
		sample = sample[:20000]
	}

	// Count words within the sample, so a word repeated many times in one
	// document does not drown out the others.
	local := map[string]int{}
	for _, w := range strings.Fields(strings.ToLower(sample)) {
		w = strings.Trim(w, ".,;:!?()[]{}\"'`")
		if len(w) >= 4 && !stopWords[w] {
			local[w]++
		}
	}

	// Ask the index how many documents contain each candidate word.
	type cand struct {
		word string
		docs int
	}
	var cands []cand
	for w := range local {
		var docs int
		// Counting documents that contain the word: a prefix search, so
		// "syllab" also counts "syllabus".
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM files_fts WHERE files_fts MATCH ?`, `"`+w+`"*`,
		).Scan(&docs); err != nil || docs == 0 {
			continue
		}
		cands = append(cands, cand{word: w, docs: docs})
	}

	// Rarest first; ties broken alphabetically so output is reproducible.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].docs != cands[j].docs {
			return cands[i].docs < cands[j].docs
		}
		return cands[i].word < cands[j].word
	})

	out := make([]string, 0, 12)
	for _, c := range cands {
		if len(out) >= 12 {
			break
		}
		out = append(out, c.word)
	}
	return out
}

// truncate shortens a string for aligned table output.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
