package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PatchAnalysis is the complete result of the post-Aider patch safety check.
type PatchAnalysis struct {
	ApprovedFiles     []string    `json:"approvedFiles"`
	ChangedFiles      []string    `json:"changedFiles"`
	UnauthorizedFiles []string    `json:"unauthorizedFiles"`
	FilePatches       []FilePatch `json:"filePatches"`
	IntentScore       int         `json:"intentScore"`
	IntentReason      string      `json:"intentReason"`
	OverallVerdict    string      `json:"overallVerdict"` // SAFE / WARN / BLOCK
	OverrideReason    string      `json:"overrideReason,omitempty"`
}

// FilePatch is the per-file change analysis result.
type FilePatch struct {
	RelPath    string   `json:"relPath"`
	Category   string   `json:"category"`   // SAFE / WARN / BLOCK
	ChangeType string   `json:"changeType"` // human label
	Reasons    []string `json:"reasons"`
	LinesAdded int      `json:"linesAdded"`
}

// analyzeDiff runs the full patch safety analysis after Aider completes.
//
//   - approvedFiles: absolute paths the user approved before Aider launched.
//     If nil (e.g. called from ExplainDiff), the unauthorised-file check is skipped.
func analyzeDiff(target, task string, approvedFiles []string) PatchAnalysis {
	analysis := PatchAnalysis{ApprovedFiles: approvedFiles}

	changed := changedFiles()
	analysis.ChangedFiles = changed

	// Build approved set (relative, slash-normalised).
	approvedSet := map[string]bool{}
	for _, f := range approvedFiles {
		rel, _ := filepath.Rel(root(), f)
		approvedSet[filepath.ToSlash(rel)] = true
	}

	// Identify unauthorised changes (only when an approved list exists).
	if len(approvedFiles) > 0 {
		for _, f := range changed {
			if !approvedSet[filepath.ToSlash(f)] {
				analysis.UnauthorizedFiles = append(analysis.UnauthorizedFiles, f)
			}
		}
	}

	// Analyse every changed file.
	fullDiff := strings.TrimSpace(capture("git", "diff", "HEAD"))
	if fullDiff == "" {
		fullDiff = strings.TrimSpace(capture("git", "diff"))
	}
	for _, f := range changed {
		analysis.FilePatches = append(analysis.FilePatches, analyzeFilePatch(f, fullDiff))
	}

	// Intent compliance score (non-streaming — we need to parse a number).
	analysis.IntentScore, analysis.IntentReason = computeIntentScore(task, fullDiff)

	// Derive overall verdict.
	analysis.OverallVerdict = computePatchVerdict(analysis)
	return analysis
}

// analyzeFilePatch categorises a single changed file.
func analyzeFilePatch(relPath, fullDiff string) FilePatch {
	fp := FilePatch{RelPath: relPath}
	fileDiff := extractFileDiff(relPath, fullDiff)
	added := diffAddedLines(fileDiff)
	fp.LinesAdded = strings.Count(added, "\n") + 1

	// Path-level category (fastest, most reliable signal).
	pathCat, pathReason := pathCategory(relPath)

	// Content-level analysis on added lines only (reduces false positives).
	contentCat := "SAFE"
	var contentReasons []string

	if detectRouteChanges(added) {
		contentReasons = append(contentReasons, "route / endpoint registration change detected")
		contentCat = mergeCategory(contentCat, "WARN")
	}
	if reason := detectPackageChanges(relPath, added); reason != "" {
		contentReasons = append(contentReasons, reason)
		contentCat = mergeCategory(contentCat, "BLOCK")
	}
	if detectDbChanges(relPath, added) {
		contentReasons = append(contentReasons, "database migration / schema change")
		contentCat = mergeCategory(contentCat, "BLOCK")
	}
	if reasons := detectSecurityChanges(added); len(reasons) > 0 {
		contentReasons = append(contentReasons, reasons...)
		contentCat = "BLOCK"
	}
	if detectAuthMiddleware(added) {
		contentReasons = append(contentReasons, "authentication / authorization middleware modified")
		contentCat = mergeCategory(contentCat, "BLOCK")
	}

	// Path-based BLOCK always wins.
	switch {
	case pathCat == "BLOCK":
		fp.Category = "BLOCK"
		fp.ChangeType = pathReason
		fp.Reasons = dedupe(append([]string{pathReason}, contentReasons...))
	case contentCat == "BLOCK":
		fp.Category = "BLOCK"
		fp.ChangeType = "dangerous content change"
		fp.Reasons = contentReasons
	case pathCat == "WARN" || contentCat == "WARN":
		fp.Category = "WARN"
		fp.ChangeType = firstNonEmpty(pathReason, "unexpected change")
		fp.Reasons = dedupe(append([]string{pathReason}, contentReasons...))
	default:
		fp.Category = "SAFE"
		fp.ChangeType = "standard code modification"
		fp.Reasons = contentReasons
		if len(fp.Reasons) == 0 {
			fp.Reasons = []string{"code change within expected scope"}
		}
	}
	return fp
}

// computeIntentScore asks Ollama to rate how well the diff matches the task (0–100).
func computeIntentScore(task, diff string) (int, string) {
	if strings.TrimSpace(diff) == "" {
		return 0, "No diff to analyse"
	}
	truncated := diff
	if len(truncated) > 3500 {
		truncated = truncated[:3500] + "\n...(diff truncated)"
	}
	prompt := fmt.Sprintf(`You are a code change intent analyser.

Original task: "%s"

Git diff (added lines):
%s

Does the diff ONLY implement what was requested, nothing more?
Reply in this EXACT format (two lines, no other text):
SCORE: <0-100>
REASON: <one sentence>

100 = perfectly on-scope, 0 = completely off-scope or dangerous.`, task, truncated)

	out, _, err := generateForRole(RoleDiffReview, prompt, false)
	if err != nil {
		return 50, "Could not compute intent score (Ollama unavailable)"
	}

	score, reason := 50, strings.TrimSpace(out)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "SCORE:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, line[:7]))
			if n, e := strconv.Atoi(strings.Fields(raw)[0]); e == nil {
				score = clamp(n, 0, 100)
			}
		}
		if strings.HasPrefix(upper, "REASON:") {
			reason = strings.TrimSpace(line[7:])
		}
	}
	return score, reason
}

// computePatchVerdict derives SAFE / WARN / BLOCK from a PatchAnalysis.
func computePatchVerdict(a PatchAnalysis) string {
	// Unauthorized files always block.
	if len(a.UnauthorizedFiles) > 0 {
		return "BLOCK"
	}
	// Any BLOCK file → BLOCK.
	for _, fp := range a.FilePatches {
		if fp.Category == "BLOCK" {
			return "BLOCK"
		}
	}
	// Intent score too low → BLOCK.
	if a.IntentScore < 40 {
		return "BLOCK"
	}
	// WARN conditions.
	for _, fp := range a.FilePatches {
		if fp.Category == "WARN" {
			return "WARN"
		}
	}
	if a.IntentScore < 70 {
		return "WARN"
	}
	return "SAFE"
}

// enforcePatchSafety handles the BLOCK / WARN / override workflow.
// Mutates analysis.OverrideReason when the user provides an override.
func enforcePatchSafety(analysis *PatchAnalysis) error {
	printPatchReport(*analysis)

	switch analysis.OverallVerdict {
	case "SAFE":
		fmt.Println("[OK]  Patch safety gate passed.")
		return nil

	case "WARN":
		fmt.Println("\n[WARN] Patch has warnings — review the report above.")
		answer := strings.ToUpper(strings.TrimSpace(
			promptLine("Type PROCEED to continue to validation, or Enter to rollback")))
		if answer != "PROCEED" {
			return errors.New("cancelled at patch safety WARN — run mera -Rollback to undo changes")
		}
		analysis.OverrideReason = "User acknowledged WARN and typed PROCEED"
		fmt.Println("[OK]  Proceeding with patch warnings acknowledged.")
		return nil

	case "BLOCK":
		fmt.Println("\n[FAIL] Patch safety gate BLOCKED. This is a hard stop.")
		fmt.Println("[MERA] To undo all changes: mera -Rollback")
		raw := promptLine("Type OVERRIDE <reason> to force through, or Enter to accept rollback recommendation")
		upper := strings.ToUpper(strings.TrimSpace(raw))
		if strings.HasPrefix(upper, "OVERRIDE") {
			reason := strings.TrimSpace(raw[8:])
			if reason == "" {
				reason = "user override — no reason given"
			}
			analysis.OverrideReason = reason
			fmt.Println("[WARN] Override accepted. Human takes full responsibility for this change.")
			return nil
		}
		return errors.New("patch safety gate BLOCKED — run mera -Rollback to undo all changes")

	default:
		return nil
	}
}

// printPatchReport prints a human-readable patch safety summary.
func printPatchReport(a PatchAnalysis) {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Patch Safety Report")
	fmt.Println("[MERA] ========================================")

	if len(a.ApprovedFiles) > 0 {
		fmt.Println("\n Approved files:")
		for _, f := range a.ApprovedFiles {
			rel, _ := filepath.Rel(root(), f)
			fmt.Println("   ->", filepath.ToSlash(rel))
		}
	}

	if len(a.UnauthorizedFiles) > 0 {
		fmt.Println("\n [BLOCK] UNAUTHORISED changes (not in approved list):")
		for _, f := range a.UnauthorizedFiles {
			fmt.Println("   !", f)
		}
	}

	fmt.Println("\n Changed files:")
	for _, fp := range a.FilePatches {
		icon := categoryIcon(fp.Category)
		fmt.Printf("   %s %-50s [%s]\n", icon, fp.RelPath, fp.Category)
		for _, r := range fp.Reasons {
			if r != "" {
				fmt.Printf("       • %s\n", r)
			}
		}
	}

	fmt.Printf("\n Intent score: %d%%\n", a.IntentScore)
	fmt.Printf(" Intent reason: %s\n", a.IntentReason)
	fmt.Printf("\n Overall verdict: %s\n", a.OverallVerdict)
	if a.OverrideReason != "" {
		fmt.Printf(" Override: %s\n", a.OverrideReason)
	}
	fmt.Println("\n[MERA] ========================================")
}

// ── Detectors ──────────────────────────────────────────────────────────────

// pathCategory returns a category and reason based on the file's path alone.
func pathCategory(relPath string) (cat, reason string) {
	l := strings.ToLower(filepath.ToSlash(relPath))
	base := strings.ToLower(filepath.Base(relPath))

	type rule struct{ pat, reason string }
	blockRules := []rule{
		{"appsettings", "application configuration file"},
		{"program.cs", "application entry point (Program.cs)"},
		{"startup.cs", "application startup configuration"},
		{"package-lock.json", "package lock file"},
		{"package.json", "package dependency manifest"},
		{".csproj", "project file (.csproj)"},
		{"migration", "database migration"},
		{"dockerfile", "container configuration"},
		{"docker-compose", "container orchestration"},
		{".terraform", "infrastructure as code (Terraform)"},
		{".tfvars", "Terraform variable file"},
		{"go.mod", "Go module file"},
		{"go.sum", "Go module checksum"},
		{"requirements.txt", "Python dependency file"},
		{"pipfile", "Python Pipfile"},
		{"secrets.", "secrets file"},
		{".env", "environment variable file"},
	}
	for _, r := range blockRules {
		if strings.Contains(l, r.pat) || strings.Contains(base, r.pat) {
			return "BLOCK", r.reason
		}
	}

	warnRules := []rule{
		{".yml", "YAML configuration file"},
		{".yaml", "YAML configuration file"},
		{".json", "JSON configuration file"},
		{".xml", "XML configuration file"},
		{"middleware", "middleware component"},
		{"filter", "request filter / interceptor"},
	}
	for _, r := range warnRules {
		if strings.HasSuffix(l, r.pat) || strings.Contains(l, r.pat) {
			// Narrow: only flag if it's actually an infra/config yaml
			if strings.Contains(l, ".yml") || strings.Contains(l, ".yaml") {
				if strings.Contains(l, "github") || strings.Contains(l, "pipeline") ||
					strings.Contains(l, "azure") || strings.Contains(l, "ci") || strings.Contains(l, "cd") {
					return "BLOCK", "CI/CD pipeline file"
				}
			}
			return "WARN", r.reason
		}
	}
	return "", ""
}

// detectRouteChanges looks for route / endpoint registration in added lines.
var routeRe = regexp.MustCompile(`(?i)(\[Route\(|\[HttpGet|\[HttpPost|\[HttpPut|\[HttpDelete|\[HttpPatch|MapGet\(|MapPost\(|MapPut\(|MapDelete\(|MapPatch\(|UseEndpoints|MapControllers\b|MapHub\()`)

func detectRouteChanges(addedLines string) bool {
	return routeRe.MatchString(addedLines)
}

// detectPackageChanges detects dependency changes in added lines or by path.
func detectPackageChanges(relPath, addedLines string) string {
	l := strings.ToLower(relPath)
	// NuGet
	if strings.Contains(addedLines, "<PackageReference") || strings.Contains(addedLines, "dotnet add package") {
		return "NuGet package reference change"
	}
	// npm (content)
	if strings.Contains(l, "package.json") && (strings.Contains(addedLines, `"dependencies"`) || strings.Contains(addedLines, `"devDependencies"`)) {
		return "npm dependency change"
	}
	// Go modules
	if (strings.Contains(l, "go.mod") || strings.Contains(l, "go.sum")) && strings.Contains(addedLines, "require") {
		return "Go module dependency change"
	}
	// Python
	if strings.Contains(l, "requirements") {
		return "Python dependency change"
	}
	return ""
}

// detectDbChanges detects database schema or migration changes.
var migrationContentRe = regexp.MustCompile(`(?i)(CREATE TABLE|DROP TABLE|ALTER TABLE|INSERT INTO|DELETE FROM|migrationBuilder\.|schema\.)`)

func detectDbChanges(relPath, addedLines string) bool {
	l := strings.ToLower(relPath)
	if strings.Contains(l, "migration") || strings.Contains(l, ".sql") || strings.Contains(l, "schema") {
		return true
	}
	return migrationContentRe.MatchString(addedLines)
}

// detectSecurityChanges looks for hardcoded secrets in added lines.
var secretRe = regexp.MustCompile(`(?i)(password\s*=\s*["'][^"']{4,}|jwt[_:]?key\s*=|connectionstring\s*=\s*["']|api[_-]?key\s*=\s*["'][^"']{4,}|-----BEGIN (RSA |EC )?PRIVATE KEY|secret\s*=\s*["'][^"']{4,}|bearer\s+[a-zA-Z0-9\-_]{20,})`)

func detectSecurityChanges(addedLines string) []string {
	var found []string
	if secretRe.MatchString(addedLines) {
		found = append(found, "potential hardcoded secret or credential detected")
	}
	return found
}

// detectAuthMiddleware looks for authentication/authorization middleware changes.
var authMiddlewareRe = regexp.MustCompile(`(?i)(UseAuthentication\(\)|UseAuthorization\(\)|AddJwtBearer|AddAuthentication\(|AddAuthorization\(|AuthenticationMiddleware|JwtSecurityToken|TokenValidationParameters)`)

func detectAuthMiddleware(addedLines string) bool {
	return authMiddlewareRe.MatchString(addedLines)
}

// ── Helpers ────────────────────────────────────────────────────────────────

// extractFileDiff returns the diff section for a specific file from the full diff output.
func extractFileDiff(relPath, fullDiff string) string {
	slashed := filepath.ToSlash(relPath)
	sections := strings.Split(fullDiff, "\ndiff --git ")
	for _, s := range sections {
		if strings.Contains(s, slashed) {
			return s
		}
	}
	return ""
}

// diffAddedLines extracts only the lines added by the diff (+lines, not +++ headers).
func diffAddedLines(diff string) string {
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			out = append(out, line[1:])
		}
	}
	return strings.Join(out, "\n")
}

func mergeCategory(current, next string) string {
	if current == "BLOCK" || next == "BLOCK" {
		return "BLOCK"
	}
	if current == "WARN" || next == "WARN" {
		return "WARN"
	}
	return "SAFE"
}

func categoryIcon(cat string) string {
	switch cat {
	case "BLOCK":
		return "[BLOCK]"
	case "WARN":
		return "[!] "
	default:
		return "OK  "
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
