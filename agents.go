package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// narrowFileLimit is the hard file cap applied to BUGFIX_NARROW tasks.
// Controller + request DTO + service implementation is sufficient for the vast majority
// of narrow endpoint bugfixes. Anything beyond 3 files (JWT helpers, OpenAPI filters,
// schema examples) adds noise and increases the risk of Aider context overflow.
const narrowFileLimit = 3

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

// plannerAgent decomposes the task into concrete implementation steps.
//
// BUGFIX_NARROW fast path: returns a deterministic plan instantly without calling Ollama.
// Phase 10.14: Uses generic intent classifier to support any domain (not just auth).
// The LLM path is only used when the task scope is broader OR when --deep-plan is passed.
// This eliminates the #1 source of pre-Aider latency (phi4 planning call, typically 60-180s).
func plannerAgent(target, task string) AgentResult {
	scope := classifyTaskScope(task)
	if scope == ScopeBugfixNarrow && !deepPlanRequested() {
		intent := classifyTaskIntentGeneric(task, target)
		if intent == IntentSingleComponentFix {
			return deterministicPlannerResultGeneric(target, task)
		}
		return deterministicPlannerResult(target, task)
	}

	p := detectProject()
	prompt := fmt.Sprintf(`You are a senior software engineering planner.

Project type: %s
Target module: %s
Task: "%s"

Break this task into 3-5 concrete, scoped implementation steps.
For each step name the exact layer it touches: controller, service, DTO, model, config, migration, test.
No preamble. Return a numbered list only.`, p.Type, target, task)

	fmt.Println("[AGENT] Planner running (LLM)...")
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

// deterministicPlannerResult builds a precise, immediate plan for BUGFIX_NARROW tasks.
// No Ollama call — derived from task keywords and project conventions.
func deterministicPlannerResult(target, task string) AgentResult {
	taskL := strings.ToLower(task)
	fmt.Println("[AGENT] Planner: BUGFIX_NARROW fast path — deterministic plan (no LLM).")

	var sb strings.Builder
	sb.WriteString("Deterministic plan (BUGFIX_NARROW — skipped LLM, use --deep-plan to enable):\n\n")

	// Step 1: always inspect the controller / endpoint entry point
	sb.WriteString("1. Controller layer — inspect action signature, [FromBody] attribute, HTTP verb, and route; confirm they match the failing request exactly.\n")

	// Step 2: DTO / request model
	sb.WriteString("2. DTO layer — verify all required fields carry [Required] (or equivalent), correct types, and no name mismatches with the serialized JSON.\n")

	// Step 3: service or auth logic if relevant
	if strings.Contains(taskL, "auth") || strings.Contains(taskL, "login") ||
		strings.Contains(taskL, "token") || strings.Contains(taskL, "credential") {
		sb.WriteString("3. Auth service — confirm the service method receives the correct parameters from the controller; check for null-guard or early return that could short-circuit the flow.\n")
	} else if strings.Contains(taskL, "service") || strings.Contains(taskL, "logic") {
		sb.WriteString("3. Service layer — trace the call chain from controller to service; confirm parameter and return types align.\n")
	} else {
		sb.WriteString("3. Service / dependency — trace one layer down from the controller to confirm the call succeeds with the corrected input.\n")
	}

	// Step 4: apply patch
	sb.WriteString("4. Apply minimal targeted patch — one change per layer, smallest possible diff; do not refactor or rename unrelated code.\n")

	// Step 5: validate
	sb.WriteString("5. Build → test → review git diff — confirm compilation, tests pass, and no unintended files changed.")

	appendMeraLog("INFO", fmt.Sprintf("Deterministic planner used for BUGFIX_NARROW target=%s", target))
	return AgentResult{
		Agent:  "Planner",
		Status: "completed",
		Output: sb.String(),
		Model:  "deterministic",
	}
}

// architectAgent analyzes architectural impact.
//
// BUGFIX_NARROW fast path: returns a deterministic architectural summary derived from
// task keywords without calling Ollama. Phase 10.14: Uses generic intent classifier for any domain.
// Eliminates the architect LLM call (typically 60-120s).
// DEEP/STRICT profiles trigger extended LLM analysis via DeeperArchitect setting.
func architectAgent(target, task string) AgentResult {
	scope := classifyTaskScope(task)
	if scope == ScopeBugfixNarrow && !deepPlanRequested() {
		intent := classifyTaskIntentGeneric(task, target)
		if intent == IntentSingleComponentFix {
			return deterministicArchitectResultGeneric(target, task)
		}
		return deterministicArchitectResult(target, task)
	}

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

	fmt.Println("[AGENT] Architect running (LLM)...")
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

// deterministicArchitectResult builds a precise architectural summary for BUGFIX_NARROW tasks.
// Derives layers, blast radius, and risks from task keywords — no Ollama call required.
func deterministicArchitectResult(target, task string) AgentResult {
	taskL := strings.ToLower(task)
	fmt.Println("[AGENT] Architect: BUGFIX_NARROW fast path — deterministic analysis (no LLM).")

	// Identify which layers from task keywords
	var layers []string
	if strings.ContainsAny(taskL, "controller endpoint api route") {
		layers = append(layers, "controller")
	}
	if strings.Contains(taskL, "dto") || strings.Contains(taskL, "request") ||
		strings.Contains(taskL, "model") || strings.Contains(taskL, "binding") {
		layers = append(layers, "DTO/request")
	}
	if strings.Contains(taskL, "service") || strings.Contains(taskL, "logic") ||
		strings.Contains(taskL, "auth") || strings.Contains(taskL, "login") {
		layers = append(layers, "service")
	}
	if len(layers) == 0 {
		layers = []string{"controller", "DTO/request"} // safe default
	}

	// Blast radius — narrow bugfix is always single-module
	blast := "2 — single module (change is contained within " + target + ")"

	// Risk assessment from keywords
	var risks []string
	if strings.Contains(taskL, "auth") || strings.Contains(taskL, "login") ||
		strings.Contains(taskL, "credential") || strings.Contains(taskL, "token") {
		risks = append(risks, "Auth layer touched — ensure no bypass of existing validation or middleware chain.")
	}
	if strings.Contains(taskL, "binding") || strings.Contains(taskL, "dto") ||
		strings.Contains(taskL, "request") {
		risks = append(risks, "Model binding — verify [Required] attributes and property names match JSON payload exactly.")
	}
	if strings.Contains(taskL, "400") || strings.Contains(taskL, "bad request") {
		risks = append(risks, "400 Bad Request — likely model validation failure; check ModelState.IsValid is evaluated before service call.")
	}
	if len(risks) == 0 {
		risks = []string{"Low risk — isolated change within single module, no DB or config involved."}
	}

	var sb strings.Builder
	sb.WriteString("Deterministic architecture analysis (BUGFIX_NARROW — skipped LLM):\n\n")
	sb.WriteString(fmt.Sprintf("Layers touched  : %s\n", strings.Join(layers, " → ")))
	sb.WriteString(fmt.Sprintf("Blast radius    : %s\n", blast))
	sb.WriteString("Dependencies    : controller → service (one call, no cascade side-effects expected)\n")
	sb.WriteString("Critical risks  :\n")
	for _, r := range risks {
		sb.WriteString("  - " + r + "\n")
	}

	appendMeraLog("INFO", fmt.Sprintf("Deterministic architect used for BUGFIX_NARROW target=%s", target))
	return AgentResult{
		Agent:  "Architect",
		Status: "completed",
		Output: strings.TrimRight(sb.String(), "\n"),
		Model:  "deterministic",
	}
}

// ── PHASE 10.14: Generic Deterministic Planner & Architect ────────────────
// Works for any domain (not just auth), based on task-driven symbol detection.

// deterministicPlannerResultGeneric builds a generic plan for single-component bugfixes.
// Analyzes task symbols (class/method names) to provide domain-agnostic steps.
func deterministicPlannerResultGeneric(target, task string) AgentResult {
	taskL := strings.ToLower(task)
	fmt.Println("[AGENT] Planner: BUGFIX_NARROW fast path — deterministic plan (no LLM).")

	symbols := extractSymbolsFromTask(task)

	var sb strings.Builder
	sb.WriteString("Deterministic plan (BUGFIX_NARROW — skipped LLM, use --deep-plan to enable):\n\n")

	// Step 1: Locate target
	if len(symbols) > 0 {
		sb.WriteString(fmt.Sprintf("1. Locate target — Find %s in the identified file(s) (target: %s module)\n", symbols[0], target))
	} else {
		sb.WriteString("1. Locate target — Find the target class/method in the identified file(s)\n")
	}

	// Step 2: Understand inputs
	sb.WriteString("2. Understand inputs — Review method/function signature, parameter types, and expected behavior\n")

	// Step 3: Analyze root cause (varies by task keywords)
	if containsAny(taskL, []string{"null", "nil", "undefined"}) {
		sb.WriteString("3. Add guard — Check for null/nil reference before use; implement appropriate null-check or default handling\n")
	} else if containsAny(taskL, []string{"calculation", "arithmetic", "math", "sum", "product", "return"}) {
		sb.WriteString("3. Trace logic — Follow arithmetic step-by-step; verify operators, precedence, and return value\n")
	} else if containsAny(taskL, []string{"condition", "if", "loop", "boundary", "off-by-one"}) {
		sb.WriteString("3. Examine control flow — Review boolean condition logic and loop boundaries\n")
	} else if containsAny(taskL, []string{"error", "exception", "fail"}) {
		sb.WriteString("3. Identify error — Trace the failure path and pinpoint the line causing the error\n")
	} else {
		sb.WriteString("3. Analyze behavior — Understand current implementation and compare against expected behavior\n")
	}

	// Step 4: Apply fix
	sb.WriteString("4. Apply minimal fix — One change per layer, smallest possible diff; do not refactor unrelated code\n")

	// Step 5: Validate
	sb.WriteString("5. Verify fix — Build/test, confirm behavior matches requirement, review git diff\n")

	appendMeraLog("INFO", fmt.Sprintf("Generic deterministic planner used for BUGFIX_NARROW target=%s intent=SingleComponentFix", target))
	return AgentResult{
		Agent:  "Planner",
		Status: "completed",
		Output: sb.String(),
		Model:  "deterministic",
	}
}

// deterministicArchitectResultGeneric provides generic architectural analysis for single-component bugfixes.
func deterministicArchitectResultGeneric(target, task string) AgentResult {
	taskL := strings.ToLower(task)
	fmt.Println("[AGENT] Architect: BUGFIX_NARROW fast path — deterministic analysis (no LLM).")

	symbols := extractSymbolsFromTask(task)

	// Identify likely layers based on symbols and keywords
	var layers []string

	// Heuristic: if method name suggests controller/endpoint, it's presentation
	if len(symbols) > 0 {
		firstSymbol := strings.ToLower(symbols[0])
		if containsAny(firstSymbol, []string{"controller", "handler", "endpoint", "api", "service"}) {
			layers = append(layers, "presentation/handler")
		}
	}

	// Most method fixes are in logic/service layer
	if len(symbols) > 0 {
		layers = append(layers, "logic/method")
	}

	// If task mentions data/database/query, add data access
	if containsAny(taskL, []string{"database", "query", "repository", "dao", "sql"}) {
		layers = append(layers, "data-access")
	}

	if len(layers) == 0 {
		layers = []string{"method-level"}
	}

	// Blast radius — narrow bugfix is always single-module
	blast := "2 — single module (change is contained within " + target + ")"

	// Generic risk assessment
	var risks []string
	if containsAny(taskL, []string{"null", "nil", "undefined"}) {
		risks = append(risks, "Null-safety — ensure guard clause prevents downstream errors.")
	}
	if containsAny(taskL, []string{"calculation", "arithmetic", "math"}) {
		risks = append(risks, "Logic correctness — verify operator precedence and boundary conditions.")
	}
	if containsAny(taskL, []string{"condition", "if", "loop"}) {
		risks = append(risks, "Control flow — confirm boolean logic and loop termination.")
	}
	if len(risks) == 0 {
		risks = []string{"Low risk — isolated change within single module, no cross-layer dependencies."}
	}

	var sb strings.Builder
	sb.WriteString("Deterministic architecture analysis (BUGFIX_NARROW — skipped LLM):\n\n")
	sb.WriteString(fmt.Sprintf("Layers touched  : %s\n", strings.Join(layers, " → ")))
	sb.WriteString(fmt.Sprintf("Blast radius    : %s\n", blast))
	sb.WriteString("Dependencies    : isolated method change, no cascade side-effects expected\n")
	sb.WriteString("Critical risks  :\n")
	for _, r := range risks {
		sb.WriteString("  - " + r + "\n")
	}

	appendMeraLog("INFO", fmt.Sprintf("Generic deterministic architect used for BUGFIX_NARROW target=%s intent=SingleComponentFix", target))
	return AgentResult{
		Agent:  "Architect",
		Status: "completed",
		Output: strings.TrimRight(sb.String(), "\n"),
		Model:  "deterministic",
	}
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

	// ── BUGFIX_NARROW: endpoint relationship analysis ──────────────────────
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

	// ── BUGFIX_NARROW: local-first fast path ─────────────────────────────────
	// If the top local-scored file has confidence ≥70% the local engine is confident
	// enough to produce a good selection without an Ollama round-trip. This eliminates
	// the FileScout LLM call (typically 30-120s) for the common case where a clear
	// controller/DTO pair dominates the score.
	if scope == ScopeBugfixNarrow && len(scored) > 0 && scored[0].Confidence >= 70 {
		fmt.Printf("[AGENT] File Scout: local confidence %d%% ≥70%% — BUGFIX_NARROW fast path (no Ollama).\n",
			scored[0].Confidence)
		appendMeraLog("INFO", fmt.Sprintf("File Scout local-first fast path: top confidence %d%% for %s",
			scored[0].Confidence, target))

		fe := sanitizeEvidence(scored)
		if len(fe) > narrowFileLimit {
			fe = fe[:narrowFileLimit]
		}
		fe = filterNoisyFilesForNarrowBugfix(fe)

		var localVerified []FileEvidence
		for _, ev := range fe {
			if exists(ev.Path) {
				localVerified = append(localVerified, ev)
			}
		}
		localVerified = sanitizeEvidence(localVerified)

		if len(localVerified) == 0 {
			fmt.Println("[AGENT] File Scout: no verified files after local fast path.")
			return buildScoutResult("degraded", "No resolvable files found.", nil)
		}

		// Collect rejected candidates for -ExplainSelection reporting.
		selectedPaths := map[string]bool{}
		for _, ev := range localVerified {
			selectedPaths[ev.RelPath] = true
		}
		var localRejected []RejectedCandidate
		for _, ev := range scored {
			if selectedPaths[ev.RelPath] || ev.Confidence < 30 {
				continue
			}
			localRejected = append(localRejected, RejectedCandidate{
				RelPath: ev.RelPath,
				Score:   ev.Confidence,
				Reason:  rejectionReason(ev, scope, intent),
			})
			if len(localRejected) >= 6 {
				break
			}
		}

		fmt.Printf("[AGENT] File Scout identified %d file(s) (local-only fast path).\n", len(localVerified))
		r := buildScoutResult("completed",
			fmt.Sprintf("Identified %d file(s) — local confidence fast path (no LLM required).", len(localVerified)),
			localVerified)
		r.RejectedCandidates = localRejected
		return r
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
	// Uses the package-level narrowFileLimit (3): controller + request DTO + service is
	// sufficient. More files add noise and risk Aider context overflow → startup timeout.
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

	// Use capped timeout for BUGFIX_NARROW tasks that fell through local fast path
	// (confidence <70%); prevents the 120s NORMAL FileScout timeout from blocking.
	fmt.Println("[AGENT] File Scout querying Ollama...")
	start := time.Now()
	var out, model string
	var err error
	if scope == ScopeBugfixNarrow {
		ps := getProfileSettings()
		out, model, err = generateForRoleCapped(RoleFileScout, prompt, false, ps.NarrowBugfixTimeout)
	} else {
		out, model, err = generateForRole(RoleFileScout, prompt, false)
	}
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

	// ── BUGFIX_NARROW: noise-file exclusion ───────────────────────────────
	// Strip infrastructure/cross-cutting files that are almost never the root cause
	// of a narrow functional bug (JWT internals, OpenAPI filters, schema examples, etc.)
	// and that cause context overflow when fed to qwen2.5-coder:7b.
	if scope == ScopeBugfixNarrow {
		finalEvidence = filterNoisyFilesForNarrowBugfix(finalEvidence)
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

// filterNoisyFilesForNarrowBugfix removes infrastructure / cross-cutting files
// that are almost never the root cause of a narrow functional bug but add context
// weight that pushes small edit models (qwen2.5-coder:7b) past their effective window.
//
// Excluded by basename keyword (case-insensitive):
//   - jwt*         — JWT token internals (signing, validation config)
//   - *openapi*    — OpenAPI / Swagger document filters
//   - *swagger*    — Swagger UI setup files
//   - *schema*     — schema example / validator registrations
//   - *filter*     — ASP.NET action filters, exception filters
//   - *example*    — response-example providers
//
// These files are NOT excluded globally — only for BUGFIX_NARROW scope where
// the 3-file cap already enforces surgical precision. Broader scopes (FULL,
// NORMAL) still include them when their scores warrant it.
func filterNoisyFilesForNarrowBugfix(in []FileEvidence) []FileEvidence {
	noisyKeywords := []string{"jwt", "openapi", "swagger", "schema", "filter", "example"}
	var out []FileEvidence
	for _, ev := range in {
		base := strings.ToLower(filepath.Base(ev.Path))
		noisy := false
		for _, kw := range noisyKeywords {
			if strings.Contains(base, kw) {
				noisy = true
				break
			}
		}
		if noisy {
			fmt.Printf("[AGENT] BUGFIX_NARROW: dropped noisy file %s (infrastructure/cross-cutting)\n", ev.RelPath)
			appendMeraLog("INFO", fmt.Sprintf("BUGFIX_NARROW noise filter dropped: %s", ev.RelPath))
		} else {
			out = append(out, ev)
		}
	}
	return out
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

// securityAgent assesses risk via pattern scan + optional Ollama (streamed).
//
// BUGFIX_NARROW fast path: runs pattern scan first (instant). Only escalates to the LLM
// when high-risk signals are present (payment, SQL/raw query, secrets, file/path/process APIs).
// This eliminates the security LLM call (~30-90s) for low-risk narrow bugfixes like auth
// endpoint model binding fixes where the pattern scan already provides sufficient coverage.
func securityAgent(target, task string) AgentResult {
	scope := classifyTaskScope(task)
	patternRisks := patternSecurityScan(target, task)

	if scope == ScopeBugfixNarrow {
		if !requiresLLMSecurity(task, patternRisks) {
			fmt.Println("[AGENT] Security: BUGFIX_NARROW fast path — pattern scan only (no high-risk signals).")
			appendMeraLog("INFO", fmt.Sprintf("Security pattern-only fast path for BUGFIX_NARROW target=%s", target))
			msg := "Pattern scan passed. No high-risk signals detected (payment/SQL/secrets/file-ops). Risk: LOW."
			if len(patternRisks) > 0 {
				msg = fmt.Sprintf("Pattern scan flagged %d item(s) — review risks below. No LLM escalation required.", len(patternRisks))
			}
			return AgentResult{
				Agent:  "Security",
				Status: "completed",
				Output: msg,
				Risks:  patternRisks,
				Model:  "pattern-only",
			}
		}
		fmt.Println("[AGENT] Security: high-risk signals detected — escalating to LLM for BUGFIX_NARROW.")
	}

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

	fmt.Println("[AGENT] Security running (LLM)...")
	start := time.Now()
	var out, model string
	var err error
	if scope == ScopeBugfixNarrow {
		ps := getProfileSettings()
		out, model, err = generateForRoleCapped(RoleSecurity, prompt, true, ps.NarrowBugfixTimeout)
	} else {
		out, model, err = generateForRole(RoleSecurity, prompt, true)
	}
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		fmt.Println("[AGENT] Security degraded to pattern-only scan.")
		appendMeraLog("WARN", "Security agent degraded to pattern-only (model="+model+"): "+err.Error())
		return AgentResult{Agent: "Security", Status: "degraded", Output: "Pattern-only scan (Ollama unavailable).", Risks: patternRisks, Model: model, DurationMs: elapsed}
	}
	fmt.Println("[AGENT] Security done.")
	return AgentResult{Agent: "Security", Status: "completed", Output: out, Risks: patternRisks, Model: model, DurationMs: elapsed}
}

// requiresLLMSecurity returns true when a BUGFIX_NARROW task contains high-risk signals
// that justify an LLM security review beyond the pattern scan.
//
// Triggers:
//   - Payment/billing keywords in task
//   - SQL/raw query execution keywords
//   - Secret/credential/environment variable manipulation
//   - File system, path traversal, or process execution keywords
//   - Pattern scan found injection-level findings (SQL injection, payment, credentials)
func requiresLLMSecurity(task string, patternRisks []string) bool {
	taskL := strings.ToLower(task)
	highRiskKeywords := []string{
		// Payment
		"payment", "stripe", "paypal", "billing", "charge", "invoice",
		// SQL / injection
		"sql", "raw query", "execute", "exec(", "sp_",
		// Secrets / config
		"secret", "password", "credential", "appsettings", "env var", "connectionstring",
		// File / process
		"file upload", "upload", "download", "path traversal", "shell", "process",
	}
	for _, kw := range highRiskKeywords {
		if strings.Contains(taskL, kw) {
			return true
		}
	}
	// Escalate if pattern scan found critical risk categories
	for _, r := range patternRisks {
		rL := strings.ToLower(r)
		if strings.Contains(rL, "sql") || strings.Contains(rL, "injection") ||
			strings.Contains(rL, "payment") || strings.Contains(rL, "secret") ||
			strings.Contains(rL, "credential") {
			return true
		}
	}
	return false
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

// reviewAgent summarizes the actual git diff and blast radius.
// Uses changedFiles() (git diff --name-only HEAD) so the count and file list
// always reflect real tracked changes — never untracked or fabricated counts.
func reviewAgent(target, task string) AgentResult {
	fs := changedFiles()
	br := blastRadius(fs)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Changed files: %d | Blast radius: %d\n", len(fs), br))
	if len(fs) > 0 {
		sb.WriteString("Files modified:\n")
		for _, f := range fs {
			sb.WriteString("  - " + f + "\n")
		}
	} else {
		sb.WriteString("No tracked file changes detected (git diff HEAD is empty).\n")
	}
	fmt.Println("[AGENT] Review done.")
	return AgentResult{
		Agent:  "Review",
		Status: "completed",
		Output: strings.TrimRight(sb.String(), "\n"),
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
