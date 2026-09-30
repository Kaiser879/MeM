package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The query parser is the part of the system most likely to break silently:
// a regression there does not crash, it just quietly returns worse results.
// These tests pin the behaviour the demo depends on.

func TestParseQueryStripsFiller(t *testing.T) {
	pq := parseQuery("the PDF about Linux process scheduling")

	if len(pq.extensions) != 1 || pq.extensions[0] != ".pdf" {
		t.Errorf("expected a .pdf filter, got %v", pq.extensions)
	}
	for _, term := range pq.terms {
		if term == "the" || term == "about" {
			t.Errorf("stop word %q leaked into keywords: %v", term, pq.terms)
		}
	}
	if !contains(pq.terms, "linux") || !contains(pq.terms, "process") {
		t.Errorf("expected linux+process keywords, got %v", pq.terms)
	}
}

func TestParseQueryConsumesTimeWords(t *testing.T) {
	pq := parseQuery("maths assumptions I downloaded last week")

	if pq.window.label != "last week" {
		t.Fatalf("expected a 'last week' window, got %q", pq.window.label)
	}
	for _, term := range pq.terms {
		if term == "last" || term == "week" {
			t.Errorf("time word %q must not also be a keyword: %v", term, pq.terms)
		}
	}
	if !contains(pq.terms, "maths") {
		t.Errorf("real keyword was dropped: %v", pq.terms)
	}
}

func TestParseQuerySingularises(t *testing.T) {
	pq := parseQuery("documents about processes")
	if !contains(pq.terms, "process") {
		t.Errorf("expected 'process' from 'processes', got %v", pq.terms)
	}
}

func TestFTSQueryIsPrefixAnd(t *testing.T) {
	got := ftsQuery([]string{"linux", "process"})
	if got != `"linux"* OR "process"*` {
		t.Errorf("unexpected MATCH expression: %q", got)
	}
}

func TestConfidenceHandlesTieScores(t *testing.T) {
	// When every term appears in every document, bm25 returns zero for all
	// of them. The display must not pretend that is a ranking.
	results := []result{{Score: 0}, {Score: 0}, {Score: 0}}
	got := confidence(results)
	for i, v := range got {
		if v <= 0 || v > 100 {
			t.Errorf("score %d out of range: %v", i, v)
		}
	}
	if got[0] != got[1] || got[1] != got[2] {
		t.Errorf("tied inputs should produce tied output, got %v", got)
	}
}

func TestConfidenceNormalisesBestToTop(t *testing.T) {
	results := []result{{Score: -4}, {Score: -2}, {Score: -1}}
	got := confidence(results)
	if got[0] < got[1] || got[1] < got[2] {
		t.Errorf("scores should be descending, got %v", got)
	}
	if got[0] < 99 {
		t.Errorf("best hit should read near 100, got %v", got[0])
	}
}

func TestCategoryForDocuments(t *testing.T) {
	cases := map[string]string{
		// Generic study notes with no stronger signal are a College guess.
		"1st Year Syllabus.pdf": "College",
		"OS_notes.pdf":          "College",
		// A specific subject beats the generic word "notes". This is the
		// ordering guarantee that docKeywords documents.
		"ubuntu_kernel_notes.txt":       "Linux",
		"golang_concurrency_lecture.md": "Programming",
		"my_resume_final.pdf":           "Personal",
	}
	for name, want := range cases {
		if got := categoryFor(filepath.Ext(name), name); got != want {
			t.Errorf("categoryFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCategoryForMedia(t *testing.T) {
	if got := categoryFor(".jpg", "photo.jpg"); got != "Images" {
		t.Errorf("expected Images, got %q", got)
	}
	if got := categoryFor(".mp4", "clip.mp4"); got != "Videos" {
		t.Errorf("expected Videos, got %q", got)
	}
}

// TestExtractPlainText reads a real temp file through the extractor.
func TestExtractPlainText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.md")
	body := "# Linux processes\nScheduling notes."
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	text, kind, err := Extract(p, ".md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != ".md" {
		t.Errorf("expected kind .md, got %q", kind)
	}
	// The whole file should survive extraction verbatim.
	if text != body {
		t.Errorf("extracted text = %q, want %q", text, body)
	}
}

// TestExtractMissingFileIsNotFatal: a file we cannot read must return a
// "meta" classification rather than an error the caller has to special-case.
func TestExtractMissingFileIsNotFatal(t *testing.T) {
	_, kind, err := Extract("/definitely/not/here.pdf", ".pdf")
	if err == nil {
		t.Error("expected an error for a missing file")
	}
	if kind == "" {
		t.Error("expected a classification even on failure")
	}
}

func TestScanFindsFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.md"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", ".hidden"), []byte("h"), 0o644)

	files, stats, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 visible files, got %d", len(files))
	}
	if stats.SkippedMeta != 1 {
		t.Errorf("expected 1 hidden file skipped, got %d", stats.SkippedMeta)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := expandHome("~/Downloads"); got != filepath.Join(home, "Downloads") {
		t.Errorf("expandHome(~/Downloads) = %q", got)
	}
	if got := expandHome("/tmp/x"); got != "/tmp/x" {
		t.Errorf("absolute path should pass through, got %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{0: "0 B", 512: "512 B", 2048: "2.0 KB", 5 << 20: "5.0 MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestOCRQualityGate pins the accept/reject decision using text captured from
// the real sample corpus.
//
// The thresholds were derived from these exact measurements, so this test
// fails if someone loosens the gate into indexing OCR noise. That is the
// failure mode that matters: a searchable "gory BEE AWK" makes every future
// query worse.
func TestOCRQualityGate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			// func ratio 0.367: a photographed question sheet. This is the
			// kind of image the product is actually for.
			name: "clean printed text is accepted",
			text: "1. What is the unit of resistance? 2. Define electric current " +
				"3. Write the unit of power 4. What is the unit of energy? " +
				"5. State Ohm's Law. 6. Write the unit of inductance.",
			want: true,
		},
		{
			// func ratio 0.077: handwriting misread as fragments.
			name: "handwriting noise is rejected",
			text: "T ——— s AR R R gory BEE (HW) AWK Jntméh 'Jofor'ci(&c(t} " +
				"'j(g(ill?ié> .fid = i] — Watt (w). - S <7 Joudes C 7).",
			want: false,
		},
		{
			// func ratio 0.091: the hardest noise case, which scored higher
			// than the others. The gate must still reject it.
			name: "worst measured noise is still rejected",
			text: "RS PP folla Kouaes spenein Ales cowton J'w e: oxolless T " +
				"fL Load U AAM_m_thLfima.u n Qdeal amol Prackicol Leuwnent",
			want: false,
		},
		{
			name: "too little text to judge",
			text: "ok",
			want: false,
		},
		{
			name: "screenshot style text is accepted",
			text: "Follow the account for more Linux tips and terminal tricks. " +
				"The new release is out now and you should read the notes " +
				"before you upgrade your system this week.",
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeReadableText(c.text); got != c.want {
				t.Errorf("looksLikeReadableText() = %v, want %v\ntext: %.90q",
					got, c.want, c.text)
			}
		})
	}
}

// TestOCRVerdictSeparatesNoTextFromNoise pins the distinction the scan report
// depends on.
//
// Both cases used to be reported as "unreadable noise", which is actively
// misleading. A picture of a dragon produces two or three stray characters:
// there is nothing to read, and no threshold change will ever find it. A
// handwriting sample produces hundreds of characters that are not prose: the
// reading failed. Telling a user the second when it was the first sends them
// to tune a threshold that cannot help, which is exactly the wrong advice for a
// file that contains no words.
func TestOCRVerdictSeparatesNoTextFromNoise(t *testing.T) {
	cases := []struct {
		name string
		text string
		want ocrVerdict
	}{
		{
			// Measured: the dragon screenshot OCRs to these three characters
			// at 150, 300 and 600 DPI, across five page-segmentation modes,
			// and under inversion and upscaling. There is no text in it.
			name: "wordless image is no text, not noise",
			text: "e 2 -",
			want: ocrNothing,
		},
		{
			name: "empty output is no text",
			text: "",
			want: ocrNothing,
		},
		{
			// Measured: a WhatsApp contact screenshot. Legible, but names and
			// phone numbers are not prose, so the English-word gate refuses
			// them (func ratio 0.070-0.084 against a floor of 0.18).
			name: "contact list is noise, not absence",
			text: "Aniket Vidhated +9152702 50821 Dhruv Meher +9178219 37897 " +
				"Dheeraj +9189990 39821 Janhavi Bandekar 49193220 12637",
			want: ocrNoise,
		},
		{
			name: "handwriting fragments are noise",
			text: "T ——— s AR R R gory BEE (HW) AWK Jntméh 'Jofor'ci(&c(t} " +
				"'j(g(ill?ié> .fid = i] — Watt (w). - S <7 Joudes C 7).",
			want: ocrNoise,
		},
		{
			name: "real prose is readable",
			text: "Naming Convention. Service units carry a .service extension. " +
				"Every service has a descriptive name, and the name usually " +
				"makes the purpose of the service obvious.",
			want: ocrReadable,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyOCR(c.text); got != c.want {
				t.Errorf("classifyOCR() = %v, want %v\ntext: %.90q", got, c.want, c.text)
			}
		})
	}
}

func TestOCRDegradesGracefullyWithoutEngine(t *testing.T) {
	// The contract that matters most for portability: with no OCR engine
	// present, an image yields no text and no panic. Never an error that
	// would fail a scan.
	dir := t.TempDir()
	p := filepath.Join(dir, "shot.png")
	// Big enough to pass the size floor, so we exercise the engine lookup.
	if err := os.WriteFile(p, make([]byte, minOCRBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	text, kind, err := Extract(p, ".png")
	if err != nil {
		t.Fatalf("a missing OCR engine must not error, got %v", err)
	}
	if text != "" {
		t.Errorf("expected no text from a blank image, got %.60q", text)
	}
	if kind != ".png" {
		t.Errorf("expected kind .png, got %q", kind)
	}
}

func TestImageExtensionsAreSharedWithOrganizer(t *testing.T) {
	// The extractor and the organizer must agree on what an image is, or
	// search and organize will disagree about the same file.
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".tiff"} {
		if !imageExtensions[ext] {
			t.Errorf("%s missing from imageExtensions", ext)
		}
		if got := categorizeFor(ext, "file"+ext); got != "Images" {
			t.Errorf("categorizeFor(%q) = %q, want Images", ext, got)
		}
		if !isTextReadable(minOCRBytes+1, ext) {
			t.Errorf("isTextReadable(%q) = false, want true", ext)
		}
	}
}

func TestTinyImagesAreNotOCRd(t *testing.T) {
	// Below the size floor, OCR costs time and reliably returns nothing.
	if isTextReadable(minOCRBytes-1, ".png") {
		t.Error("a tiny image should not be considered readable")
	}
}

// TestNoMatchDoesNotFallBackToRecentFiles guards the most trust-damaging bug
// in the tool: answering a query that matched nothing with unrelated recent
// files, printed at 60% confidence.
//
// The user's cost of this bug is high. They see three files with percentages
// and conclude the tool is confidently wrong, rather than admitting a miss.
func TestNoMatchDoesNotFallBackToRecentFiles(t *testing.T) {
	store := newTestStore(t)
	seedFiles(t, store,
		fileSpec{name: "alpha.pdf", content: "quantum mechanics syllabus"},
		fileSpec{name: "beta.pdf", content: "operating system scheduling"},
	)

	// A query whose keywords appear nowhere must return nothing at all.
	rows, pq, err := store.Search("zzzqqqxyzzy wobbleflurb", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		var names []string
		for _, r := range rows {
			names = append(names, r.Source+":"+r.Filename)
		}
		t.Errorf("expected no results for a non-matching query, got %v", names)
	}
	if len(pq.terms) != 2 {
		t.Errorf("expected both terms preserved for the error message, got %v", pq.terms)
	}
}

// TestTypeHintAloneStillListsFiles pins the behaviour that the fix must NOT
// break: a query made only of stop words and a type hint is a legitimate
// "show me the PDFs" request, not a miss.
func TestTypeHintAloneStillListsFiles(t *testing.T) {
	store := newTestStore(t)
	seedFiles(t, store,
		fileSpec{name: "alpha.pdf", content: "quantum mechanics syllabus"},
		fileSpec{name: "notes.txt", content: "grocery list"},
	)

	rows, pq, err := store.Search("show me the pdf", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pq.terms) != 0 {
		t.Fatalf("expected the query to parse to no terms, got %v", pq.terms)
	}
	if len(rows) == 0 {
		t.Fatal("a type-hint-only query should still list files, got nothing")
	}
	for _, r := range rows {
		if r.Extension != ".pdf" {
			t.Errorf("type hint not honoured: got %s in results", r.Extension)
		}
	}
}

// TestNarrowTypeHintWithNoKeywordMatchReturnsNothing covers the specific case
// that motivated the fix: a type hint plus keywords that match nothing. The
// old code returned PDFs that had nothing to do with the keywords.
func TestNarrowTypeHintWithNoKeywordMatchReturnsNothing(t *testing.T) {
	store := newTestStore(t)
	seedFiles(t, store,
		fileSpec{name: "alpha.pdf", content: "quantum mechanics syllabus"},
		fileSpec{name: "beta.pdf", content: "operating system scheduling"},
	)

	rows, _, err := store.Search("the pdf about zzzqqqxyzzy", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("expected nothing, got %d unrelated files", len(rows))
	}
}

// TestNoMatchOutputNamesTheConstraint checks that the failure message points
// at the actual cause instead of a generic suggestion.
// TestNoMatchStillZeroExitCode matters at the CLI level: an honest miss is a
// successful run of the program. Reporting failure would make shell scripts
// and `&&` chains treat a correct answer as a crash.
func TestNoMatchStillZeroExitCode(t *testing.T) {
	// An honest miss is a successful run of the program. Reporting failure
	// would make shell scripts treat a correct answer as a crash.
	if runCLI(t, "search", "zzzqqqxyzzy wobbleflurb") {
		t.Error("a no-match search should succeed, not report an error")
	}
	// Real errors must still fail, or the check above is worthless.
	if !runCLI(t, "frobnicate") {
		t.Error("an unknown command should report an error")
	}
	if !runCLI(t, "search") {
		t.Error("a missing query should report an error")
	}
}

// runCLI calls the real run() dispatcher against a throwaway index and reports
// whether it would have exited non-zero. It goes through run() rather than
// main() precisely because run() returns an error instead of calling os.Exit,
// which is what makes this checkable at all.
func runCLI(t *testing.T, args ...string) (failed bool) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "test.db")

	// Seed one document so a search exercises the real "indexed but no
	// match" path rather than the empty-index shortcut.
	store, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	fi := FileInfo{Path: "/test/a.pdf", Filename: "a.pdf", Ext: ".pdf", Size: 10,
		ModifiedAt: time.Now().Unix()}
	if err := store.Upsert(fi, "quantum mechanics syllabus", "", "College", "sha"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	return run(append([]string{"--db", db}, args...)) != nil
}

// TestNoMatchNamesTextFreeFiles covers the dead end that cost real debugging
// time. Searching a folder of screenshots for "dragon" returns nothing, and
// the files are not broken and the search is not wrong: they hold no words.
// Without naming that population the only advice offered is "try fewer or more
// general words", which cannot possibly work.
func TestNoMatchNamesTextFreeFiles(t *testing.T) {
	store := newTestStore(t)

	// One document with text, three with none: the shape of a screenshots
	// folder next to some notes.
	seedFiles(t, store, fileSpec{name: "notes.pdf", content: "quantum mechanics"})
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		fi := FileInfo{Path: "/test/" + name, Filename: name, Ext: ".png",
			Size: 100, ModifiedAt: time.Now().Unix()}
		if err := store.Upsert(fi, "", "meta", "Images", "sha"); err != nil {
			t.Fatal(err)
		}
	}

	rows, pq, err := store.Search("dragon", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no match, got %d results", len(rows))
	}
	if pq.unsearchable != 3 {
		t.Fatalf("expected 3 text-free files to be counted, got %d", pq.unsearchable)
	}

	out := captureStdout(t, func() { printNoMatch("dragon", pq, 4) })
	for _, want := range []string{"3 indexed files", "no readable text", "filename"} {
		if !strings.Contains(out, want) {
			t.Errorf("no-match output missing %q:\n%s", want, out)
		}
	}
}

// TestNoMatchStaysQuietWhenEverythingHasText keeps the new paragraph from
// becoming noise on an ordinary miss. If every indexed file has text, the
// absence of text is not the explanation and must not be offered.
func TestNoMatchStaysQuietWhenEverythingHasText(t *testing.T) {
	store := newTestStore(t)
	seedFiles(t, store, fileSpec{name: "one.pdf", content: "quantum mechanics"},
		fileSpec{name: "two.pdf", content: "operating systems"})

	_, pq, err := store.Search("zzzqqqxyzzy", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if pq.unsearchable != 0 {
		t.Errorf("all files have text, so unsearchable should be 0, got %d", pq.unsearchable)
	}

	out := captureStdout(t, func() { printNoMatch("zzzqqqxyzzy", pq, 2) })
	if strings.Contains(out, "no readable text") {
		t.Errorf("text-free note should be absent when it does not apply:\n%s", out)
	}
}

func TestNoMatchOutputNamesTheConstraint(t *testing.T) {
	pq := parsedQuery{terms: []string{"nothing"}}
	pq.extensions = []string{".pdf"}

	out := captureStdout(t, func() { printNoMatch("nothing here", pq, 42) })

	for _, want := range []string{"No match", "nothing", "42", ".pdf"} {
		if !strings.Contains(out, want) {
			t.Errorf("no-match output missing %q:\n%s", want, out)
		}
	}
}

// TestDateFilterIsNamedAsTheBlocker covers the most common cause of an empty
// result. "syllabus last week" returning nothing while syllabus files plainly
// exist looks like a broken tool unless the message says the date did it.
func TestDateFilterIsNamedAsTheBlocker(t *testing.T) {
	store := newTestStore(t)

	// Three documents that match, all older than a week.
	for i, name := range []string{"one.pdf", "two.pdf", "three.pdf"} {
		fi := FileInfo{
			Path: "/test/" + name, Filename: name, Ext: ".pdf",
			Size: 100, ModifiedAt: time.Now().Unix() - int64(20+i)*86400,
		}
		if err := store.Upsert(fi, "syllabus of physics", "", "College", "sha"+name); err != nil {
			t.Fatal(err)
		}
	}

	rows, pq, err := store.Search("syllabus last week", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected the date filter to exclude everything, got %d", len(rows))
	}
	if pq.blockedByDate != 3 {
		t.Errorf("expected to detect 3 date-blocked matches, got %d", pq.blockedByDate)
	}

	out := captureStdout(t, func() { printNoMatch("syllabus last week", pq, 3) })
	if !strings.Contains(out, "3 files, but outside the date range") {
		t.Errorf("message should name the date as the blocker:\n%s", out)
	}
}

// TestNoDateHintWhenNothingMatchedAnywhere ensures we never claim the date
// blocked results that simply do not exist.
func TestNoDateHintWhenNothingMatchedAnywhere(t *testing.T) {
	store := newTestStore(t)
	seedFiles(t, store, fileSpec{name: "alpha.pdf", content: "quantum mechanics"})

	_, pq, err := store.Search("zzzqqqxyzzy last week", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if pq.blockedByDate != 0 {
		t.Errorf("blockedByDate should be 0 when nothing matches at all, got %d",
			pq.blockedByDate)
	}
}

// ---------------------------------------------------------------------------
// Pruning deleted files.
//
// A row for a deleted file used to survive forever, and mem search would keep
// returning it at 99% confidence. Nothing in the pipeline could remove a row,
// so re-scanning did not help. These tests pin both the fix and, more
// importantly, the two ways a prune could destroy live data.
// ---------------------------------------------------------------------------

// indexTempFile writes a real file and records it in the store, so the store
// and the filesystem agree the way they do after a genuine scan.
func indexTempFile(t *testing.T, s *Store, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	fi := FileInfo{Path: p, Filename: name, Ext: filepath.Ext(name),
		Size: st.Size(), ModifiedAt: st.ModTime().Unix()}
	if err := s.Upsert(fi, body, "", documentCategory(name), "sha"); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPruneRemovesDeletedFiles(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()

	keep := indexTempFile(t, store, dir, "keep.txt", "quantum mechanics notes")
	gone := indexTempFile(t, store, dir, "gone.txt", "operating system scheduling")

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// The set the walk would have produced: keep was seen, gone was not.
	pruned, err := store.PruneMissing(dir, map[string]bool{keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != gone {
		t.Fatalf("expected only %s to be pruned, got %v", gone, pruned)
	}

	if _, found, _ := store.Lookup(keep); !found {
		t.Error("the file still on disk must survive the prune")
	}
	if _, found, _ := store.Lookup(gone); found {
		t.Error("the deleted file's row should be gone")
	}
}

// TestPruneIsScopedToScannedRoot is the destructive-failure guard. The index is
// shared across every folder ever scanned, so pruning ~/Downloads must never
// reach into ~/Pictures. A bug here silently deletes a user's whole index for
// one folder.
func TestPruneIsScopedToScannedRoot(t *testing.T) {
	store := newTestStore(t)
	folderA := t.TempDir()
	folderB := t.TempDir()

	// Both are indexed, then both files are deleted. Only B is re-scanned.
	deletedA := indexTempFile(t, store, folderA, "a.txt", "alpha quantum notes")
	deletedB := indexTempFile(t, store, folderB, "b.txt", "bravo topology notes")
	if err := os.Remove(deletedA); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deletedB); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.PruneMissing(folderB, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != deletedB {
		t.Fatalf("scanning B should prune only B, got %v", pruned)
	}

	// A's file is gone from disk, but A was not the folder scanned. Its row
	// must survive: a later scan of A will clean it up.
	if _, found, _ := store.Lookup(deletedA); !found {
		t.Error("pruning folder B deleted a row belonging to folder A")
	}
}

// TestPruneKeepsFilesTheWalkSkipped pins the stat safety net. "Not seen by the
// walk" does not mean "deleted": the walk skips files over 64 MiB, hidden
// files, and everything under a hidden directory. Pruning on the walk's say-so
// alone would delete rows for files sitting right there on disk.
func TestPruneKeepsFilesTheWalkSkipped(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()

	// Pretend this was indexed when it was small enough to read.
	big := filepath.Join(dir, "huge.pdf")
	if err := os.WriteFile(big, []byte("plant physiology"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi := FileInfo{Path: big, Filename: "huge.pdf", Ext: ".pdf",
		Size: maxIndexableSize + 1, ModifiedAt: time.Now().Unix()}
	if err := store.Upsert(fi, "", "meta", "College", "sha"); err != nil {
		t.Fatal(err)
	}

	// The walk skipped it (too large), so it is absent from the seen set.
	pruned, err := store.PruneMissing(dir, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Errorf("a file that exists must never be pruned, got %v", pruned)
	}
	if _, found, _ := store.Lookup(big); !found {
		t.Error("row for a skipped-but-present file was deleted")
	}
}

// TestPruneRespectsSiblingPrefix guards the SQL. "substr" is used instead of
// LIKE precisely because LIKE treats "_" as a single-character wildcard.
//
// The two directories below are identical except at the position where parent
// has "_" and sibling has "X". That is the only arrangement that exposes the
// bug: a LIKE pattern of parent+"/"+"%" treats the "_" as a one-character
// wildcard, matches the "X", and then swallows everything under the sibling.
// Suffix-style siblings (/tmp/a and /tmp/a_other) would not catch it, because
// the path separator never lines up with the wildcard.
func TestPruneRespectsSiblingPrefix(t *testing.T) {
	store := newTestStore(t)
	base := t.TempDir()
	parent := filepath.Join(base, "a_b")
	sibling := filepath.Join(base, "aXb") // differs exactly at the "_"

	for _, d := range []string{parent, sibling} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	siblingFile := indexTempFile(t, store, sibling, "s.txt", "sibling folder notes")
	if err := os.Remove(siblingFile); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.PruneMissing(parent, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Errorf("a sibling directory matching the prefix as a wildcard must not be pruned, got %v", pruned)
	}
}

// newTestStore opens a throwaway store in a temp dir. Every test gets its own
// database so they cannot interfere with each other, and nothing here can
// touch the user's real index at ~/.mem/index.db.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

type fileSpec struct {
	name    string
	content string
}

// seedFiles inserts documents straight into the store, bypassing the scanner
// so a test does not need real files on disk for the parts it is not testing.
func seedFiles(t *testing.T, s *Store, specs ...fileSpec) {
	t.Helper()
	for i, spec := range specs {
		fi := FileInfo{
			Path:       "/test/" + spec.name,
			Filename:   spec.name,
			Ext:        strings.ToLower(filepath.Ext(spec.name)),
			Size:       int64(len(spec.content)),
			ModifiedAt: time.Now().Unix() - int64(i)*86400,
		}
		err := s.Upsert(fi, spec.content, "", documentCategory(fi.Filename), "sha"+spec.name)
		if err != nil {
			t.Fatalf("seed %s: %v", spec.name, err)
		}
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// TestHelpIsAvailableForEveryCommand stops the help system from silently
// rotting. A command with no help entry would print the whole overview and
// exit 1, which is exactly what a confused user experiences as "broken".
func TestHelpIsAvailableForEveryCommand(t *testing.T) {
	commands := []string{"scan", "search", "organize", "related", "stats", "help", "version"}
	for _, c := range commands {
		text, ok := helpFor(c)
		if !ok {
			t.Errorf("no help registered for %q", c)
			continue
		}
		if len(text) < 80 {
			t.Errorf("help for %q is only %d chars, too thin to be useful", c, len(text))
		}
		if !strings.Contains(text, c) && !strings.Contains(text, strings.TrimSuffix(c, "e")) {
			t.Errorf("help for %q does not mention the command name", c)
		}
	}
}

// TestHelpAliasesResolve pins that `mem help find` and `mem help organise`
// work, since those are documented aliases and would be a papercut otherwise.
func TestHelpAliasesResolve(t *testing.T) {
	for alias, canonical := range map[string]string{
		"find":     "search",
		"organise": "organize",
	} {
		got, ok := helpFor(alias)
		if !ok {
			t.Errorf("alias %q has no help", alias)
			continue
		}
		want, _ := helpFor(canonical)
		if got != want {
			t.Errorf("alias %q should show the %q help", alias, canonical)
		}
	}
}

// TestCommandHelpFlagEqualsHelpCommand checks both spellings of asking for
// help produce identical output. Divergence here is easy to introduce and
// hard to notice.
func TestCommandHelpFlagEqualsHelpCommand(t *testing.T) {
	fromFlag := captureStdout(t, func() { printCommandHelp("search") })
	fromSub := captureStdout(t, func() { printCommandHelp("find") })
	if fromFlag != fromSub {
		t.Error("`mem search --help` and `mem help find` should be identical")
	}
}

// TestHelpFlagDoesNotTouchDisk: `mem scan --help /nonsense` must explain
// itself rather than fail on the path. Checking help before dispatch is what
// guarantees that.
func TestHelpFlagDoesNotTouchDisk(t *testing.T) {
	out := captureStdout(t, func() {
		if err := run([]string{"scan", "--help", "/definitely/not/here"}); err != nil {
			t.Errorf("--help should not attempt the scan: %v", err)
		}
	})
	if !strings.Contains(out, "mem scan <directory>") {
		t.Errorf("expected scan help, got:\n%s", out)
	}
}

// TestMissingArgumentErrorsPointAtHelp keeps error messages actionable. The
// command name is enough to tell someone which help page to read.
func TestMissingArgumentErrorsPointAtHelp(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"search without a query", []string{"search"}, "mem help search"},
		{"scan without a directory", []string{"scan"}, "mem help scan"},
		{"organize without a directory", []string{"organize"}, "mem help organize"},
		{"related without a filename", []string{"related"}, "mem help related"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := run(append([]string{"--db", filepath.Join(t.TempDir(), "t.db")}, c.args...))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should point at %q, got: %v", c.want, err)
			}
		})
	}
}

// TestOverviewListsEveryCommand protects against adding a command and
// forgetting the overview. The overview is the first thing anyone reads.
func TestOverviewListsEveryCommand(t *testing.T) {
	for _, c := range helpTopics() {
		if !strings.Contains(usage, c) {
			t.Errorf("overview does not mention the %q command", c)
		}
	}
	// Safety promises belong in the overview: it is where someone decides
	// whether to trust the tool with a real folder.
	for _, promise := range []string{"Delete", "network"} {
		if !strings.Contains(usage, promise) {
			t.Errorf("overview should state the %q safety promise", promise)
		}
	}
}

// TestTimeHintsDoNotLeakIntoKeywords: once "this year" has become a date
// range, "year" must not also be searched as a content word. Otherwise the
// query silently demands that every match literally contain the word "year",
// which quietly destroys recall.
func TestTimeHintsDoNotLeakIntoKeywords(t *testing.T) {
	for _, q := range []string{
		"syllabus this year",
		"notes last year",
		"diagram yesterday",
		"slides this month",
		"chart last week",
		"memo today",
		"file recently",
	} {
		pq := parseQuery(q)
		if pq.window.label == "" {
			t.Errorf("%q: expected a time hint, got none", q)
			continue
		}
		for _, bad := range []string{"year", "years", "week", "month", "day", "today", "recent", "last", "this"} {
			for _, term := range pq.terms {
				if term == bad {
					t.Errorf("%q: time word %q leaked into keywords %v", q, bad, pq.terms)
				}
			}
		}
	}
}

// TestNarrowerTimeHintWins checks the ordering of the time-hint cases. A
// broader match evaluated first would swallow the narrower one, so "this year"
// would silently become "this" and return a whole year of files.
func TestNarrowerTimeHintWins(t *testing.T) {
	cases := map[string]string{
		"syllabus this year":  "this year",
		"syllabus last year":  "last year",
		"syllabus last month": "last month",
		"syllabus last week":  "last week",
		"syllabus this week":  "this week",
		"note from yesterday": "yesterday",
	}
	for query, want := range cases {
		if got := parseQuery(query).window.label; got != want {
			t.Errorf("%q: got time hint %q, want %q", query, got, want)
		}
	}
}

// TestTimeWindowBoundariesAreOrdered checks that each hint produces a sane
// range: "last week" must end before "this week" begins, or the two overlap
// and neither means anything.
func TestTimeWindowBoundariesAreOrdered(t *testing.T) {
	thisWeek := parseQuery("x this week").window
	lastWeek := parseQuery("x last week").window
	if lastWeek.before == 0 || thisWeek.after == 0 {
		t.Fatal("both windows should be bounded")
	}
	if lastWeek.before != thisWeek.after {
		t.Errorf("last week ends at %d but this week starts at %d; they should meet, not gap",
			lastWeek.before, thisWeek.after)
	}
	if lastWeek.after >= lastWeek.before {
		t.Error("last week must be a real interval, not an inverted one")
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
