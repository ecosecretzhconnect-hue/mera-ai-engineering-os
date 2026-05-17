package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func root() string       { wd, _ := os.Getwd(); return wd }
func meraDir() string    { return filepath.Join(root(), ".mera") }
func reportsDir() string  { return filepath.Join(meraDir(), "reports") }
func snapshotsDir() string { return filepath.Join(meraDir(), "snapshots") }
func sessionsDir() string  { return filepath.Join(meraDir(), "sessions") }

func exists(p string) bool { _, e := os.Stat(p); return e == nil }

func writeNoBOM(path string, b []byte) error { return os.WriteFile(path, b, 0644) }

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func capture(n string, args ...string) string {
	cmd := exec.Command(n, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	return out.String()
}

// stdinReader is a single package-level buffered reader for os.Stdin.
// Using one shared reader is critical when stdin is piped from a file (e.g. automated tests):
// each call to bufio.NewReader(os.Stdin) pre-buffers the entire pipe content into its
// internal buffer, then discards it when the local variable is GC'd — so only the first
// promptLine call would ever see data. A shared reader retains the buffer across calls.
var stdinReader = bufio.NewReader(os.Stdin)

func promptLine(label string) string {
	fmt.Print(label + ": ")
	s, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(s)
}

// sampleFile reads the first maxLines lines from a file.
func sampleFile(path string, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() && len(lines) < maxLines {
		lines = append(lines, sc.Text())
	}
	return strings.Join(lines, "\n")
}

// parseJSONStringArray extracts a JSON string array from text that may have surrounding prose.
func parseJSONStringArray(s string) []string {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start == -1 || end == -1 || end <= start {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(s[start:end+1]), &result); err != nil {
		return nil
	}
	return result
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func skipDir(name string) bool {
	switch name {
	// VCS / IDE
	case ".git", ".vs", ".idea",
		// MERA internal
		".mera",
		// AI tooling — worktrees live here, never scan them
		".claude",
		// JS toolchain
		"node_modules", ".next",
		// Build outputs
		"bin", "obj", "dist", "build",
		// Test / CI outputs
		"coverage", "TestResults",
		// Artifact staging
		"artifacts", "packages",
		// Temp / logs
		"logs", "tmp", "temp",
		// Python
		"__pycache__":
		return true
	}
	return false
}

// isExcludedPath returns true if the slash-normalized relative path contains any
// directory segment that must never appear in File Scout results.
//
// This is a defense-in-depth check applied at EVERY layer:
//   - repo walker (belt-and-suspenders alongside skipDir)
//   - cache loading (re-filters stale cached entries)
//   - confidence scorer (hard-zeros excluded files)
//   - fileScoutAgent final sanitizer (last resort before evidence is returned)
//
// It operates on the FULL path, not just the base name, so it catches cases
// where skipDir may miss (junction points, symlinks, stale cache entries,
// paths injected by Ollama output, etc.).
func isExcludedPath(relPath string) bool {
	// Normalise once: lowercase + forward slashes
	p := strings.ToLower(filepath.ToSlash(relPath))

	// Fast explicit check for the most critical case — .claude worktrees.
	// Checked as prefix, substring, and exact match to handle all nesting.
	if strings.HasPrefix(p, ".claude/") || p == ".claude" ||
		strings.Contains(p, "/.claude/") || strings.HasSuffix(p, "/.claude") {
		return true
	}

	// Segment-level check: every component of the path is tested.
	// This catches .claude (and all other excluded dirs) at any nesting depth,
	// regardless of whether skipDir fired during the original walk.
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		switch seg {
		case ".claude", ".git", ".mera", ".vs", ".idea",
			"node_modules", ".next",
			"bin", "obj", "dist", "build",
			"coverage", "testresults",
			"artifacts", "packages",
			"logs", "tmp", "temp",
			"__pycache__":
			return true
		}
	}
	return false
}

// skipFile returns true for generated, binary, or lock files that should never
// appear in File Scout results even if they pass the extension filter.
func skipFile(name string) bool {
	lower := strings.ToLower(name)
	// Minified / source-map web assets
	if strings.HasSuffix(lower, ".min.js") || strings.HasSuffix(lower, ".min.css") ||
		strings.HasSuffix(lower, ".map") {
		return true
	}
	// Windows binaries / debug symbols
	ext := filepath.Ext(lower)
	switch ext {
	case ".dll", ".exe", ".pdb", ".cache", ".lock", ".user", ".suo":
		return true
	}
	return false
}

// classifyFile labels a repo path as "backend", "frontend", or "test".
// Used by the confidence engine to apply domain-mismatch penalties.
func classifyFile(relPath string) string {
	l := strings.ToLower(filepath.ToSlash(relPath))
	name := strings.ToLower(filepath.Base(relPath))
	ext := filepath.Ext(name)

	// Test detection — check before frontend so test.tsx is still "test"
	if strings.Contains(l, "/test/") || strings.Contains(l, "/tests/") ||
		strings.HasSuffix(name, ".test.cs") || strings.HasSuffix(name, ".spec.ts") ||
		strings.HasSuffix(name, ".test.ts") || strings.HasSuffix(name, ".test.tsx") ||
		strings.HasSuffix(name, "_test.go") ||
		strings.Contains(name, "test") && (ext == ".cs" || ext == ".go") ||
		strings.Contains(l, "testresults/") {
		return "test"
	}

	// Frontend detection
	if ext == ".tsx" || ext == ".jsx" {
		return "frontend"
	}
	if ext == ".ts" || ext == ".js" {
		if strings.Contains(l, "/components/") || strings.Contains(l, "/pages/") ||
			strings.Contains(l, "/features/") || strings.Contains(l, "/hooks/") ||
			strings.Contains(l, "/stores/") || strings.Contains(l, "/contexts/") ||
			strings.Contains(l, "/styles/") || strings.Contains(l, ".web/") ||
			strings.Contains(l, "frontend/") || strings.Contains(l, "/ui/") {
			return "frontend"
		}
	}
	return "backend"
}

// inferTaskDomain returns "frontend" if the task text contains UI/frontend cues,
// "backend" otherwise. Used to penalise frontend files in backend-focused tasks.
func inferTaskDomain(task string) string {
	l := strings.ToLower(task)
	frontendCues := []string{
		"frontend", "ui", "login page", "react", "next.js", "nextjs",
		"component", "hook", "zustand", "css", "button", "form",
		"modal", "redirect page", "page component", "service worker",
	}
	for _, cue := range frontendCues {
		if strings.Contains(l, cue) {
			return "frontend"
		}
	}
	return "backend"
}

func isCodeFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".cs", ".go", ".ts", ".tsx", ".js", ".jsx", ".py",
		".java", ".rs", ".rb", ".php", ".swift", ".kt",
		".json", ".yaml", ".yml", ".toml", ".xml",
		".csproj", ".sln", ".slnx":
		return true
	}
	return false
}
