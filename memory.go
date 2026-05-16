package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TaskOutcome records a single completed MERA session.
type TaskOutcome struct {
	ID               string   `json:"id"`
	Target           string   `json:"target"`
	Task             string   `json:"task"`
	Mode             string   `json:"mode"`
	Verdict          string   `json:"verdict"`          // APPROVE / NEEDS_REVIEW / REJECT / NO_DIFF
	ValidationPassed bool     `json:"validationPassed"`
	FilesTargeted    []string `json:"filesTargeted"`    // File Scout output
	FilesChanged     []string `json:"filesChanged"`     // actual git diff --name-only
	Confidence       int      `json:"confidence"`
	Timestamp        string   `json:"timestamp"`
}

// FileStats tracks per-file modification history.
type FileStats struct {
	TouchCount   int `json:"touchCount"`
	SuccessCount int `json:"successCount"`
	RejectCount  int `json:"rejectCount"`
}

// TargetStats tracks per-module history and learned file patterns.
type TargetStats struct {
	RunCount      int      `json:"runCount"`
	SuccessCount  int      `json:"successCount"`
	AvgConfidence float64  `json:"avgConfidence"`
	CommonFiles   []string `json:"commonFiles"` // files in ≥50% of APPROVE outcomes
}

// ProjectProfile is the persistent learning store for a project.
// Lives at .mera/profile.json and grows with every session.
type ProjectProfile struct {
	ProjectType string                 `json:"projectType"`
	CreatedAt   string                 `json:"createdAt"`
	UpdatedAt   string                 `json:"updatedAt"`
	Outcomes    []TaskOutcome          `json:"outcomes"`
	FileStats   map[string]FileStats   `json:"fileStats"`
	TargetStats map[string]TargetStats `json:"targetStats"`
}

func profilePath() string { return filepath.Join(meraDir(), "profile.json") }

const (
	maxProfileOutcomes = 200
	maxProfileBytes    = 512 * 1024 // 512 KB hard cap
)

func backupProfilePath() string { return profilePath() + ".bak" }

func loadProfile() ProjectProfile {
	empty := ProjectProfile{
		FileStats:   map[string]FileStats{},
		TargetStats: map[string]TargetStats{},
	}

	b, err := os.ReadFile(profilePath())
	if err != nil {
		return empty
	}

	var p ProjectProfile
	if jsonErr := json.Unmarshal(b, &p); jsonErr != nil {
		// Primary corrupted — attempt backup recovery.
		fmt.Println("[WARN] profile.json corrupted, attempting recovery from backup...")
		if bb, berr := os.ReadFile(backupProfilePath()); berr == nil {
			var pb ProjectProfile
			if json.Unmarshal(bb, &pb) == nil {
				fmt.Println("[OK]  Profile recovered from backup.")
				pb = normaliseProfile(pb)
				saveProfile(pb) // re-persist recovered copy
				return pb
			}
		}
		fmt.Println("[WARN] Could not recover profile — starting fresh.")
		return empty
	}

	return normaliseProfile(p)
}

func saveProfile(p ProjectProfile) {
	p = normaliseProfile(p)
	p.UpdatedAt = time.Now().Format(time.RFC3339)

	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}

	// Hard size cap — trim oldest outcomes until under limit.
	for len(b) > maxProfileBytes && len(p.Outcomes) > 10 {
		trim := len(p.Outcomes) / 4
		p.Outcomes = p.Outcomes[trim:]
		b, _ = json.MarshalIndent(p, "", "  ")
	}

	// Rotate backup before overwriting.
	if exists(profilePath()) {
		if current, rerr := os.ReadFile(profilePath()); rerr == nil {
			_ = os.WriteFile(backupProfilePath(), current, 0644)
		}
	}

	_ = writeNoBOM(profilePath(), b)
}

// normaliseProfile ensures nil maps are initialised and outcome count is capped.
func normaliseProfile(p ProjectProfile) ProjectProfile {
	if p.FileStats == nil {
		p.FileStats = map[string]FileStats{}
	}
	if p.TargetStats == nil {
		p.TargetStats = map[string]TargetStats{}
	}
	if len(p.Outcomes) > maxProfileOutcomes {
		p.Outcomes = p.Outcomes[len(p.Outcomes)-maxProfileOutcomes:]
	}
	return p
}

// recordOutcome writes a session result into the project profile and updates all derived stats.
func recordOutcome(target, task, mode string, filesTargeted, filesChanged []string, verdict string, confidence int, validationPassed bool) {
	profile := loadProfile()

	if profile.CreatedAt == "" {
		profile.CreatedAt = time.Now().Format(time.RFC3339)
		profile.ProjectType = detectProject().Type
	}

	outcome := TaskOutcome{
		ID:               time.Now().Format("20060102-150405"),
		Target:           target,
		Task:             task,
		Mode:             mode,
		Verdict:          verdict,
		ValidationPassed: validationPassed,
		FilesTargeted:    filesTargeted,
		FilesChanged:     filesChanged,
		Confidence:       confidence,
		Timestamp:        time.Now().Format(time.RFC3339),
	}
	profile.Outcomes = append(profile.Outcomes, outcome)

	// Cap history at 200 outcomes.
	if len(profile.Outcomes) > 200 {
		profile.Outcomes = profile.Outcomes[len(profile.Outcomes)-200:]
	}

	success := verdict == "APPROVE" && validationPassed

	// Update per-file stats.
	r := root()
	for _, f := range filesChanged {
		rel, _ := filepath.Rel(r, f)
		rel = filepath.ToSlash(rel)
		stat := profile.FileStats[rel]
		stat.TouchCount++
		if success {
			stat.SuccessCount++
		} else {
			stat.RejectCount++
		}
		profile.FileStats[rel] = stat
	}

	// Update per-target stats.
	ts := profile.TargetStats[target]
	ts.RunCount++
	if success {
		ts.SuccessCount++
	}
	// Rolling average confidence.
	if ts.RunCount == 1 {
		ts.AvgConfidence = float64(confidence)
	} else {
		ts.AvgConfidence = (ts.AvgConfidence*float64(ts.RunCount-1) + float64(confidence)) / float64(ts.RunCount)
	}
	ts.CommonFiles = computeCommonFiles(profile.Outcomes, target)
	profile.TargetStats[target] = ts

	saveProfile(profile)
}

// getFileHints returns files that appeared in ≥50% of successful sessions for this target.
// Used by File Scout to prime its Ollama prompt with historically accurate candidates.
func getFileHints(target string) []string {
	profile := loadProfile()
	ts, ok := profile.TargetStats[target]
	if !ok || len(ts.CommonFiles) == 0 {
		return nil
	}
	return ts.CommonFiles
}

// getPastFailureWarning returns a human-readable warning if a similar task previously failed.
func getPastFailureWarning(target, task string) string {
	profile := loadProfile()
	taskWords := significantWords(task)
	if len(taskWords) == 0 {
		return ""
	}
	for i := len(profile.Outcomes) - 1; i >= 0; i-- {
		o := profile.Outcomes[i]
		if o.Target != target {
			continue
		}
		if o.Verdict == "REJECT" || (o.Verdict != "" && !o.ValidationPassed) {
			overlap := wordOverlap(taskWords, significantWords(o.Task))
			if overlap >= 2 {
				date := o.Timestamp
				if len(date) >= 10 {
					date = date[:10]
				}
				label := o.Verdict
				if label == "" {
					label = "VALIDATION_FAILED"
				}
				return fmt.Sprintf("Similar task '%s' was previously %s on %s",
					truncate(o.Task, 50), label, date)
			}
		}
	}
	return ""
}

// getHotspotWarnings returns warnings for files modified many times (potential refactor candidates).
func getHotspotWarnings(files []string) []string {
	profile := loadProfile()
	r := root()
	var warnings []string
	for _, f := range files {
		rel, _ := filepath.Rel(r, f)
		rel = filepath.ToSlash(rel)
		stat, ok := profile.FileStats[rel]
		if !ok {
			continue
		}
		if stat.TouchCount >= 5 {
			warnings = append(warnings, fmt.Sprintf(
				"%s modified %d times (success: %d, reject: %d) — consider refactoring",
				rel, stat.TouchCount, stat.SuccessCount, stat.RejectCount))
		}
	}
	return warnings
}

// suggestNextTasks uses Ollama to recommend follow-up work based on the completed session.
func suggestNextTasks(target, task string, filesChanged []string) string {
	p := detectProject()
	files := strings.Join(filesChanged, ", ")
	if files == "" {
		files = "none"
	}
	prompt := fmt.Sprintf(`You are a software engineering advisor.

Project type: %s
Target module: %s
Completed task: "%s"
Files modified: %s

Suggest 3-4 concrete follow-up tasks that naturally come next.
Consider: missing tests for what changed, similar issues in related code, edge cases, documentation.
Return a numbered list only. Be specific and actionable. No preamble.`, p.Type, target, task, files)

	out, _, err := generateForRole(RoleSprintAdvisor, prompt, true)
	if err != nil {
		return ""
	}
	return out
}

// printProfile displays the project's learning state in a readable format.
func printProfile() {
	profile := loadProfile()

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Project Learning Profile")
	fmt.Println("[MERA] ========================================")
	fmt.Println(" Project type:", profile.ProjectType)
	fmt.Println(" Profile created:", profile.CreatedAt)
	fmt.Println(" Last updated:  ", profile.UpdatedAt)
	fmt.Println(" Total outcomes:", len(profile.Outcomes))

	// Per-target summary.
	if len(profile.TargetStats) > 0 {
		fmt.Println("\n Target Stats:")
		// Sort target names for stable output.
		targets := make([]string, 0, len(profile.TargetStats))
		for t := range profile.TargetStats {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			ts := profile.TargetStats[t]
			rate := 0.0
			if ts.RunCount > 0 {
				rate = float64(ts.SuccessCount) / float64(ts.RunCount) * 100
			}
			fmt.Printf("   %-20s runs: %-3d  success: %5.1f%%  avg confidence: %5.1f%%\n",
				t+":", ts.RunCount, rate, ts.AvgConfidence)
			for _, f := range ts.CommonFiles {
				fmt.Printf("     -> %s\n", f)
			}
		}
	}

	// Top 5 hotspot files.
	type fileStat struct {
		name string
		stat FileStats
	}
	var fstats []fileStat
	for name, stat := range profile.FileStats {
		fstats = append(fstats, fileStat{name, stat})
	}
	sort.Slice(fstats, func(i, j int) bool {
		return fstats[i].stat.TouchCount > fstats[j].stat.TouchCount
	})
	if len(fstats) > 0 {
		fmt.Println("\n File Hotspots (most modified):")
		limit := 8
		if len(fstats) < limit {
			limit = len(fstats)
		}
		for _, fs := range fstats[:limit] {
			fmt.Printf("   %-50s  touched: %d  ok: %d  rejected: %d\n",
				fs.name, fs.stat.TouchCount, fs.stat.SuccessCount, fs.stat.RejectCount)
		}
	}

	// Last 5 outcomes.
	if len(profile.Outcomes) > 0 {
		fmt.Println("\n Recent Outcomes:")
		start := len(profile.Outcomes) - 5
		if start < 0 {
			start = 0
		}
		for _, o := range profile.Outcomes[start:] {
			date := o.Timestamp
			if len(date) >= 10 {
				date = date[:10]
			}
			verdict := o.Verdict
			if verdict == "" {
				verdict = "NO_DIFF"
			}
			fmt.Printf("   [%-12s] %-15s %s — %s\n", verdict, o.Target+":", truncate(o.Task, 45), date)
		}
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Profile path:", profilePath())
}

// computeCommonFiles returns files that appear in ≥50% of APPROVE outcomes for a target.
func computeCommonFiles(outcomes []TaskOutcome, target string) []string {
	counts := map[string]int{}
	successes := 0
	for _, o := range outcomes {
		if o.Target != target || o.Verdict != "APPROVE" || !o.ValidationPassed {
			continue
		}
		successes++
		for _, f := range o.FilesChanged {
			counts[f]++
		}
	}
	if successes == 0 {
		return nil
	}
	threshold := successes/2 + 1
	if threshold < 1 {
		threshold = 1
	}
	var common []string
	for f, c := range counts {
		if c >= threshold {
			common = append(common, f)
		}
	}
	sort.Strings(common)
	return common
}

func significantWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(s)) {
		// Strip punctuation, skip short/common words.
		w = strings.Trim(w, `.,;:"'()[]{}`)
		if len(w) >= 4 && !isStopWord(w) {
			out = append(out, w)
		}
	}
	return out
}

func isStopWord(w string) bool {
	switch w {
	case "with", "that", "this", "from", "into", "will", "have",
		"been", "when", "then", "also", "just", "some", "more":
		return true
	}
	return false
}

func wordOverlap(a, b []string) int {
	set := map[string]bool{}
	for _, w := range a {
		set[w] = true
	}
	count := 0
	for _, w := range b {
		if set[w] {
			count++
		}
	}
	return count
}
