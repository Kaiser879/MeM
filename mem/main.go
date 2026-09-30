// Command mem is a local-first digital memory index.
//
//	mem scan <dir>        index a folder tree
//	mem search "<query>"  find files by what you remember, not their names
//	mem organize <dir>    preview a tidy-up plan (never destructive)
//	mem related <file>    find documents that overlap with one you have
//	mem stats             what the index currently holds
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mem: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches a command and returns an error instead of calling os.Exit,
// which keeps the failure path testable and keeps main() tiny.
func run(args []string) error {
	// A global flag may appear anywhere; pull it out before dispatch.
	dbPath, args, err := extractDBFlag(args)
	if err != nil {
		return err
	}

	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	command := args[0]
	rest := args[1:]

	// `mem help scan` and `mem scan --help` should both work, and neither
	// should touch the disk. Help is checked before the command runs so that
	// `mem scan --help ~/nonsense` explains itself instead of failing.
	if command == "help" || command == "-h" || command == "--help" {
		if len(rest) > 0 {
			return printCommandHelp(rest[0])
		}
		fmt.Print(usage)
		return nil
	}
	if command == "version" || command == "--version" || command == "-v" {
		fmt.Println("mem", version)
		return nil
	}
	if wantsHelp(rest) {
		return printCommandHelp(command)
	}

	switch command {
	case "scan":
		return cmdScan(rest, dbPath)
	case "search", "find":
		return cmdSearch(rest, dbPath)
	case "organize", "organise":
		return cmdOrganize(rest, dbPath)
	case "related":
		return cmdRelated(rest, dbPath)
	case "stats":
		return cmdStats(dbPath)
	default:
		// Print the overview before the error: someone who mistyped a
		// command needs to see what the commands are.
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// extractDBFlag pulls --db <path> out of the argument list.
func extractDBFlag(args []string) (dbPath string, rest []string, err error) {
	dbPath, err = DefaultDBPath()
	if err != nil {
		return "", nil, err
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--db" {
			if i+1 >= len(args) {
				return "", nil, errors.New("--db needs a path")
			}
			dbPath = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return dbPath, rest, nil
}

// cmdScan indexes a directory tree.
func cmdScan(args []string, dbPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("scan needs a directory, e.g. mem scan ~/Downloads\n\n%s", "run 'mem help scan' for details")
	}
	dir := expandHome(args[0])
	force := hasFlag(args, "--force")

	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	fmt.Printf("Scanning %s...\n\n", prettyPath(dir))

	files, stats, err := Scan(dir)
	if err != nil {
		return err
	}

	var indexed, extracted, unchanged int
	resetOCRStats()

	// Every path the walk produced, including the ones we are about to skip as
	// unchanged. PruneMissing compares against this set to spot rows whose
	// files have since been deleted.
	seen := make(map[string]bool, len(files))
	for _, fi := range files {
		seen[fi.Path] = true
	}

	for _, fi := range files {
		// Skip work entirely when size and mtime match what we stored. This
		// is what makes a second scan feel instant.
		if !force {
			if prev, found, err := store.Lookup(fi.Path); err == nil && found {
				if prev.Size == fi.Size && prev.Modified == fi.ModifiedAt {
					unchanged++
					continue
				}
			}
		}

		text, kind, xerr := Extract(fi.Path, fi.Ext)
		if xerr != nil {
			// Extraction problems are never fatal: the file still gets indexed
			// by name, size and timestamp, so it stays findable. A scanned PDF
			// or a corrupted file simply has no text to search.
			text, kind = "", "meta"
		}
		if strings.TrimSpace(text) == "" {
			// Extracted nothing usable: record it as metadata-only.
			text, kind = "", "meta"
		}
		if kind != "meta" {
			extracted++
		}

		cat := categoryFor(fi.Ext, fi.Filename)
		if err := store.Upsert(fi, text, kind, cat, hashFile(fi.Path)); err != nil {
			return fmt.Errorf("index %s: %w", fi.Path, err)
		}
		indexed++
	}

	// Drop rows for files that have been deleted since the last scan. Scoped to
	// the folder just walked, and only reached because Scan returned without
	// error — a walk that could not read the tree tells us nothing about what is
	// missing from it, so pruning on a partial walk would delete live rows.
	absRoot, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	pruned, err := store.PruneMissing(absRoot, seen)
	if err != nil {
		return fmt.Errorf("prune missing files: %w", err)
	}

	fmt.Printf("Indexed %d files\n", indexed)
	fmt.Printf("Extracted text from %d files\n", extracted)
	if ocrCounts.used > 0 {
		// Report OCR coverage precisely: how many images yielded text, how
		// many were unreadable, and how many produced noise we discarded.
		// Reporting a single blended number would overstate what happened.
		fmt.Printf("OCR'd %d images: %d readable, %d no text, %d unreadable noise\n",
			ocrCounts.used, ocrIndexed, ocrCounts.empty, ocrCounts.rejected)
	}
	if !ocrEnabled() {
		fmt.Println("OCR not installed: images are indexed by name only")
	}
	fmt.Printf("Skipped %d unchanged files\n", unchanged)
	if len(pruned) > 0 {
		// Removal is reported, never silent. Silently dropping index rows would
		// make search results change with no explanation for it.
		fmt.Printf("Removed %d file(s) that are no longer on disk\n", len(pruned))
		for i, p := range pruned {
			if i == 3 {
				fmt.Printf("  ... and %d more\n", len(pruned)-3)
				break
			}
			fmt.Printf("  %s\n", prettyPath(p))
		}
	}
	if stats.SkippedLarge > 0 {
		fmt.Printf("Skipped %d files over %d MB\n", stats.SkippedLarge, maxIndexableSize>>20)
	}
	if stats.SkippedMeta > 0 {
		fmt.Printf("Skipped %d hidden or non-regular files\n", stats.SkippedMeta)
	}
	total, _ := store.Count()
	fmt.Printf("\nMemory index updated. %d files known in total.\n", total)
	fmt.Printf("Index file: %s\n", store.Path())
	return nil
}

// cmdStats prints a summary of the index.
func cmdStats(dbPath string) error {
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
		fmt.Println("The index is empty. Try:  mem scan ~/Downloads")
		return nil
	}

	var bytes int64
	_ = store.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM files`).Scan(&bytes)

	fmt.Printf("Memories indexed: %d\n", total)
	fmt.Printf("Total size:       %s\n", humanBytes(uint64(bytes)))
	fmt.Printf("Index file:       %s\n\n", store.Path())

	fmt.Println("By category:")
	catRows, err := store.db.Query(`SELECT category, COUNT(*), SUM(size)
	                                 FROM files GROUP BY category ORDER BY 2 DESC`)
	if err != nil {
		return err
	}
	for catRows.Next() {
		var cat string
		var n int
		var sz int64
		catRows.Scan(&cat, &n, &sz)
		if cat == "" {
			cat = "Uncategorised"
		}
		fmt.Printf("  %-14s %4d  %s\n", cat, n, humanBytes(uint64(sz)))
	}
	catRows.Close()

	fmt.Println("\nBy file type:")
	extRows, err := store.db.Query(`SELECT extension, COUNT(*)
	                                 FROM files GROUP BY extension ORDER BY 2 DESC LIMIT 12`)
	if err != nil {
		return err
	}
	for extRows.Next() {
		var ext string
		var n int
		extRows.Scan(&ext, &n)
		if ext == "" {
			ext = "(none)"
		}
		fmt.Printf("  %-14s %4d\n", ext, n)
	}
	extRows.Close()
	return nil
}

// cmdRelated is a thin wrapper so the command exists in the CLI surface even
// if the underlying lookup is unavailable.
func cmdRelated(args []string, dbPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("related needs a filename, e.g. mem related OS_notes.pdf\n\n%s", "run 'mem help related' for details")
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.related(args[0])
}

// cmdOrganize is defined in organizer.go.

// expandHome turns "~" into the home directory and cleans the path.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return filepath.Clean(p)
}

// hasFlag reports whether a bare flag is present.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue returns the value following a flag, if present.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// prettyPath shortens the home directory back to ~ so output stays readable.
func prettyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// humanBytes renders a byte count the way a person would say it.
func humanBytes(n uint64) string {
	units := []string{"B", "KB", "MB", "GB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// confirm asks a yes/no question on stdin.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
