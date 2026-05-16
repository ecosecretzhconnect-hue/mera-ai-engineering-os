package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AgentResult is the output of a single MERA workflow agent.
// Evidence is populated only by File Scout and carries per-file confidence data.
// RejectedCandidates lists scored files that were excluded (BUGFIX_NARROW scope only).
// Model and DurationMs are populated at runtime for performance reporting.
type AgentResult struct {
	Agent              string              `json:"agent"`
	Status             string              `json:"status"`
	Output             string              `json:"output"`
	Risks              []string            `json:"risks,omitempty"`
	Files              []string            `json:"files,omitempty"`              // absolute paths
	Evidence           []FileEvidence      `json:"evidence,omitempty"`           // per-file confidence (File Scout only)
	RejectedCandidates []RejectedCandidate `json:"rejectedCandidates,omitempty"` // excluded candidates (BUGFIX_NARROW)
	Model              string              `json:"model,omitempty"`
	DurationMs         int64               `json:"durationMs,omitempty"`
}

type candidateFile struct {
	absPath string
	relPath string
}

// plannerAgent decomposes the task into concrete implementation steps via Ollama (streamed).
func plannerAgent(target, task string) AgentResult {
	p := detectProject()
	prompt := fmt.Sprintf(`You are a senior software engineering planner.

Project type: %s
Target module: %s
Task: "%s"

Break this task into 3-5 concrete, scoped implementation steps.
For each step name the exact layer it touches: controller, service, DTO, model, config, migration, test.
No preamble. Return a numbered list only.`, p.Type, target, task)

	fmt.Println("[AGENT] Planner running...")
	start := time.Now()
	out, model, err := generateForRole(RolePlanner, prompt, true)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		fmt.Println("[AGENT] Planner failed:", err)
		appendMeraLog("ERROR", "Planner agent failed (model="+model+"): "+err.Error())
		return AgentResult{Agent: "Planner", Status: "failed", Output: err.Error(), Model: model, DurationMs: elapsed}
	}
	fmt.Println("[AGENT] Planner done.")
	return AgentResult{Agent: "Planner", Status: "completed", Output: out, Model: model, DurationMs: elapsed}
}

// architectAgent analyzes architectural impact via Ollama (streamed).
// DEEP/STRICT profiles trigger extended analysis via DeeperArchitect setting.
func architectAgent(target, task string) AgentResult {
	p := detectProject()
	depthHint := "5-8 lines max. Be specific."
	if getProfileSettings().DeeperArchitect {
		depthHint = "10-15 lines. Provide thorough analysis including dependency graph, performance implications, data-flow risks, and migration considerations."
	}
	prompt := fmt.Sprintf(`You are a software architect reviewing a change request.

Project type: %s
Target module: %s
Task: "%s"

Analyze:
1. Which layers will this touch? (controller / service / DTO / DB / config)
2. Dependencies between those layers?
3. Blast radius: 1=isolated file, 2=single module, 3=cross-module, 4=system-wide
4. Critical risks?

%s`, p.Type, target, task, depthHint)

	fmt.Println("[AGENT] Architect running...")
	start := time.Now()
	out, model, err := generateForRole(RoleArchitect, prompt, true)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		fmt.Println("[AGENT] Architect failed:", err)
		appendMeraLog("ERROR", "Architect agent failed (model="+model+"): "+err.Error())
		return AgentResult{Agent: "Architect", Status: "failed", Output: err.Error(), Model: model, DurationMs: elapsed}
	}
	fmt.Println("[AGENT] Architect done.")
	return AgentResult{Agent: "Architect", Status: "completed", Output: out, Model: model, DurationMs: elapsed}
}

// fileScoutAgent identifies which specific files need to change using the confidence engine.
//
// Flow:
//  1. Score all candidate files locally (filename, directory, history, content)
//  2. Send top 15 local-scored candidates to Ollama for AI endorsement
//  3. Merge: boost Ollama-selected files by +15 confidence
//  4. Enforce maxFilesForAider cap (profile-dependent)
//  5. Return AgentResult with Evidence populated
func fileScoutAgent(target, task string) AgentResult {
	p := detectProject()

	// Pull all repo files (cached) and prioritize by target module.
	all := allRepoFiles()
	candidates := prioritize(all, target, 60)
	if len(candidates) == 0 {
		fmt.Println("[AGENT] File Scout: no candidate files found.")
		return AgentResult{Agent: "File Scout", Status: "completed", Output: "No candidate files found."}
	}

	// Score every candidate with the confidence engine.
	scored := make([]FileEvidence, 0, len(candidates))
	for _, c := range candidates {
		scored = append(scored, scoreFile(c, target, task))
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Confidence > scored[j].Confidence
	})

	// ── BUGFIX_NARROW: endpoint relationship analysis ─────────────────────
	// When the task is a narrow bug-fix (e.g. "Fix POST login 400 Bad Request"),
	// read the top controller file and extract which DTOs and services are actually
	// referenced by the target endpoint. Boost referenced files, penalise siblings.
	scope := classifyTaskScope(task)
	intent := classifyTaskIntent(task)
	if scope == ScopeBugfixNarrow {
		epKw := endpointKeyword(strings.ToLower(task))
		for _, ev := range scored {
			if ev.Confidence >= 60 && strings.Contains(strings.ToLower(ev.RelPath), "controller") {
				dtoRefs, serviceRefs := findEndpointReferences(ev.Path, epKw)
				if len(dtoRefs) > 0 || len(serviceRefs) > 0 {
					scored = applyRelationshipBoosts(scored, dtoRefs, serviceRefs, scope, intent)
					sort.Slice(scored, func(i, j int) bool {
						return scored[i].Confidence > scored[j].Confidence
					})
					fmt.Printf("[AGENT] BUGFIX_NARROW: endpoint relationship analysis applied (endpoint: %q).\n", epKw)
				}
				break // use only the highest-confidence controller
			}
		}
	}

	// Send top 15 (by local score) to Ollama — keeps prompt small and within timeout.
	const ollamaTopN = 15
	topForOllama := scored
	if len(topForOllama) > ollamaTopN {
		topForOllama = topForOllama[:ollamaTopN]
	}

	taskDomain := inferTaskDomain(task)
	var sb strings.Builder
	for _, ev := range topForOllama {
		sample := sampleFile(ev.Path, 20) // 20 lines per file
		fileClass := classifyFile(ev.RelPath)
		sb.WriteString(fmt.Sprintf("--- %s [score:%d%% type:%s] ---\n%s\n\n",
			ev.RelPath, ev.Confidence, fileClass, sample))
	}

	// Inject learned file hints as explicit prompt context.
	hintsBlock := ""
	if hints := getFileHints(target); len(hints) > 0 {
		hintsBlock = fmt.Sprintf("\nHistorically confirmed files for the %s module:\n", target)
		for _, h := range hints {
			hintsBlock += "- " + h + "\n"
		}
		hintsBlock += "Treat these as strong candidates if relevant to the task.\n"
	}

	ps := getProfileSettings()
	domainHint := ""
	if taskDomain == "backend" {
		domainHint = "\nThis is a backend task — prefer controller, service, repository, and DTO files over frontend files.\n"
	} else if taskDomain == "frontend" {
		domainHint = "\nThis is a frontend task — prefer components, hooks, services, and store files.\n"
	}

	// For BUGFIX_NARROW tasks, ask Ollama for fewer files — surgical selection.
	const narrowFileLimit = 4
	maxFilesForPrompt := ps.MaxFiles
	if scope == ScopeBugfixNarrow && maxFilesForPrompt > narrowFileLimit {
		maxFilesForPrompt = narrowFileLimit
	}

	scopeHint := ""
	if scope == ScopeBugfixNarrow {
		scopeHint = "\nThis is a narrow bug-fix task. Return ONLY the files directly involved in the failing endpoint — no sibling DTOs or unrelated services.\n"
	}

	prompt := fmt.Sprintf(`You are a precise file scout for a software project.

Project type: %s
Target module: %s
Task: "%s"
%s%s%s
Candidate files (relative path + local score + file type + first 20 lines):
%s
Which of these files need to be modified to complete this task?
Return ONLY a JSON array of relative paths. Maximum %d files. No markdown, no explanation.
Example: ["Identity/Controllers/AuthController.cs", "Identity/DTOs/LoginRequest.cs"]`,
		p.Type, target, task, domainHint, hintsBlock, scopeHint, sb.String(), maxFilesForPrompt)

	fmt.Println("[AGENT] File Scout querying Ollama...")
	start := time.Now()
	out, model, err := generateForRole(RoleFileScout, prompt, false)
	elapsed := time.Since(start).Milliseconds()

	var finalEvidence []FileEvidence
	if err != nil {
		// Ollama timed out or unavailable — use top local scores directly.
		fmt.Printf("[AGENT] File Scout degraded: %v — returning top local-scored files.\n", err)
		fmt.Println("[AGENT]         Tip: run 'mera -ExplainSelection' to inspect local scoring details.")
		appendMeraLog("WARN", "File Scout degraded — Ollama unavailable (model="+model+"): "+err.Error())
		finalEvidence = sanitizeEvidence(scored) // sanitize before limiting
		lim := effectiveFileLimit()
		if len(finalEvidence) > lim {
			finalEvidence = finalEvidence[:lim]
		}
		r := buildScoutResult("degraded",
			"Ollama unavailable — local scores only. Run mera -ExplainSelection to inspect scoring.",
			finalEvidence)
		r.Model, r.DurationMs = model, elapsed
		return r
	}

	relPaths := parseJSONStringArray(out)
	if len(relPaths) == 0 {
		// Ollama returned unparseable output — fall back to local scores.
		fmt.Println("[AGENT] File Scout: unparseable Ollama output, using local scores.")
		appendMeraLog("WARN", "File Scout: unparseable output from model="+model+" — fell back to local scores")
		finalEvidence = sanitizeEvidence(scored) // sanitize before limiting
		lim := effectiveFileLimit()
		if len(finalEvidence) > lim {
			finalEvidence = finalEvidence[:lim]
		}
		r := buildScoutResult("degraded", "Unparseable AI output — local scores used.", finalEvidence)
		r.Model, r.DurationMs = model, elapsed
		return r
	}

	// Merge: boost Ollama-endorsed files and cap at profile file limit.
	finalEvidence = mergeWithOllamaSelection(relPaths, scored)

	// ── BUGFIX_NARROW: tighter dynamic file cap ───────────────────────────
	// Profile limit (e.g. 6 for NORMAL) may still be too broad for a narrow bug fix.
	// After relationship boosts have re-ranked files, apply a hard cap of narrowFileLimit.
	if scope == ScopeBugfixNarrow && len(finalEvidence) > narrowFileLimit {
		fmt.Printf("[AGENT] BUGFIX_NARROW: capped selection at %d files (profile allows %d).\n",
			narrowFileLimit, effectiveFileLimit())
		finalEvidence = finalEvidence[:narrowFileLimit]
	}

	// Remove any entries whose absolute path no longer exists on disk.
	var verified []FileEvidence
	for _, ev := range finalEvidence {
		if exists(ev.Path) {
			verified = append(verified, ev)
		}
	}

	// ── Final exclusion sanitizer (last-resort defense) ──────────────────
	// Even if an excluded path somehow survived the walker, the cache filter,
	// the scorer, and Ollama's selection, this hard gate catches it before
	// any excluded file is ever returned as evidence.
	verified = sanitizeEvidence(verified)

	if len(verified) == 0 {
		fmt.Println("[AGENT] File Scout: no verified files after path resolution.")
		return buildScoutResult("degraded", "No resolvable files found.", nil)
	}

	// ── Collect rejected candidates (BUGFIX_NARROW only) ──────────────────
	// Record high-scoring files that were excluded so ExplainSelection can show WHY.
	var rejected []RejectedCandidate
	if scope == ScopeBugfixNarrow {
		selectedPaths := map[string]bool{}
		for _, ev := range verified {
			selectedPaths[ev.RelPath] = true
		}
		for _, ev := range scored {
			if selectedPaths[ev.RelPath] || ev.Confidence < 30 {
				continue
			}
			rejected = append(rejected, RejectedCandidate{
				RelPath: ev.RelPath,
				Score:   ev.Confidence,
				Reason:  rejectionReason(ev, scope, intent),
			})
			if len(rejected) >= 6 {
				break
			}
		}
	}

	fmt.Printf("[AGENT] File Scout identified %d file(s).\n", len(verified))
	r := buildScoutResult("completed", fmt.Sprintf("Identified %d file(s) with confidence scoring.", len(verified)), verified)
	r.RejectedCandidates = rejected
	r.Model, r.DurationMs = model, elapsed
	return r
}

// sanitizeEvidence removes any FileEvidence entry whose path matches isExcludedPath.
// This is the last-resort filter — it fires regardless of how the path entered the list.
// Every exit path from fileScoutAgent (success, degraded, fallback) must call this.
func sanitizeEvidence(in []FileEvidence) []FileEvidence {
	var out []FileEvidence
	removed := 0
	for _, ev := range in {
		if isExcludedPath(ev.RelPath) {
			removed++
			appendMeraLog("WARN",
				fmt.Sprintf("File Scout exclusion sanitizer removed path: %s", ev.RelPath))
			fmt.Printf("[AGENT] [EXCLUSION] Removed excluded path from results: %s\n", ev.RelPath)
			continue
		}
		out = append(out, ev)
	}
	if removed > 0 {
		fmt.Printf("[AGENT] [EXCLUSION] Sanitizer removed %d excluded path(s) from File Scout results.\n", removed)
	}
	return out
}

// securityAgent assesses risk via Ollama (streamed) + pattern scan.
func securityAgent(target, task string) AgentResult {
	p := detectProject()
	prompt := fmt.Sprintf(`You are a security reviewer for a software project.

Project type: %s
Target module: %s
Task: "%s"

Assess this change:
1. Does it touch authentication, authorization, or session management?
2. Does it touch payments, PII, or secrets?
3. Is there injection risk (SQL, command, path traversal)?
4. State risk level on the last line as: RISK: LOW / MEDIUM / HIGH

4-6 lines. Be concise.`, p.Type, target, task)

	fmt.Println("[AGENT] Security running...")
	start := time.Now()
	out, model, err := generateForRole(RoleSecurity, prompt, true)
	elapsed := time.Since(start).Milliseconds()
	patternRisks := patternSecurityScan(target, task)

	if err != nil {
		fmt.Println("[AGENT] Security degraded to pattern-only scan.")
		appendMeraLog("WARN", "Security agent degraded to pattern-only (model="+model+"): "+err.Error())
		return AgentResult{Agent: "Security", Status: "degraded", Output: "Pattern-only scan (Ollama unavailable).", Risks: patternRisks, Model: model, DurationMs: elapsed}
	}
	fmt.Println("[AGENT] Security done.")
	return AgentResult{Agent: "Security", Status: "completed", Output: out, Risks: patternRisks, Model: model, DurationMs: elapsed}
}

// diffReviewAgent reads the current git diff and asks Ollama to review the changes.
func diffReviewAgent(target, task string) AgentResult {
	diff := strings.TrimSpace(capture("git", "diff", "HEAD"))
	if diff == "" {
		diff = strings.TrimSpace(capture("git", "diff"))
	}
	if diff == "" {
		return AgentResult{Agent: "Diff Review", Status: "completed", Output: "No changes detected in working tree."}
	}
	const maxDiff = 4000
	if len(diff) > maxDiff {
		diff = diff[:maxDiff] + "\n... (diff truncated)"
	}
	prompt := fmt.Sprintf(`You are a code reviewer analyzing a git diff.

Task that was implemented: "%s"
Target module: %s

Git diff:
%s

Review:
1. Does it correctly implement the stated task?
2. Any quality issues, missing pieces, or incomplete changes?
3. Any security or safety concerns introduced?
4. Final verdict on the last line: VERDICT: APPROVE / NEEDS_REVIEW / REJECT

Be concise. 6-10 lines.`, task, target, diff)

	fmt.Println("[AGENT] Diff Review running...")
	start := time.Now()
	out, model, err := generateForRole(RoleDiffReview, prompt, true)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		appendMeraLog("WARN", "Diff Review failed (model="+model+"): "+err.Error())
		return AgentResult{Agent: "Diff Review", Status: "degraded", Output: "Could not review diff: " + err.Error(), Model: model, DurationMs: elapsed}
	}
	fmt.Println("[AGENT] Diff Review done.")
	return AgentResult{Agent: "Diff Review", Status: "completed", Output: out, Model: model, DurationMs: elapsed}
}

// qaAgent returns the validation commands for this project.
func qaAgent(target, task string) AgentResult {
	p := detectProject()
	cmds := []string{p.Build, p.Test}
	if p.FrontendBuild != "" {
		cmds = append(cmds, p.FrontendBuild)
	}
	fmt.Println("[AGENT] QA prepared validation plan.")
	return AgentResult{Agent: "QA", Status: "completed", Output: strings.Join(cmds, " -> ")}
}

// reviewAgent summarizes the current diff and blast radius.
func reviewAgent(target, task string) AgentResult {
	fs := changedFiles()
	br := blastRadius(fs)
	fmt.Println("[AGENT] Review done.")
	return AgentResult{
		Agent:  "Review",
		Status: "completed",
		Output: fmt.Sprintf("Changed files: %d | Blast radius: %d", len(fs), br),
		Files:  fs,
	}
}

// buildScoutResult assembles an AgentResult from a FileEvidence slice.
func buildScoutResult(status, output string, evidence []FileEvidence) AgentResult {
	var absPaths []string
	for _, ev := range evidence {
		absPaths = append(absPaths, ev.Path)
	}
	return AgentResult{
		Agent:    "File Scout",
		Status:   status,
		Output:   output,
		Files:    absPaths,
		Evidence: evidence,
	}
}

// moduleVariants returns all lower-case path patterns to match for a given target name.
// For "Identity" it generates patterns like "identity", "identity.api", "hconnect.identity", etc.
func moduleVariants(target string) []string {
	t := strings.ToLower(target)
	return []string{
		t,
		t + ".api", t + ".logic", t + ".repository", t + ".models",
		t + ".tests", t + ".data", t + ".core", t + ".domain",
		t + ".infrastructure", t + ".application",
		"hconnect." + t,
		"ecosecretz." + t,
		"ecosecretz.hconnect." + t,
	}
}

// prioritize returns candidates with target-matching files first, up to limit.
// It uses the solution module map (from .sln / .slnx) when available, and falls
// back to module name variants when not.
// Primary bucket: files whose path matches the target module.
// Secondary bucket: everything else, included only when primary doesn't fill the limit.
func prioritize(all []candidateFile, target string, limit int) []candidateFile {
	targetLower := strings.ToLower(target)
	variants := moduleVariants(targetLower)

	// Build the set of known module project dirs from solution files.
	modMap := solutionModuleMap()
	moduleDirs := modMap[targetLower] // e.g. ["Identity/Src/EcoSecretz.HConnect.Identity.API", ...]

	isModuleMatch := func(c candidateFile) bool {
		pathLower := strings.ToLower(filepath.ToSlash(c.relPath))

		// Solution-derived exact dir prefix match (most reliable).
		for _, mdir := range moduleDirs {
			prefix := strings.ToLower(filepath.ToSlash(mdir)) + "/"
			if strings.HasPrefix(pathLower, prefix) {
				return true
			}
		}
		// Name-variant containment match.
		for _, v := range variants {
			if strings.Contains(pathLower, v) {
				return true
			}
		}
		return false
	}

	var primary, secondary []candidateFile
	for _, c := range all {
		if targetLower != "" && isModuleMatch(c) {
			primary = append(primary, c)
		} else {
			secondary = append(secondary, c)
		}
	}

	// Fill up to limit: all primary first, then secondary for remaining slots.
	combined := primary
	if rem := limit - len(combined); rem > 0 && len(secondary) > 0 {
		if rem > len(secondary) {
			rem = len(secondary)
		}
		combined = append(combined, secondary[:rem]...)
	}
	if len(combined) > limit {
		combined = combined[:limit]
	}
	return combined
}

// resolveExisting converts relative paths to absolute paths that exist on disk.
func resolveExisting(relPaths []string) []string {
	r := root()
	var out []string
	for _, rel := range relPaths {
		abs := filepath.Join(r, filepath.FromSlash(rel))
		if exists(abs) {
			out = append(out, abs)
		}
	}
	return out
}

func patternSecurityScan(target, task string) []string {
	var risks []string
	l := strings.ToLower(target + " " + task)
	if strings.Contains(l, "auth") || strings.Contains(l, "login") || strings.Contains(l, "jwt") || strings.Contains(l, "token") {
		risks = append(risks, "Authentication-related change — human review required.")
	}
	if strings.Contains(l, "payment") || strings.Contains(l, "razorpay") || strings.Contains(l, "stripe") {
		risks = append(risks, "Payment-related change — security review mandatory.")
	}
	if strings.Contains(l, "secret") || strings.Contains(l, "password") || strings.Contains(l, "credential") {
		risks = append(risks, "Possible secret exposure — verify no credentials in code.")
	}
	return risks
}
