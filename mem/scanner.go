package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxIndexableSize is the ceiling for files we are willing to hash and read.
// A 72MB scanned textbook tells us nothing a filename does not, and hashing
// every byte of it would make a demo scan feel broken.
const maxIndexableSize = 64 << 20 // 64 MiB

// maxFiles guards against walking into something enormous by accident
// (a home directory with a mounted network share, a node_modules tree, ...).
const maxFiles = 200000

// FileInfo is one discovered file, before any content extraction happens.
type FileInfo struct {
	Path       string
	Filename   string
	Ext        string // lowercased, includes the dot: ".pdf"
	Size       int64
	ModifiedAt int64 // unix seconds
}

// ScanStats reports what a scan actually did, so the CLI can be honest with
// the user about coverage instead of just claiming success.
type ScanStats struct {
	Seen         int
	Indexed      int
	TextReadable int // files we expect to extract text from
	SkippedLarge int
	SkippedMeta  int // hidden files, sockets, our own database
	Errs         []error
}

// shouldSkipDir keeps the walk out of places that are never worth indexing.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".cache", ".local", ".config", ".npm", ".Trash", ".obsidian":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// Scan walks root recursively and returns every regular file it finds,
// sorted so that scans are reproducible and easy to eyeball in a diff.
func Scan(root string) ([]FileInfo, ScanStats, error) {
	var stats ScanStats

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, stats, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, stats, err
	}
	if !info.IsDir() {
		return nil, stats, &os.PathError{Op: "scan", Path: root, Err: os.ErrInvalid}
	}

	var out []FileInfo
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable subfolder must not abort the whole scan.
			stats.Errs = append(stats.Errs, walkErr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		name := d.Name()

		if d.IsDir() {
			// Never descend into our own database directory, or into hidden
			// trees such as .git.
			if path != abs && shouldSkipDir(name) {
				return fs.SkipDir
			}
			return nil
		}

		// Symlinks can point back up the tree and send us into an infinite
		// loop, so they are ignored rather than followed.
		if !d.Type().IsRegular() {
			stats.SkippedMeta++
			return nil
		}
		if strings.HasPrefix(name, ".") {
			stats.SkippedMeta++
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			stats.Errs = append(stats.Errs, err)
			return nil
		}
		if fi.Size() > maxIndexableSize {
			stats.SkippedLarge++
			return nil
		}

		stats.Seen++
		if len(out) >= maxFiles {
			return fs.SkipAll
		}

		out = append(out, FileInfo{
			Path:       path,
			Filename:   name,
			Ext:        strings.ToLower(filepath.Ext(name)),
			Size:       fi.Size(),
			ModifiedAt: fi.ModTime().Unix(),
		})
		if isTextReadable(fi.Size(), strings.ToLower(filepath.Ext(name))) {
			stats.TextReadable++
		}
		return nil
	})
	if err != nil {
		return nil, stats, err
	}

	stats.Indexed = len(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, stats, nil
}

// ScanProgress is a snapshot of an in-flight scan, handed to a progress
// callback after each file so a UI can draw a bar without guessing.
type ScanProgress struct {
	Done  int    // files processed so far this pass
	Total int    // files the walk discovered
	Path  string // the file just finished, if any
}

// ScanReport is what a completed scan did, as data. The CLI formats it into
// its usual lines; the TUI renders the same numbers its own way.
type ScanReport struct {
	Dir          string
	Indexed      int
	Extracted    int
	Unchanged    int
	Pruned       []string
	SkippedLarge int
	SkippedMeta  int
	OCR          ocrStats
	Total        int
	DBPath       string
}

// ScanAndIndex walks dir, extracts text from changed files, and writes them to
// the index. progress, when non-nil, is called once up front and then after
// every file; it must be cheap and must not touch the store.
//
// This is the whole of what `mem scan` does, lifted out of cmdScan so that the
// TUI can run the exact same pipeline with a progress bar instead of a
// transcript. The indexing rules are unchanged: extraction failures are never
// fatal, unchanged files are skipped unless force is set, and rows whose files
// have vanished are pruned.
func (s *Store) ScanAndIndex(dir string, force bool, progress func(ScanProgress)) (ScanReport, error) {
	rep := ScanReport{Dir: dir, DBPath: s.Path()}

	files, stats, err := Scan(dir)
	if err != nil {
		return rep, err
	}
	rep.SkippedLarge = stats.SkippedLarge
	rep.SkippedMeta = stats.SkippedMeta

	// Every path the walk produced, including the ones we are about to skip as
	// unchanged. PruneMissing compares against this set to spot rows whose
	// files have since been deleted.
	seen := make(map[string]bool, len(files))
	for _, fi := range files {
		seen[fi.Path] = true
	}

	total := len(files)
	if progress != nil {
		progress(ScanProgress{Done: 0, Total: total})
	}

	resetOCRStats()

	for i, fi := range files {
		// Skip work entirely when size and mtime match what we stored. This
		// is what makes a second scan feel instant.
		if !force {
			if prev, found, err := s.Lookup(fi.Path); err == nil && found {
				if prev.Size == fi.Size && prev.Modified == fi.ModifiedAt {
					rep.Unchanged++
					if progress != nil {
						progress(ScanProgress{Done: i + 1, Total: total, Path: fi.Path})
					}
					continue
				}
			}
		}

		text, kind, xerr := Extract(fi.Path, fi.Ext)
		if xerr != nil {
			// Extraction problems are never fatal: the file still gets indexed
			// by name, size and timestamp, so it stays findable.
			text, kind = "", "meta"
		}
		if strings.TrimSpace(text) == "" {
			// Extracted nothing usable: record it as metadata-only.
			text, kind = "", "meta"
		}
		if kind != "meta" {
			rep.Extracted++
		}

		cat := categoryFor(fi.Ext, fi.Filename)
		if err := s.Upsert(fi, text, kind, cat, hashFile(fi.Path)); err != nil {
			return rep, fmt.Errorf("index %s: %w", fi.Path, err)
		}
		rep.Indexed++
		if progress != nil {
			progress(ScanProgress{Done: i + 1, Total: total, Path: fi.Path})
		}
	}

	// Drop rows for files that have been deleted since the last scan. Scoped to
	// the folder just walked, and only reached because Scan returned without
	// error — a walk that could not read the tree tells us nothing about what is
	// missing from it, so pruning on a partial walk would delete live rows.
	absRoot, err := filepath.Abs(dir)
	if err != nil {
		return rep, err
	}
	pruned, err := s.PruneMissing(absRoot, seen)
	if err != nil {
		return rep, fmt.Errorf("prune missing files: %w", err)
	}
	rep.Pruned = pruned
	rep.OCR = ocrCounts

	total2, _ := s.Count()
	rep.Total = total2
	return rep, nil
}
