package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// planItem is one file the organizer intends to place, remembered by the
// exact path the scanner found. Keeping the real path (rather than
// reconstructing it from the category and filename) is what lets the
// organizer work on nested folders without guessing.
type planItem struct {
	srcPath string
	name    string
}

// organizePlan maps a destination folder to the files that would go in it.
// It is only ever a description: nothing here has touched the filesystem.
type organizePlan map[string][]planItem

// organizeGroup is one destination folder's worth of the plan, in the order it
// should be shown. A slice rather than the map makes presentation stable
// between runs.
type organizeGroup struct {
	Category string
	Items    []planItem
}

// organizePreview is a full, ordered tidy-up proposal. Building it touches
// nothing on the filesystem; only Apply acts, and only when the caller asks.
type organizePreview struct {
	Dir    string
	Count  int
	Groups []organizeGroup
	plan   organizePlan
}

// OrganizePlan walks dir and proposes where each file would go. It performs no
// writes at all — the safety model keeps the decision and the action in
// separate functions on purpose.
func OrganizePlan(dir string) (organizePreview, error) {
	files, stats, err := Scan(dir)
	if err != nil {
		return organizePreview{}, err
	}
	plan := organizePlan{}
	for _, fi := range files {
		cat := categorizeFor(fi.Ext, fi.Filename)
		plan[cat] = append(plan[cat], planItem{srcPath: fi.Path, name: fi.Filename})
	}
	prev := organizePreview{Dir: dir, Count: stats.Indexed, plan: plan}
	for _, cat := range sortedKeys(plan) {
		prev.Groups = append(prev.Groups, organizeGroup{Category: cat, Items: plan[cat]})
	}
	return prev, nil
}

// Apply copies the planned files into destRoot. It never moves, deletes or
// overwrites; collisions get a numeric suffix. See applyPlan.
func (p organizePreview) Apply(destRoot string) (copied, skipped int, err error) {
	return applyPlan(p.plan, destRoot)
}

// categorizeFor decides the destination folder name for a file.
func categorizeFor(ext, filename string) string {
	// OCR-able raster images, plus the two image formats OCR cannot read.
	// Sharing one definition with the extractor keeps the two from drifting.
	if imageExtensions[ext] || ext == ".svg" || ext == ".heic" {
		return "Images"
	}

	// Same reasoning, same shared set. This used to be a hand-typed list of
	// 17 extensions, which is precisely how source files ended up filed as
	// code while the extractor refused to read them.
	if codeExtensions[ext] {
		return "Programming"
	}

	switch ext {
	case ".pdf", ".doc", ".docx", ".odt", ".rtf", ".txt", ".md", ".markdown",
		".ppt", ".pptx", ".odp", ".epub", ".pages":
		// Documents get a second-level topic split, so a folder of PDFs is
		// not just "more PDFs".
		return "Documents/" + documentCategory(filename)
	case ".mp4", ".mkv", ".webm", ".mov", ".avi", ".flv", ".m4v":
		return "Videos"
	case ".mp3", ".wav", ".flac", ".m4a", ".ogg", ".aac":
		return "Audio"
	case ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar", ".tgz":
		return "Archives"
	case ".deb", ".rpm", ".appimage", ".exe", ".msi", ".dmg", ".pkg":
		return "Software"
	}
	return "Other"
}

// cmdOrganize previews (and optionally applies) a tidying plan.
//
// Safety model, in order of importance:
//  1. Without --apply it is a dry run and touches nothing.
//  2. With --apply it COPIES files; it never deletes and never overwrites.
//  3. Even when applying, it asks for confirmation unless --yes is given.
func cmdOrganize(args []string, dbPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("organize needs a directory, e.g. mem organize ~/Downloads\n\n%s", "run 'mem help organize' for details")
	}
	dir := expandHome(args[0])
	apply := hasFlag(args, "--apply")
	autoYes := hasFlag(args, "--yes")
	destRoot := dir
	if into, ok := flagValue(args, "--into"); ok {
		destRoot = expandHome(into)
	}

	prev, err := OrganizePlan(dir)
	if err != nil {
		return err
	}
	if prev.Count == 0 {
		fmt.Printf("Nothing to organize in %s.\n", prettyPath(dir))
		return nil
	}

	fmt.Printf("Organization plan for %s\n", prettyPath(prev.Dir))
	fmt.Printf("(%d files considered)\n\n", prev.Count)

	for _, g := range prev.Groups {
		fmt.Printf("%-28s %d file(s)\n", g.Category+"/", len(g.Items))
		for _, it := range g.Items {
			fmt.Printf("    %s\n", it.name)
		}
		fmt.Println()
	}

	if !apply {
		fmt.Println("This was a preview. Nothing was changed.")
		fmt.Println("Re-run with --apply to copy files into the folder structure.")
		fmt.Println("(Files are copied, never moved or deleted.)")
		return nil
	}

	fmt.Printf("About to copy %d files into %s\n", prev.Count, prettyPath(destRoot))
	if !autoYes && !confirm("Proceed?") {
		fmt.Println("Cancelled. Nothing was changed.")
		return nil
	}

	copied, skipped, err := prev.Apply(destRoot)
	if err != nil {
		return err
	}
	fmt.Printf("\nCopied %d files into new folders.\n", copied)
	if skipped > 0 {
		fmt.Printf("Skipped %d (already present, or unreadable).\n", skipped)
	}
	fmt.Println("Your original files were never moved or deleted.")
	return nil
}

// applyPlan creates the destination folders and copies files into them.
//
// It never overwrites: if a target name is taken, a numeric suffix is added.
// That makes running the organizer twice harmless.
func applyPlan(plan organizePlan, destRoot string) (copied, skipped int, err error) {
	for _, cat := range sortedKeys(plan) {
		destDir := filepath.Join(destRoot, filepath.FromSlash(cat))
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return copied, skipped, fmt.Errorf("create %s: %w", destDir, err)
		}

		// Track names used in this folder so two "IMG_1.png" files do not
		// silently collapse into one.
		used := map[string]bool{}

		for _, item := range plan[cat] {
			dst := filepath.Join(destDir, item.name)
			if used[dst] {
				base := strings.TrimSuffix(item.name, filepath.Ext(item.name))
				ext := filepath.Ext(item.name)
				for i := 2; used[dst]; i++ {
					dst = filepath.Join(destDir, fmt.Sprintf("%s-%d%s", base, i, ext))
				}
			}

			if err := copyFile(item.srcPath, dst); err != nil {
				skipped++
				continue
			}
			used[dst] = true
			copied++
		}
	}
	return copied, skipped, nil
}

// copyFile copies src to dst. It never overwrites and never removes src.
//
// On a mid-copy failure the partial destination is removed, so a failed copy
// never leaves a truncated file that looks like a real one.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// O_EXCL guarantees we refuse to clobber an existing file.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}

	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	// Sync before reporting success, so the copy survives a crash.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
