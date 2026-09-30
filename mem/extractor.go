package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// maxContentBytes caps how much extracted text we keep per file. Search only
// needs enough text to characterise a document; storing a whole textbook
// would bloat the index without improving recall.
const maxContentBytes = 200 << 10 // 200 KiB

// maxZipUncompressed bounds a single member read, so a zip bomb inside a
// document cannot exhaust memory during a scan.
const maxZipUncompressed = 8 << 20 // 8 MiB

// errTooLarge is returned when a file exceeds the extraction limits.
var errTooLarge = errors.New("file too large to extract")

// textExtensions lists the formats we can read words out of without help.
var textExtensions = map[string]bool{
	".txt":      true,
	".md":       true,
	".markdown": true,
	".rst":      true,
	".log":      true,
	".csv":      true,
	".json":     true,
	".html":     true,
	".htm":      true,
	".tex":      true,
}

// imageExtensions are the raster formats we can read text out of using OCR.
// Deliberately excludes .svg and .heic: SVG is XML (no OCR needed) and HEIC
// has no reliable decoder in either tesseract or our dependency set.
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true,
	".webp": true, ".bmp": true, ".tif": true, ".tiff": true, ".gif": true,
}

// minOCRBytes skips images too small to hold a screenshot of anything useful.
// Below this, OCR costs time and reliably returns nothing.
const minOCRBytes = 8 << 10 // 8 KiB

// isTextReadable predicts whether extraction will succeed, so that `scan`
// can report an honest "extracted text from N files" number before doing
// the work. Cheap metadata guess, no file is opened.
func isTextReadable(size int64, ext string) bool {
	if size == 0 || size > maxIndexableSize {
		return false
	}
	if textExtensions[ext] || ext == ".pdf" || ext == ".docx" || ext == ".pptx" {
		return true
	}
	return imageExtensions[ext] && size >= minOCRBytes
}

// Extract pulls readable text out of one file.
//
// Contract: a failure here is never fatal. A file we cannot read still gets
// indexed by name, size and timestamp, and stays discoverable — which is
// exactly how a screenshot with a useless filename remains findable.
func Extract(path, ext string) (text string, kind string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", "meta", err
	}
	if fi.Size() == 0 {
		return "", "meta", nil // empty or truncated download
	}

	switch {
	case textExtensions[ext]:
		t, err := extractPlain(path)
		return t, ext, err

	case ext == ".pdf":
		t, err := extractPDF(path)
		return t, ext, err

	case imageExtensions[ext]:
		// OCR is opportunistic: it either yields clean text or nothing.
		// Returning no error keeps a failed image from failing the scan.
		return extractImageOCR(path), ext, nil

	case ext == ".docx":
		t, err := extractOpenXML(path, "word/document.xml")
		return t, ext, err

	case ext == ".pptx":
		t, err := extractOpenXML(path, "ppt/slides/") // directory prefix
		return t, ext, err
	}

	return "", "meta", nil
}

// maxPDFPages bounds how deep we read. A 300-page textbook would take
// noticeably longer than the rest of the scan put together, and the first few
// pages identify a document perfectly well for search purposes.
const maxPDFPages = 40

// pdfTimeout caps a single PDF conversion. A malformed file must not be able
// to hang a whole scan.
const pdfTimeout = 30 * time.Second

// extractPDF pulls the text layer out of a PDF.
//
// Two extractors are used on purpose.
//
// poppler's pdftotext is tried first because it is dramatically more
// accurate: it knows real font metrics, so it can tell the gap between two
// words from the kerning gap inside one. Pure-Go libraries that guess that
// produce text like "Contents1WhatisLinux?" or "Lin ux", and a full-text
// index built on that cannot match the word "linux" at all.
//
// If poppler is not installed we fall back to the pure-Go library, which
// still returns usable words. Search quality degrades gracefully instead of
// breaking, and the program has no hard external dependency.
//
// Failure is normal and never fatal. A scanned PDF has no text layer (it is a
// photograph of pages), and an encrypted or corrupt PDF cannot be read. In
// both cases the file is simply indexed by name, size and timestamp, which is
// still enough for it to be found.
func extractPDF(path string) (string, error) {
	if text := extractPDFPoppler(path); strings.TrimSpace(text) != "" {
		return text, nil
	}
	return extractPDFPureGo(path)
}

// extractPDFPoppler shells out to poppler's pdftotext when it is installed.
func extractPDFPoppler(path string) string {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "" // not installed: caller falls back
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdfTimeout)
	defer cancel()

	// "-" as the output file means "write to stdout".
	cmd := exec.CommandContext(ctx, "pdftotext",
		"-q", "-enc", "UTF-8",
		"-f", "1", "-l", strconv.Itoa(maxPDFPages),
		path, "-")

	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return capText(string(out))
}

// extractPDFPureGo is the dependency-free fallback. It reconstructs words by
// joining positioned text runs; where a word is split across runs it may end
// up with a stray space, so it is a fallback and not the default.
func extractPDFPureGo(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	// ledongthuc/pdf reads via ReaderAt, so it needs the size up front.
	doc, err := pdf.NewReader(f, stat.Size())
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for i := 0; i < doc.NumPage() && i < maxPDFPages; i++ {
		if sb.Len() >= maxContentBytes {
			break
		}
		rows, err := doc.Page(i).GetTextByRow()
		if err != nil {
			continue // skip a page we cannot parse, keep the document
		}
		for _, r := range rows {
			for _, t := range r.Content {
				sb.WriteString(t.S)
				sb.WriteString(" ")
			}
			sb.WriteString("\n")
		}
	}
	return capText(sb.String()), nil
}

// capText trims to the size we are willing to store per file.
func capText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxContentBytes {
		return s[:maxContentBytes]
	}
	return s
}

// ---------------------------------------------------------------------------
// OCR
//
// OCR is the one extractor that routinely produces confident nonsense. Given a
// photograph of handwriting, tesseract will happily return
//
//	T ——— s AR R R gory BEE (HW) AWK
//
// rather than admitting failure. Indexing that is actively harmful: those
// fragments become searchable, so a future query for "bee" matches a photo of
// someone's handwriting. A tool that makes search worse is worse than one that
// admits it cannot read the file.
//
// So OCR output is treated as untrusted until it passes a quality gate.
// ---------------------------------------------------------------------------

// ocrTimeout caps a single image so one pathological file cannot hang a scan.
const ocrTimeout = 20 * time.Second

// ocrEnabled reports whether an OCR engine is installed. Resolved once, since
// it costs a filesystem lookup and the answer cannot change mid-run.
var ocrEnabled = sync.OnceValue(func() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
})

// ocrStats records what OCR did, so scan can report coverage honestly rather
// than implying every image was read successfully.
type ocrStats struct {
	used     int // images that produced indexable text
	rejected int // images the engine read but whose output was noise
	empty    int // images with no text at all
}

var ocrCounts = ocrStats{}

// ocrRejects is the subset of OCR attempts that produced noise.
var ocrRejects int

// ocrIndexed counts images that yielded text good enough to index.
var ocrIndexed int

// resetOCRStats clears the per-scan OCR counters. Without this, a second scan
// would report the first scan's numbers.
func resetOCRStats() {
	ocrCounts = ocrStats{}
	ocrRejects = 0
	ocrIndexed = 0
}

// extractImageOCR reads text out of a raster image, returning "" when it
// cannot produce text worth trusting.
//
// It degrades quietly: no engine installed, unreadable file, or output that
// looks like noise all return "" with no error, because a file with no
// searchable content is still perfectly findable by name.
func extractImageOCR(path string) string {
	if !ocrEnabled() {
		return ""
	}
	if st, err := os.Stat(path); err != nil || st.Size() < minOCRBytes {
		return ""
	}
	ocrCounts.used++

	ctx, cancel := context.WithTimeout(context.Background(), ocrTimeout)
	defer cancel()

	// "-" as the output base means "write to stdout" instead of a file.
	// (An empty base is silently wrong: tesseract reads the image, finds
	// nowhere to write, and returns nothing at all.)
	// --psm 6 treats the image as one uniform block of text, which suits
	// screenshots and chat images; the default mode assumes a scanned book
	// page and interleaves columns into nonsense.
	out, err := exec.CommandContext(ctx, "tesseract", path, "-",
		"--psm", "6", "--dpi", "150").Output()
	if err != nil {
		return ""
	}

	text := capText(string(out))
	if text == "" {
		// Nothing was read at all: an image with no text, or a page that
		// failed to parse. Not a rejection, just nothing to say.
		ocrCounts.empty++
		return ""
	}
	if !looksLikeReadableText(text) {
		// The engine produced output but it is not real text. Counting it
		// keeps scan honest about coverage.
		ocrCounts.rejected++
		ocrRejects++
		return ""
	}
	ocrIndexed++
	return text
}

// Thresholds for the OCR quality gate.
//
// These are measured against the sample corpus, not guessed. The share of
// "function words" (the, of, is, write) turned out to separate real text from
// OCR noise far better than word-shape alone:
//
//	clean printed question sheet -> func 0.367   accepted
//	handwriting noise            -> func <=0.091 rejected
//
// That is a 4x gap, and the threshold sits at 0.18, roughly midway in log
// space between the two populations, so the gate is not balanced on a knife
// edge. Word-shape is kept as a secondary floor because it catches short
// fragment output that happens to contain a stray function word.
const (
	ocrFuncRatioFloor = 0.18
	ocrWordRatioFloor = 0.30
)

// looksLikeReadableText decides whether OCR output is real text or noise.
//
// The signal is the share of "word-shaped" tokens: things that are 3-18
// characters long, contain a vowel, and are not shouting in all capitals.
// Genuine English prose is overwhelmingly made of such tokens. OCR noise is
// dominated by fragments, initialisms and consonant clusters ("AWK", "HW",
// "gory").
//
// A second, cheaper signal guards the long tail: real prose contains common
// English function words, because sentences are mostly short connective words
// and pronouns. Noise almost never does.
func looksLikeReadableText(s string) bool {
	tokens := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	// Too little text to judge. Three words is the smallest sample that can
	// distinguish a label from a fragment.
	if len(tokens) < 4 {
		return false
	}

	wordish, functionish := 0, 0
	for _, t := range tokens {
		if isWordShaped(t) {
			wordish++
		}
		if commonEnglishWords[strings.ToLower(t)] {
			functionish++
		}
	}

	// Function words are the primary signal. Real prose is roughly 30-50%
	// function words; OCR noise from handwriting or graphics is single-digit.
	if float64(functionish)/float64(len(tokens)) < ocrFuncRatioFloor {
		return false
	}
	// Word-shape is the secondary floor: catches output where nearly every
	// token is a fragment but one happened to be a common word.
	return float64(wordish)/float64(len(tokens)) >= ocrWordRatioFloor
}

// isWordShaped reports whether a token looks like an English word rather than
// an OCR artefact.
//
// This is the *secondary* signal in the quality gate, not the primary one. It
// was tried as the sole gate first and does not work: on the sample corpus the
// clean question sheet scored 0.57 while the noisiest handwriting scored 0.36,
// too little separation to set a safe threshold. The function-word share
// (0.367 vs 0.091) is what actually separates the two, and drives the
// decision in looksLikeReadableText. Keep this as a floor for catching short
// fragment output; do not promote it back to the main test.
func isWordShaped(t string) bool {
	if len(t) < 3 || len(t) > 18 {
		return false
	}
	hasVowel := false
	for _, r := range t {
		switch {
		case unicode.IsLetter(r):
		case r == '\'' || r == '-':
		default:
			// Digits or symbols inside a token mean it is not a word.
			return false
		}
		if strings.ContainsRune("aeiouyAEIOUY", r) {
			hasVowel = true
		}
	}
	if !hasVowel {
		return false
	}
	// A token in all caps is far more likely to be an initialism or a
	// misread graphic than a real word.
	upper := 0
	for _, r := range t {
		if unicode.IsUpper(r) {
			upper++
		}
	}
	return float64(upper)/float64(len([]rune(t))) < 0.8
}

// commonEnglishWords are the high-frequency words that appear in almost any
// piece of real prose. Used only as a "this looks like a sentence" signal.
var commonEnglishWords = map[string]bool{
	"the": true, "and": true, "of": true, "a": true, "to": true, "in": true,
	"is": true, "it": true, "you": true, "that": true, "he": true, "was": true,
	"for": true, "on": true, "are": true, "as": true, "with": true, "his": true,
	"they": true, "i": true, "at": true, "be": true, "this": true, "have": true,
	"from": true, "or": true, "one": true, "had": true, "by": true, "word": true,
	"but": true, "not": true, "what": true, "all": true, "were": true, "we": true,
	"when": true, "your": true, "can": true, "said": true, "there": true,
	"use": true, "an": true, "each": true, "which": true, "she": true, "do": true,
	"how": true, "their": true, "if": true, "will": true, "up": true, "other": true,
	"about": true, "out": true, "many": true, "then": true, "them": true,
	"these": true, "so": true, "some": true, "her": true, "would": true,
	"make": true, "like": true, "him": true, "into": true, "time": true,
	"has": true, "look": true, "two": true, "more": true, "write": true,
	"go": true, "see": true, "number": true, "no": true, "way": true,
	"could": true, "people": true, "my": true, "first": true,
	"water": true, "been": true, "call": true, "who": true, "its": true,
	"now": true, "find": true, "long": true, "down": true, "day": true,
	"did": true, "get": true, "come": true, "made": true, "may": true,
	"part": true, "define": true, "state": true, "name": true, "give": true,
	"using": true, "does": true, "set": true, "need": true, "mean": true,
	"know": true, "take": true,
}

// extractPlain reads a text file, stopping at the size cap.
func extractPlain(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, maxContentBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// xmlTags matches any XML/HTML tag so we can replace markup with whitespace.
var xmlTags = regexp.MustCompile(`<[^>]+>`)

var xmlEntities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
	"&apos;", "'", "&#39;", "'", "&nbsp;", " ", "\r\n", "\n",
)

// stripMarkup converts a chunk of XML into flowing text.
func stripMarkup(s string) string {
	s = xmlTags.ReplaceAllString(s, " ")
	s = xmlEntities.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// extractOpenXML reads text out of a .docx or .pptx.
//
// Both formats are ZIP archives full of XML, so no third-party parser is
// needed — which is why these two formats cost almost nothing to support.
func extractOpenXML(path, entry string) (string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer r.Close()

	var parts []string
	budget := maxContentBytes

	for _, f := range r.File {
		if !matchesEntry(f.Name, entry) {
			continue
		}
		if f.UncompressedSize64 > maxZipUncompressed {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue // skip a broken member rather than fail the document
		}
		b, err := io.ReadAll(io.LimitReader(rc, min64(int64(budget), maxZipUncompressed)))
		rc.Close()
		if err != nil || len(b) == 0 {
			continue
		}
		parts = append(parts, stripMarkup(string(b)))
		budget -= len(b)
		if budget <= 0 {
			break
		}
	}

	if len(parts) == 0 {
		return "", nil // image-only document: still indexed by metadata
	}
	text := strings.Join(parts, "\n")
	if len(text) > maxContentBytes {
		text = text[:maxContentBytes]
	}
	return text, nil
}

// matchesEntry reports whether a zip member name is the one we want.
// A plain name matches exactly; a trailing slash means "everything under
// this directory", which is how we collect every slide of a .pptx in order.
func matchesEntry(name, entry string) bool {
	if strings.HasSuffix(entry, "/") {
		return strings.HasPrefix(name, entry) && strings.HasSuffix(name, ".xml")
	}
	return name == entry
}

// categoryFor assigns a coarse category from the file extension.
// Deterministic on purpose: a rule the user can predict beats a classifier
// they cannot explain.
func categoryFor(ext, filename string) string {
	switch ext {
	case ".pdf", ".doc", ".docx", ".odt", ".rtf", ".txt", ".md", ".markdown",
		".ppt", ".pptx", ".odp", ".epub", ".pages":
		return documentCategory(filename)
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff", ".svg", ".heic":
		return "Images"
	case ".mp4", ".mkv", ".webm", ".mov", ".avi", ".flv", ".m4v":
		return "Videos"
	case ".mp3", ".wav", ".flac", ".m4a", ".ogg", ".aac":
		return "Audio"
	case ".go", ".c", ".cpp", ".h", ".hpp", ".py", ".rs", ".js", ".ts", ".java",
		".rb", ".sh", ".sql", ".php", ".cs", ".swift", ".kt":
		return "Programming"
	case ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar", ".tgz":
		return "Archives"
	case ".deb", ".rpm", ".appimage", ".exe", ".msi", ".dmg", ".pkg":
		return "Software"
	}
	return "Other"
}

// docKeywords maps a keyword in a document's name to a human-facing topic
// folder. Order matters: first match wins.
//
// The ordering is by specificity, not alphabetically. A file called
// "ubuntu_kernel_notes.txt" should land under Linux, not be swallowed by the
// generic word "notes", so strong subject signals are checked before weak
// generic ones. This keeps categorisation predictable for the user.
var docKeywords = []struct {
	topic string
	words []string
}{
	// Strong, unambiguous subject markers.
	{"Linux", []string{"linux", "ubuntu", "kernel", "bash", "shell", "unix", "systemd", "grep", "gnome"}},
	{"Programming", []string{"code", "dsa", "algorithm", "leetcode", "golang", "python", "java", "api", "sql", "web", "hack", "lab_manual", "programming", "source"}},

	// Personal paperwork. Kept above College so "resume" beats a stray
	// "assignment"-like word in a personal filename.
	{"Personal", []string{"resume", "curriculum vitae", "invoice", "receipt", "passport", "aadhaar", "salary", "diary", "personal", "bank"}},

	// College artefacts: generic enough that they act as a fallback, so they
	// come last. A file with no other signal and the word "notes" is a
	// reasonable guess for College.
	{"College", []string{"syllabus", "semester", "assignment", "exam", "question paper", "marks", "batch", "btech", "nep", "theory", "practical", "lab", "unit", "notes", "paper"}},
}

// documentCategory picks the topic folder for a document from its filename.
// Falls back to "Other" rather than guessing aggressively.
func documentCategory(filename string) string {
	lower := strings.ToLower(filename)
	base := strings.TrimSuffix(lower, filepath.Ext(lower))
	// Normalise separators and spaces so "operating-systems" matches
	// "operating_systems" too.
	norm := strings.NewReplacer("_", " ", "-", " ").Replace(base)

	for _, kw := range docKeywords {
		if kw.topic == "Other" {
			continue // the catch-all is handled below
		}
		for _, w := range kw.words {
			if strings.Contains(norm, strings.ReplaceAll(w, "_", " ")) {
				return kw.topic
			}
		}
	}
	return "Other"
}

// hashFile returns the hex SHA-256 of a file, used to notice when content
// changed underneath us. Empty string means "could not hash", which callers
// treat as "fall back to size and timestamp".
func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	// Hashing is streamed, never loading the whole file into memory.
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortedKeys is a small helper so map-driven output is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
