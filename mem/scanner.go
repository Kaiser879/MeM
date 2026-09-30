package main

import (
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
