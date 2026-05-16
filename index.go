package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// repoIndexVersion must be incremented whenever exclusion rules change.
// A cached index whose version does not match is discarded and rebuilt.
// Current: 3 — added isExcludedPath full-path exclusion (Phase 10.4.1)
const repoIndexVersion = 3

type RepoIndex struct {
	Version   int       `json:"version"`   // schema/exclusion-rules version
	CreatedAt time.Time `json:"createdAt"`
	Root      string    `json:"root"`
	Files     []string  `json:"files"` // relative slash paths, already exclusion-filtered
}

func repoIndexPath() string {
	return filepath.Join(meraDir(), "repo-index.json")
}

// loadCachedIndex returns cached relative file paths if they are fresh enough AND
// match the current index version and root.
// It re-applies isExcludedPath to every cached entry as a defense-in-depth filter:
// if a path slipped into a previous index build it will not be served from cache.
func loadCachedIndex(maxAge time.Duration) []string {
	b, err := os.ReadFile(repoIndexPath())
	if err != nil {
		return nil
	}
	var idx RepoIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil
	}
	if idx.Version != repoIndexVersion {
		return nil // exclusion rules changed — force fresh walk
	}
	if idx.Root != root() {
		return nil // different working directory
	}
	if time.Since(idx.CreatedAt) > maxAge {
		return nil // stale
	}

	// Re-apply exclusion rules to every cached entry.
	// A pre-fix index may contain paths that should now be excluded.
	var clean []string
	for _, rel := range idx.Files {
		if !isExcludedPath(rel) {
			clean = append(clean, rel)
		}
	}
	return clean
}

// saveRepoIndex writes the current file list to the cache with the current version.
func saveRepoIndex(files []candidateFile) {
	var relPaths []string
	for _, f := range files {
		relPaths = append(relPaths, f.relPath)
	}
	idx := RepoIndex{
		Version:   repoIndexVersion,
		CreatedAt: time.Now(),
		Root:      root(),
		Files:     relPaths,
	}
	b, _ := json.MarshalIndent(idx, "", "  ")
	_ = writeNoBOM(repoIndexPath(), b)
}

// allRepoFiles returns all code files, using a 5-minute cache to avoid re-walking large repos.
// The cache is keyed to repo root + exclusion-rules version.
// Delete .mera/repo-index.json to force a fresh walk immediately.
func allRepoFiles() []candidateFile {
	const maxAge = 5 * time.Minute

	if cached := loadCachedIndex(maxAge); cached != nil {
		r := root()
		var out []candidateFile
		for _, rel := range cached {
			// isExcludedPath already applied in loadCachedIndex; exists() is the only
			// remaining check needed here.
			abs := filepath.Join(r, filepath.FromSlash(rel))
			if exists(abs) {
				out = append(out, candidateFile{absPath: abs, relPath: rel})
			}
		}
		return out
	}

	// Cache miss: walk the repo.
	r := root()
	var files []candidateFile
	_ = filepath.WalkDir(r, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if d.IsDir() {
			baseName := filepath.Base(p)
			// Primary dir-level skip (fast path for most repos).
			if skipDir(baseName) {
				return filepath.SkipDir
			}
			// Belt-and-suspenders: also check the relative path so far.
			// This catches cases where skipDir fires on base but the relative
			// path segment contains an excluded dir at a deeper level.
			rel, _ := filepath.Rel(r, p)
			if isExcludedPath(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !isCodeFile(name) || skipFile(name) {
			return nil
		}
		rel, _ := filepath.Rel(r, p)
		relSlash := filepath.ToSlash(rel)
		// Final belt-and-suspenders path check before accepting the file.
		if isExcludedPath(relSlash) {
			return nil
		}
		files = append(files, candidateFile{
			absPath: p,
			relPath: relSlash,
		})
		return nil
	})

	saveRepoIndex(files)
	return files
}
