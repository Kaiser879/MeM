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
	case "ui":
		return cmdUI(rest, dbPath)
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

	rep, err := store.ScanAndIndex(dir, force, nil)
	if err != nil {
		return err
	}

	fmt.Printf("Indexed %d files\n", rep.Indexed)
	fmt.Printf("Extracted text from %d files\n", rep.Extracted)
	if rep.OCR.used > 0 {
		// Report OCR coverage precisely, and keep "found no words" separate
		// from "found words that were not prose". They are different problems
		// with opposite responses.
		fmt.Printf("OCR'd %d images: %d readable, %d with no text, %d unreadable noise\n",
			rep.OCR.used, rep.OCR.readable, rep.OCR.nothing, rep.OCR.noise)
	}
	if !ocrEnabled() {
		fmt.Println("OCR not installed: images are indexed by name only")
	}
	fmt.Printf("Skipped %d unchanged files\n", rep.Unchanged)
	if len(rep.Pruned) > 0 {
		// Removal is reported, never silent. Silently dropping index rows would
		// make search results change with no explanation for it.
		fmt.Printf("Removed %d file(s) that are no longer on disk\n", len(rep.Pruned))
		for i, p := range rep.Pruned {
			if i == 3 {
				fmt.Printf("  ... and %d more\n", len(rep.Pruned)-3)
				break
			}
			fmt.Printf("  %s\n", prettyPath(p))
		}
	}
	if rep.SkippedLarge > 0 {
		fmt.Printf("Skipped %d files over %d MB\n", rep.SkippedLarge, maxIndexableSize>>20)
	}
	if rep.SkippedMeta > 0 {
		fmt.Printf("Skipped %d hidden or non-regular files\n", rep.SkippedMeta)
	}
	fmt.Printf("\nMemory index updated. %d files known in total.\n", rep.Total)
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

	st, err := store.Stats()
	if err != nil {
		return err
	}
	if st.Total == 0 {
		fmt.Println("The index is empty. Try:  mem scan ~/Downloads")
		return nil
	}

	fmt.Printf("Memories indexed: %d\n", st.Total)
	fmt.Printf("Total size:       %s\n", humanBytes(uint64(st.TotalBytes)))
	fmt.Printf("Index file:       %s\n\n", st.Path)

	fmt.Println("By category:")
	for _, c := range st.Categories {
		fmt.Printf("  %-14s %4d  %s\n", c.Name, c.Count, humanBytes(uint64(c.Bytes)))
	}

	fmt.Println("\nBy file type:")
	for _, e := range st.Extensions {
		fmt.Printf("  %-14s %4d\n", e.Ext, e.Count)
	}
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

	data, err := store.Related(args[0])
	if err != nil {
		return err
	}

	target := data.Target
	fmt.Printf("Memories related to %s\n\n", target.Filename)
	fmt.Printf("  %s\n  category: %s · %s\n\n", prettyPath(target.Path),
		target.Category, humanBytes(uint64(target.Size)))

	if len(data.Terms) == 0 {
		fmt.Println("  (this file has no extracted text to compare on)")
		return nil
	}
	for _, r := range data.Hits {
		fmt.Printf("  %-44s %s · %s\n", truncate(r.Filename, 44),
			humanBytes(uint64(r.Size)), timeAgo(r.Modified))
	}
	if len(data.Hits) == 0 {
		fmt.Println("  (nothing else in the index shares vocabulary with this file)")
	}
	return nil
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
