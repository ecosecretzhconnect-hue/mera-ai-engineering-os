package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// solutionModuleMap walks the repo root for .sln / .slnx files and returns a map of
//
//	lower-case-segment -> []slash-normalised-relative-project-dir
//
// Example (EcoSecretz.HConnect solution):
//
//	"identity"  -> ["Identity/Src/EcoSecretz.HConnect.Identity.API",
//	                "Identity/Src/EcoSecretz.HConnect.Identity.Logic", ...]
//	"ecosecretz"-> [same dirs]
//
// Returns an empty map when no solution files are found.
func solutionModuleMap() map[string][]string {
	r := root()
	result := map[string][]string{}

	_ = filepath.WalkDir(r, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(filepath.Base(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		lower := strings.ToLower(d.Name())
		switch {
		case strings.HasSuffix(lower, ".sln"):
			parseSln(r, p, result)
		case strings.HasSuffix(lower, ".slnx"):
			parseSlnx(r, p, result)
		}
		return nil
	})
	return result
}

// slnProjectRe matches lines like:
//
//	Project("{FAE04EC0-...}") = "MyProject", "Relative\Path\MyProject.csproj", "{...}"
var slnProjectRe = regexp.MustCompile(
	`(?i)^Project\("[^"]*"\)\s*=\s*"([^"]+)"\s*,\s*"([^"]+\.csproj)"`)

// parseSln extracts project directories from a .sln file and merges them into result.
func parseSln(repoRoot, slnPath string, result map[string][]string) {
	data, err := os.ReadFile(slnPath)
	if err != nil {
		return
	}
	slnDir := filepath.Dir(slnPath)

	for _, line := range strings.Split(string(data), "\n") {
		m := slnProjectRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		projectName := m[1] // e.g. "EcoSecretz.HConnect.Identity.API"
		csprojPath := filepath.Join(slnDir, filepath.FromSlash(strings.ReplaceAll(m[2], "\\", "/")))
		projectDir := filepath.Dir(csprojPath)

		relDir, err := filepath.Rel(repoRoot, projectDir)
		if err != nil {
			continue
		}
		relSlash := filepath.ToSlash(relDir)
		indexProjectName(projectName, relSlash, result)
	}
}

// slnxProjectRe matches Path="..." attributes in .slnx XML that point to .csproj files.
var slnxProjectRe = regexp.MustCompile(`(?i)Path\s*=\s*"([^"]+\.csproj)"`)

// parseSlnx extracts project directories from a .slnx XML file.
func parseSlnx(repoRoot, slnxPath string, result map[string][]string) {
	data, err := os.ReadFile(slnxPath)
	if err != nil {
		return
	}
	slnxDir := filepath.Dir(slnxPath)

	for _, line := range strings.Split(string(data), "\n") {
		m := slnxProjectRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		csprojPath := filepath.Join(slnxDir, filepath.FromSlash(strings.ReplaceAll(m[1], "\\", "/")))
		projectDir := filepath.Dir(csprojPath)

		relDir, err := filepath.Rel(repoRoot, projectDir)
		if err != nil {
			continue
		}
		relSlash := filepath.ToSlash(relDir)

		// Derive project name from the .csproj filename.
		projectName := strings.TrimSuffix(filepath.Base(m[1]), ".csproj")
		indexProjectName(projectName, relSlash, result)
	}
}

// indexProjectName indexes a project directory under every meaningful lower-case segment
// of the project name separated by ".".
//
// "EcoSecretz.HConnect.Identity.API" → keys: "ecosecretz.hconnect.identity.api",
//
//	"ecosecretz", "hconnect", "identity", "api"
func indexProjectName(projectName, relDir string, result map[string][]string) {
	nameLower := strings.ToLower(projectName)
	slnMapAdd(result, nameLower, relDir)

	for _, seg := range strings.Split(nameLower, ".") {
		seg = strings.TrimSpace(seg)
		if len(seg) >= 3 { // skip very short tokens like "v1", "ui"
			slnMapAdd(result, seg, relDir)
		}
	}
}

// slnMapAdd appends relDir to result[key] without duplicates.
func slnMapAdd(result map[string][]string, key, relDir string) {
	for _, existing := range result[key] {
		if existing == relDir {
			return
		}
	}
	result[key] = append(result[key], relDir)
}
