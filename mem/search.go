package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// bm25 column weights. Order must match the FTS5 column order:
// filename, path, category, content.
const (
	weightFilename = 10.0
	weightPath     = 2.0
	weightCategory = 3.0
	weightContent  = 1.0
)

// stopWords carry no retrieval signal. "the PDF about X" should behave like
// "X"; without this filter every document scores zero because they are all,
// technically, "about" something.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "from": true,
	"by": true, "with": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "it": true, "its": true, "this": true, "that": true,
	"these": true, "those": true, "i": true, "im": true, "ive": true, "my": true,
	"me": true, "we": true, "you": true, "your": true, "he": true, "she": true,
	"they": true, "them": true, "his": true, "her": true, "their": true,
	"about": true, "note": true, "notes": true, "file": true, "files": true,
	"doc": true, "docs": true, "document": true, "documents": true, "paper": true,
	"papers": true, "thing": true, "things": true, "stuff": true, "content": true,
	"saved": true, "save": true, "downloaded": true, "download": true, "downloads": true,
	"find": true, "show": true, "search": true, "searching": true, "look": true,
	"looking": true, "got": true, "get": true, "please": true, "help": true,
	"want": true, "need": true, "where": true, "when": true,
	"what": true, "which": true, "who": true, "how": true, "why": true,
	"can": true, "could": true, "should": true, "would": true, "will": true,
	"there": true, "here": true, "then": true, "than": true, "some": true,
	"any": true, "all": true, "one": true, "two": true, "again": true, "also": true,
	"very": true, "just": true, "only": true, "even": true, "still": true,
}

// typeWords maps a word the user typed to file extensions. Seeing "PDF" or
// "screenshot" in a query is a strong hint about *what kind* of file they
// want, which is worth more than searching for those words.
var typeWords = map[string][]string{
	"pdf": {".pdf"}, "docx": {".docx"}, "doc": {".doc", ".docx"},
	"pptx": {".pptx"}, "ppt": {".ppt", ".pptx"}, "slides": {".pptx", ".ppt"},
	"spreadsheet": {".csv", ".xlsx"}, "sheet": {".csv", ".xlsx"},
	"markdown": {".md", ".markdown"}, "text": {".txt", ".md"},
	"note": {".md", ".txt", ".pdf"}, "notes": {".md", ".txt", ".pdf"},
	"image": {".png", ".jpg", ".jpeg", ".webp", ".gif"}, "images": {".png", ".jpg", ".jpeg", ".webp", ".gif"},
	"photo": {".png", ".jpg", ".jpeg"}, "photos": {".png", ".jpg", ".jpeg"},
	"picture": {".png", ".jpg", ".jpeg"}, "screenshot": {".png", ".jpg", ".jpeg"},
	"screenshots": {".png", ".jpg", ".jpeg"}, "video": {".mp4", ".mkv", ".webm"},
	"audio": {".mp3", ".wav", ".flac"}, "zip": {".zip", ".tar", ".gz", ".7z"},
}

// timeWindow is a decoded time hint from the query.
type timeWindow struct {
	after  int64 // unix seconds, 0 means no lower bound
	before int64 // unix seconds, 0 means no upper bound
	label  string
}

// parsedQuery is the structured form of a user's sentence.
type parsedQuery struct {
	terms      []string
	extensions []string
	window     timeWindow
	category   string
	raw        string

	// blockedByDate is set by Search when a query found nothing but would
	// have matched if the time filter were removed. It exists only to make
	// the failure message useful, and is never consulted for ranking.
	blockedByDate int

	// unsearchable is set by Search on a miss: how many indexed files we hold
	// metadata for but no text, so they can never match a content query. Also
	// only used to make the failure message useful.
	unsearchable int
}

// timeWords are consumed by the time-hint parser. Once "last week" has been
// turned into a date range, those words must not also be searched as content
// keywords, or the query drags in every document that mentions "week".
var timeWords = map[string]bool{
	"today": true, "yesterday": true, "tonight": true, "morning": true,
	"evening": true, "recently": true, "recent": true, "lately": true,
	"week": true, "weeks": true, "month": true, "months": true,
	"day": true, "days": true, "last": true, "past": true, "this": true,
	"ago": true, "night": true,
	"year": true, "years": true,
}

// parseQuery turns a human sentence into something a search engine can use.
//
// This is the heart of "natural language" support, and it is entirely
// deterministic: no API calls, no network, nothing to fail at demo time.
func parseQuery(raw string) parsedQuery {
	pq := parsedQuery{raw: raw}

	lowered := strings.ToLower(raw)

	// 1. Time hints: "last week", "yesterday", "recently".
	now := todayUnix()
	switch {
	case strings.Contains(lowered, "today") || strings.Contains(lowered, "just now"):
		pq.window = timeWindow{after: startOfDay(now), label: "today"}
	case strings.Contains(lowered, "yesterday"):
		pq.window = timeWindow{after: startOfDay(now - 86400), before: startOfDay(now), label: "yesterday"}
	case strings.Contains(lowered, "this week"), strings.Contains(lowered, "last 7 days"):
		pq.window = timeWindow{after: now - 7*86400, label: "this week"}
	case strings.Contains(lowered, "last week"):
		pq.window = timeWindow{after: now - 14*86400, before: now - 7*86400, label: "last week"}
	case strings.Contains(lowered, "this month"), strings.Contains(lowered, "recently"):
		pq.window = timeWindow{after: now - 30*86400, label: "recently"}
	case strings.Contains(lowered, "last month"):
		pq.window = timeWindow{after: now - 60*86400, before: now - 30*86400, label: "last month"}
	case strings.Contains(lowered, "this year"):
		// Order matters: "last month" and "this year" are checked before the
		// broader "this" match below, so a query naming both resolves to the
		// narrower one.
		pq.window = timeWindow{after: now - 365*86400, label: "this year"}
	case strings.Contains(lowered, "last year"):
		pq.window = timeWindow{after: now - 2*365*86400, before: now - 365*86400, label: "last year"}
	}

	// 2. Tokenize into words.
	fields := strings.FieldsFunc(lowered, func(r rune) bool {
		return !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})

	// Split glued words: "schedulingnotes" -> scheduling, notes.
	var words []string
	for _, f := range fields {
		words = append(words, splitCamel(f)...)
	}

	seen := map[string]bool{}
	hintFound := pq.window.label != ""
	for _, w := range words {
		// Digits alone are rarely useful and pollute FTS syntax.
		if w == "" || isAllDigits(w) {
			continue
		}
		// 3. File type hints become filters, not keywords.
		if exts, ok := typeWords[w]; ok {
			if pq.extensions == nil {
				pq.extensions = exts
			}
			continue
		}
		// 3b. Time words were already consumed into a date range above.
		// Searching for them too would match every document that happens to
		// say "week", drowning out the real keywords.
		if hintFound && timeWords[w] {
			continue
		}
		// 4. Stop words go.
		if stopWords[w] {
			continue
		}
		// 5. Keep a couple of variants so "processes" finds "process".
		for _, v := range variants(w) {
			if !seen[v] {
				seen[v] = true
				pq.terms = append(pq.terms, v)
			}
		}
	}
	return pq
}

// variants returns the forms of a word worth searching for: the word itself
// and a light singularisation. FTS5 has no stemmer, and "process" must match
// "processes" or the single most common kind of query silently fails.
func variants(w string) []string {
	out := []string{w}
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		// studies -> study
		out = append(out, strings.TrimSuffix(w, "ies")+"y")
	case len(w) > 4 && strings.HasSuffix(w, "ses"):
		// processes -> process. Must be checked before the generic "es"
		// rule, which would otherwise produce "processe".
		out = append(out, strings.TrimSuffix(w, "es"))
	case len(w) > 3 && strings.HasSuffix(w, "es") && !strings.HasSuffix(w, "ss"):
		// boxes -> box, matches -> match
		out = append(out, strings.TrimSuffix(w, "es"))
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		// notes -> note
		out = append(out, strings.TrimSuffix(w, "s"))
	}
	// A variant too short to be a real word is noise, not signal.
	if len(out) > 1 && len(out[len(out)-1]) < 3 {
		out = out[:1]
	}
	return out
}

// splitCamel breaks "resumeFinal" into "resume", "final" so that oddly
// capitalised filenames stay searchable as words.
func splitCamel(s string) []string {
	runes := []rune(s)
	var out []string
	start := 0
	for i := 1; i < len(runes); i++ {
		if runes[i] >= 'A' && runes[i] <= 'Z' && i > start {
			out = append(out, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	if start < len(runes) {
		out = append(out, strings.ToLower(string(runes[start:])))
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// isAllDigits reports whether a token is purely numeric.
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ftsQuery builds the MATCH expression. Terms are OR-ed together because a
// user describing a memory will mention several right things and a couple of
// wrong ones; AND would return nothing, which is the worst possible outcome.
// Prefix matching ("schedul*") catches partial words.
func ftsQuery(terms []string) string {
	var parts []string
	for _, t := range terms {
		// Quote to keep FTS5 syntax characters from being interpreted.
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	return strings.Join(parts, " OR ")
}

// result is one search hit.
type result struct {
	ID        int64
	Filename  string
	Path      string
	Category  string
	Extension string
	Size      int64
	Modified  int64
	ContentTy string
	Score     float64
	Snippet   string
	Matched   []string
	Source    string
}

// Search runs a query against the index.
func (s *Store) Search(rawQuery string, limit int, filterExt string) ([]result, parsedQuery, error) {
	pq := parseQuery(rawQuery)
	if filterExt != "" {
		if !strings.HasPrefix(filterExt, ".") {
			filterExt = "." + filterExt
		}
		pq.extensions = []string{filterExt}
	}

	if len(pq.terms) == 0 {
		// Everything was a stop word or a type hint, e.g. "find me the pdf".
		// Fall back to matching the type hint alone, or list everything.
		rows, err := s.searchBroad(pq, limit)
		return rows, pq, err
	}

	rows, err := s.queryFTS(pq, limit*3)
	if err != nil {
		return nil, pq, err
	}
	if len(rows) > 0 {
		return rows, pq, nil
	}

	// Nothing from full-text: try partial filename matching, because a user
	// who half-remembers a name is exactly the user we are here for.
	rows, err = s.searchFilename(pq, limit)
	if err != nil {
		return nil, pq, err
	}
	if len(rows) > 0 {
		return rows, pq, nil
	}

	// No match. Return nothing rather than falling back to recent files.
	//
	// An earlier version did fall through to searchBroad here, so that
	// "the pdf about zzzqqq" would at least show some PDFs. But searchBroad
	// ignores the query terms entirely, so it answered with recent files that
	// had nothing to do with what was asked, printed at 60% confidence. That
	// is worse than an empty result: a wrong result that looks confident
	// teaches the user to trust answers that are not answers. Saying "I
	// found nothing" is the correct outcome here, and it is the only one
	// that is true.
	//
	// Before giving up, check whether the words matched but the date filter
	// excluded everything. That is the single most common cause, and it is
	// cheap to detect on this cold path.
	loose := pq
	loose.window = timeWindow{}
	if n, err := s.queryFTS(loose, 5); err == nil {
		pq.blockedByDate = len(n)
	}
	// Also check how much of the index is metadata-only. Asking for "dragon"
	// against a folder of photographs returns nothing, and without this the
	// message is "try fewer or more general words" — advice that cannot
	// possibly work, because the files hold no words at all. Both extra
	// queries run only after a miss, so a successful search pays nothing.
	if n, err := s.CountUnsearchable(); err == nil {
		pq.unsearchable = n
	}
	return nil, pq, nil
}

// appendWindow adds the date bounds for a parsed time hint to a WHERE clause.
// Every query path must use it: a user who says "last week" and gets back a
// file from last month has been given worse than no answer at all, because
// it looks like an answer.
func appendWindow(pq parsedQuery, where []string, args []any, col string) ([]string, []any) {
	now := timeNow().Unix()
	if pq.window.after > 0 {
		where = append(where, col+" >= ?")
		args = append(args, pq.window.after)
	}
	if pq.window.before > 0 && pq.window.before < now {
		where = append(where, col+" < ?")
		args = append(args, pq.window.before)
	}
	return where, args
}

// queryFTS is the main relevance search.
func (s *Store) queryFTS(pq parsedQuery, limit int) ([]result, error) {
	match := ftsQuery(pq.terms)

	where := []string{"files_fts MATCH ?"}
	args := []any{match}
	where, args = appendWindow(pq, where, args, "f.modified_at")
	if len(pq.extensions) > 0 {
		marks := make([]string, len(pq.extensions))
		for i, e := range pq.extensions {
			marks[i] = "?"
			args = append(args, e)
		}
		where = append(where, fmt.Sprintf("f.extension IN (%s)", strings.Join(marks, ",")))
	}

	// bm25 returns a negative number where MORE negative is better.
	// Column weights make a filename hit outrank a passing mention in a body
	// of text, which is what users intuitively expect.
	sqlText := fmt.Sprintf(`
		SELECT f.id, f.filename, f.path, f.category, f.extension, f.size,
		       f.modified_at, f.content_type,
		       bm25(files_fts, %.1f, %.1f, %.1f, %.1f) AS score,
		       snippet(files_fts, 3, ' >> ', ' << ', ' ... ', 14) AS snip
		FROM files_fts
		JOIN files f ON f.id = files_fts.rowid
		WHERE %s
		ORDER BY score
		LIMIT ?`, weightFilename, weightPath, weightCategory, weightContent,
		strings.Join(where, " AND "))
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []result
	best := 0.0
	for rows.Next() {
		var r result
		var snip sql.NullString
		if err := rows.Scan(&r.ID, &r.Filename, &r.Path, &r.Category, &r.Extension,
			&r.Size, &r.Modified, &r.ContentTy, &r.Score, &snip); err != nil {
			return nil, err
		}
		r.Snippet = cleanSnippet(snip.String)
		r.Source = "content"
		if best == 0 || r.Score < best {
			best = r.Score
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Fill in which words actually hit, for the "Matches:" line.
	for i := range out {
		out[i].Matched = matchTerms(s, out[i].ID, pq.terms)
	}
	return out, nil
}

// searchFilename rescues a query that only half matches a name.
func (s *Store) searchFilename(pq parsedQuery, limit int) ([]result, error) {
	where := []string{"1=1"}
	args := []any{}
	for _, t := range pq.terms {
		where = append(where, "(f.filename LIKE ? OR f.path LIKE ?)")
		args = append(args, "%"+t+"%", "%"+t+"%")
	}
	where, args = appendWindow(pq, where, args, "f.modified_at")
	if len(pq.extensions) > 0 {
		marks := make([]string, len(pq.extensions))
		for i, e := range pq.extensions {
			marks[i] = "?"
			args = append(args, e)
		}
		where = append(where, fmt.Sprintf("f.extension IN (%s)", strings.Join(marks, ",")))
	}

	args = append(args, limit)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT f.id, f.filename, f.path, f.category, f.extension, f.size,
		       f.modified_at, f.content_type
		FROM files f
		WHERE %s
		ORDER BY f.modified_at DESC
		LIMIT ?`, strings.Join(where, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []result
	for rows.Next() {
		var r result
		if err := rows.Scan(&r.ID, &r.Filename, &r.Path, &r.Category, &r.Extension,
			&r.Size, &r.Modified, &r.ContentTy); err != nil {
			return nil, err
		}
		r.Source = "filename"
		out = append(out, r)
	}
	return out, rows.Err()
}

// searchBroad handles the "no real keywords" case: return the newest files of
// the requested type instead of an empty screen.
//
// It deliberately ignores pq.terms, so it must only ever be reached when there
// are no terms to match. Calling it as a fallback for a query that matched
// nothing returns recent files as though they were answers.
func (s *Store) searchBroad(pq parsedQuery, limit int) ([]result, error) {
	where := []string{"1=1"}
	args := []any{}
	if len(pq.extensions) > 0 {
		marks := make([]string, len(pq.extensions))
		for i, e := range pq.extensions {
			marks[i] = "?"
			args = append(args, e)
		}
		where = append(where, fmt.Sprintf("extension IN (%s)", strings.Join(marks, ",")))
	}
	where, args = appendWindow(pq, where, args, "modified_at")
	args = append(args, limit)

	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id, filename, path, category, extension, size, modified_at, content_type
		FROM files WHERE %s ORDER BY modified_at DESC LIMIT ?`,
		strings.Join(where, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []result
	for rows.Next() {
		var r result
		if err := rows.Scan(&r.ID, &r.Filename, &r.Path, &r.Category, &r.Extension,
			&r.Size, &r.Modified, &r.ContentTy); err != nil {
			return nil, err
		}
		r.Source = "recent"
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchTerms reports which query words a document actually contains, so the
// output can explain *why* something matched.
func matchTerms(s *Store, id int64, terms []string) []string {
	var content sql.NullString
	var name, path, cat sql.NullString
	err := s.db.QueryRow(
		`SELECT content, filename, path, category FROM files WHERE id = ?`, id,
	).Scan(&content, &name, &path, &cat)
	if err != nil {
		return nil
	}
	hay := strings.ToLower(strings.Join([]string{
		name.String, path.String, cat.String, content.String,
	}, " "))

	var hits []string
	for _, t := range terms {
		if strings.Contains(hay, t) {
			hits = append(hits, t)
		}
	}
	if len(hits) > 5 {
		hits = hits[:5]
	}
	return hits
}

// cleanSnippet squashes whitespace and tidies the highlighted region.
func cleanSnippet(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

// confidence converts a bm25 score into an honest-looking percentage.
//
// bm25 has no natural upper bound, so an absolute number would be a lie.
// Instead we rank-normalise: the best hit is 99%, and the rest are scaled
// relative to it. That is a presentation choice, not a probability.
func confidence(results []result) []float64 {
	if len(results) == 0 {
		return nil
	}
	scores := make([]float64, len(results))
	for i, r := range results {
		// More negative bm25 = better, so flip the sign.
		scores[i] = -r.Score
	}
	best := scores[0]
	if best <= 0 {
		// Every document matched every term equally (bm25's IDF term goes to
		// zero when a word is in all of them). Ranking is meaningless here,
		// so fall back to a flat, honest band instead of fake precision.
		flat := make([]float64, len(results))
		for i := range flat {
			flat[i] = 60
		}
		return flat
	}

	out := make([]float64, len(results))
	for i, v := range scores {
		ratio := v / best
		if ratio > 1 {
			ratio = 1
		}
		if ratio < 0 {
			ratio = 0
		}
		// Compress the top end so scores spread out nicely.
		out[i] = 40 + ratio*59
	}
	return out
}

// cmdSearch is the CLI entry point for searching.
func cmdSearch(args []string, dbPath string) error {
	var positionals []string
	limit := 10
	var filterExt string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit", "-n":
			if i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n <= 0 {
					return errors.New("--limit needs a positive number")
				}
				limit = n
				i++
			}
		case "--type", "-t":
			if i+1 < len(args) {
				filterExt = args[i+1]
				i++
			}
		default:
			positionals = append(positionals, args[i])
		}
	}

	// Join remaining words: unquoted queries should still work.
	query := strings.Join(positionals, " ")
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("search needs a query, e.g. mem search %q\n\n%s",
			"notes about operating systems", "run 'mem help search' to see the hints it understands")
	}

	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	total, err := store.Count()
	if err != nil {
		return err
	}
	if total == 0 {
		fmt.Println("Nothing indexed yet. Try:  mem scan ~/Downloads")
		return nil
	}

	results, pq, err := store.Search(query, limit, filterExt)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		printNoMatch(query, pq, total)
		return nil
	}
	if len(results) > limit {
		results = results[:limit]
	}

	conf := confidence(results)
	printResults(query, pq, results, conf, limit)
	return nil
}

// printNoMatch explains an empty result honestly.
//
// A wrong answer that looks confident is worse than no answer, so this message
// never guesses at what the user "probably" meant. It reports what was
// actually searched, names the constraints that were in force (a time window
// or file type can silently shrink the pool to nothing), and stops there.
func printNoMatch(query string, pq parsedQuery, total int) {
	fmt.Printf("No match for %q.\n\n", query)

	if len(pq.terms) > 0 {
		fmt.Println("Searched for:")
		for _, t := range pq.terms {
			fmt.Printf("  %s\n", t)
		}
	} else {
		fmt.Println("That query had no searchable words in it, only:")
		if len(pq.extensions) > 0 {
			fmt.Printf("  file type: %s\n", strings.Join(pq.extensions, " or "))
		}
		if pq.category != "" {
			fmt.Printf("  category:  %s\n", pq.category)
		}
		if pq.window.after > 0 || pq.window.before > 0 {
			fmt.Println("  a date range")
		}
	}

	// The most valuable thing we can say is *why* nothing came back. A user
	// who asked for "syllabus last week" and got silence is far more likely to
	// conclude the tool is broken than to guess that the date was the problem.
	// If dropping the time window would have found something, say so.
	if pq.blockedByDate > 0 {
		noun := "files"
		if pq.blockedByDate == 1 {
			noun = "file"
		}
		fmt.Printf("\nYour words match %d %s, but outside the date range you asked for.\n",
			pq.blockedByDate, noun)
	}

	// Name the one class of file that can never answer a content query, so a
	// user searching for what a picture depicts learns the boundary instead of
	// concluding the tool is broken. mem finds words inside files, including
	// words inside images; it does not recognise subjects.
	if pq.unsearchable > 0 {
		if pq.unsearchable == 1 {
			fmt.Printf("\n1 indexed file has no readable text, so it is findable only by filename.\n")
		} else {
			fmt.Printf("\n%d indexed files have no readable text, so they are findable only by filename.\n",
				pq.unsearchable)
		}
		fmt.Println("Searching for what an image shows is outside what text search can do.")
	}

	// The most common cause of a zero result is a constraint the user forgot
	// they typed. Naming it is more useful than a generic "try fewer words".
	switch {
	case len(pq.extensions) > 0 && pq.window.after > 0:
		fmt.Printf("\nBoth a file type (%s) and a date range were required. "+
			"%d files are indexed in total.\n",
			strings.Join(pq.extensions, " or "), total)
	case len(pq.extensions) > 0:
		fmt.Printf("\nOnly %s files were considered, out of %d indexed.\n",
			strings.Join(pq.extensions, " or "), total)
	case pq.window.after > 0 || pq.window.before > 0:
		fmt.Printf("\nOnly files in that date range were considered, "+
			"out of %d indexed.\n", total)
	default:
		fmt.Printf("\n%d files are indexed. Try fewer or more general words.\n", total)
	}
	fmt.Println("Or index somewhere else:  mem scan <dir>")
}

// printResults renders the ranked list.
func printResults(query string, pq parsedQuery, results []result, conf []float64, limit int) {
	fmt.Printf("Found %d memories for %q\n\n", len(results), query)

	// Show what the query was understood as. This transparency is the whole
	// point of the tool: the user can see why these results appeared.
	if len(pq.terms) > 0 || len(pq.extensions) > 0 || pq.window.label != "" {
		fmt.Println("Understood as:")
		if len(pq.terms) > 0 {
			fmt.Printf("  looking for: %s\n", strings.Join(dedupe(pq.terms), ", "))
		}
		if len(pq.extensions) > 0 {
			fmt.Printf("  file type:   %s\n", strings.Join(pq.extensions, " or "))
		}
		if pq.window.label != "" {
			fmt.Printf("  modified:    %s\n", pq.window.label)
		}
		fmt.Println()
	}

	for i, r := range results {
		fmt.Printf("[%2d%%] %s\n", int(conf[i]+0.5), r.Filename)
		fmt.Printf("       %s\n", prettyPath(r.Path))

		var bits []string
		if len(r.Matched) > 0 {
			bits = append(bits, "Matches: "+strings.Join(r.Matched, ", "))
		}
		if r.Snippet != "" {
			bits = append(bits, r.Snippet)
		}
		meta := fmt.Sprintf("%s · %s · %s", humanBytes(uint64(r.Size)),
			timeAgo(r.Modified), r.Category)
		bits = append(bits, meta)
		for _, b := range bits {
			fmt.Printf("       %s\n", b)
		}
		fmt.Println()
	}

	if len(results) == limit {
		fmt.Printf("Showing top %d. Use --limit N for more.\n", limit)
	}
}

// dedupe removes repeats while preserving order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// startOfDay returns the unix timestamp of midnight for the day containing ts.
func startOfDay(ts int64) int64 {
	return ts - ((ts%86400)+86400)%86400
}

// todayUnix is a seam for testing time-dependent behaviour.
var todayUnix = func() int64 { return timeNow().Unix() }

// ensure the imports used by helper functions are all reachable here.
var _ = os.Stdout
var _ = filepath.Base
