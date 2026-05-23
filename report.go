package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codePhase outcome constants — written to WorkflowReport.ChangeStatus.
const (
	codePhaseSuccess  = "success"   // Aider ran, files changed, validation ran
	codePhaseNoChange = "no_change" // Aider ran cleanly but produced zero file changes
	codePhaseFailed   = "failed"    // Aider exited with a non-timeout error
	codePhaseTimeout  = "timeout"   // Watchdog killed Aider after silence period
)

type WorkflowReport struct {
	SessionID      string          `json:"sessionId,omitempty"`
	Task           string          `json:"task"`
	Target         string          `json:"target"`
	Mode           string          `json:"mode"`
	Version        string          `json:"version"`
	StartedAt      string          `json:"startedAt"`
	CompletedAt    string          `json:"completedAt"`
	Agents         []AgentResult   `json:"agents"`
	Validation     map[string]bool `json:"validation"`
	Next           []string        `json:"next"`
	Patch          *PatchAnalysis  `json:"patch,omitempty"`
	// Code phase integrity fields (Issue 5)
	FilesChanged   []string `json:"filesChanged"`   // actual git diff --name-only HEAD
	ChangeStatus   string   `json:"changeStatus"`   // success / no_change / failed / timeout
	TimeoutOccurred bool    `json:"timeoutOccurred"` // true when watchdog killed Aider
	AiderExitCode  int      `json:"aiderExitCode"`  // raw process exit code (0 = clean exit)
}

func runDoctor() {
	tools := []string{"git", "aider", "ollama", "dotnet", "node", "npm", "code", "go"}
	for _, t := range tools {
		if _, e := exec.LookPath(t); e == nil {
			fmt.Printf("[OK]   %-10s found\n", t)
		} else {
			fmt.Printf("[WARN] %-10s not found\n", t)
		}
	}
	if e := ensureOllama(); e != nil {
		fmt.Println("[WARN] Ollama API not reachable:", e)
	} else {
		fmt.Println("[OK]   Ollama API reachable")
		fmt.Println("\n[MERA] Model availability:")
		checkModelsForDoctor()
		printLargeRepoRecommendations()
	}
}

// printLargeRepoRecommendations shows optional model upgrade suggestions when running
// in single-model (Minimal) mode. These are recommendations only — not failures.
func printLargeRepoRecommendations() {
	mc := loadModelConfig()
	if modelModeLabel(mc) == "Multi-model" {
		return // already using distinct models per role — no recommendation needed
	}
	fmt.Println()
	fmt.Println("[MERA] Recommendations for large repo / real-project work:")
	fmt.Println("[INFO]   Current install uses a single small model for all roles.")
	fmt.Println("[INFO]   For better File Scout accuracy on large codebases, consider:")
	fmt.Println("[INFO]     ollama pull phi4")
	fmt.Println("[INFO]     ollama pull llama3.1:8b")
	fmt.Println("[INFO]   Then upgrade the profile:")
	fmt.Println("[INFO]     mera -SetProfile NORMAL")
	fmt.Println("[INFO]   This is a recommendation only — Minimal install continues to work.")
}

// orchestrate runs the full MERA workflow:
//
//	Analysis agents → Confidence gates → Code (Aider) → Diff review + verdict → Validation
func orchestrate(target, task, mode string, mayCode bool) error {
	defer func() {
		if r := recover(); r != nil {
			appendMeraLog("ERROR", fmt.Sprintf("orchestrate panic: %v", r))
			stopHeartbeat()
			releaseSessionLock()
			fmt.Fprintf(os.Stderr, "\n[FAIL] MERA panicked: %v\n[FAIL] Session lock released. Run 'mera -Replay' to inspect the partial session.\n", r)
			os.Exit(1)
		}
	}()

	if e := ensureGitRepo(false); e != nil {
		return e
	}
	_ = createSnapshot()

	sess := beginSession(mode, target, mode, task)
	setupAbortHandler()

	rep := WorkflowReport{
		SessionID:  sess.ID,
		Task:       task,
		Target:     target,
		Mode:       mode,
		Version:    BuildVersion,
		StartedAt:  time.Now().Format(time.RFC3339),
		Validation: map[string]bool{},
	}

	ps := getProfileSettings()
	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA] Workflow: %s | Target: %s | Profile: %s\n", strings.ToUpper(mode), target, activeProfile())
	fmt.Printf("[MERA] Task: %s\n", task)
	fmt.Println("[MERA] ========================================")
	if ps.ExtraGating {
		fmt.Println("[MERA] STRICT mode: all soft gates require explicit acknowledgement.")
	}

	// ── Analysis phase ──────────────────────────────────────────────────
	sessionBeginPhase("Planner")
	planner := plannerAgent(target, task)
	sessionEndPhase("Planner", planner.Status, planner.Model)
	rep.Agents = append(rep.Agents, planner)
	printAgentSummary(planner)

	sessionBeginPhase("Architect")
	architect := architectAgent(target, task)
	sessionEndPhase("Architect", architect.Status, architect.Model)
	rep.Agents = append(rep.Agents, architect)
	printAgentSummary(architect)

	sessionBeginPhase("File Scout")
	scout := fileScoutAgent(target, task)
	sessionEndPhase("File Scout", scout.Status, scout.Model)
	rep.Agents = append(rep.Agents, scout)
	printAgentSummary(scout)

	sessionBeginPhase("Security")
	security := securityAgent(target, task)
	sessionEndPhase("Security", security.Status, security.Model)
	rep.Agents = append(rep.Agents, security)
	printAgentSummary(security)

	if len(security.Risks) > 0 {
		fmt.Println("\n[MERA] Security flags:")
		for _, r := range security.Risks {
			fmt.Println("  [!]", r)
		}
	}
	// Print evidence report from File Scout.
	if len(scout.Evidence) > 0 {
		printEvidenceReport(scout.Evidence)
	} else if len(scout.Files) > 0 {
		fmt.Println("\n[MERA] Target files:")
		for _, f := range scout.Files {
			rel, _ := filepath.Rel(root(), f)
			fmt.Println("  ->", rel)
		}
	}

	analysisAgents := []AgentResult{planner, architect, scout, security}

	// ── Confidence gates ─────────────────────────────────────────────────
	gates := runConfidenceGates(target, analysisAgents)
	printGateReport(gates)

	// Record gate results in the validation map.
	for _, g := range gates {
		rep.Validation["gate:"+g.Name] = g.Passed
	}

	if mayCode {
		if err := enforceGates(gates); err != nil {
			rep.Next = append(rep.Next, "Gate check failed: "+err.Error())
			rep.CompletedAt = time.Now().Format(time.RFC3339)
			writeReport(rep, "workflow")
			return err
		}
	} else {
		// In analyze mode show gates as informational only.
		score := confidenceScore(gates)
		fmt.Printf("\n[MERA] Analysis complete. Confidence: %d%%\n", score)
		fmt.Printf("[MERA] To implement: mera -Code %s %q\n", target, task)
		rep.CompletedAt = time.Now().Format(time.RFC3339)
		writeReport(rep, "workflow")
		return nil
	}

	// ── File approval ────────────────────────────────────────────────────
	// User reviews evidence and approves, edits, or cancels before Aider launches.
	evidence, truncated := enforceFileLimit(scout.Evidence)
	if truncated {
		fmt.Printf("[WARN] File count limited to %d — approve the highest-confidence selection.\n", maxFilesForAider)
	}

	approvedPaths, approvalErr := approveFiles(evidence)
	if approvalErr != nil {
		rep.Next = append(rep.Next, "File approval cancelled: "+approvalErr.Error())
		rep.CompletedAt = time.Now().Format(time.RFC3339)
		writeReport(rep, "workflow")
		return approvalErr
	}

	// ── Minimal model guard ──────────────────────────────────────────────
	// Block -Code if the code model is qwen2.5-coder:7b and the repo is large.
	if guardErr := checkMinimalModelGuard(target); guardErr != nil {
		fmt.Println()
		fmt.Println(guardErr.Error())
		return guardErr
	}

	// ── Code phase ───────────────────────────────────────────────────────
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Starting code phase...")

	sessionBeginPhase("Code (Aider)")
	aiderErr := runAider(mode, target, task, mode, true, approvedPaths, analysisAgents)

	// ── Determine outcome status (Issues 2 & 3) ──────────────────────────
	// Precedence: timeout > failed > no_change > success.
	// Only call changedFiles() once — single source of truth for the whole workflow.
	changed := changedFiles()
	rep.FilesChanged = changed

	isTimeout := errors.Is(aiderErr, ErrAiderSilenceTimeout)
	isBlocked := errors.Is(aiderErr, ErrAiderBlocked)
	rep.TimeoutOccurred = isTimeout

	var aiderExitCode int
	var changeStatus string
	var codeAgentOut string
	switch {
	case isBlocked:
		// Phase 10.13: BLOCKED_INTERACTIVE is a distinct failure mode.
		// Aider was alive and producing output but waiting for stdin input
		// that will never arrive in headless mode. This is a configuration
		// problem (missing --message / --yes flags), not a model timeout.
		changeStatus = codePhaseFailed
		codeAgentOut = "Aider blocked on an interactive prompt (BLOCKED_INTERACTIVE). " +
			"Check .mera/logs/aider-session.log for the blocking line. " +
			"Run: mera -AiderSmoke to diagnose headless mode."
	case isTimeout:
		changeStatus = codePhaseTimeout
		codeAgentOut = "Aider watchdog terminated after 120s of silence."
	case aiderErr != nil:
		changeStatus = codePhaseFailed
		codeAgentOut = aiderErr.Error()
		// Attempt to extract process exit code from exec.ExitError.
		var exitErr interface{ ExitCode() int }
		if errors.As(aiderErr, &exitErr) {
			aiderExitCode = exitErr.ExitCode()
		} else {
			aiderExitCode = -1
		}
	case len(changed) == 0:
		changeStatus = codePhaseNoChange
		codeAgentOut = "Aider exited cleanly but produced no file changes."
	default:
		changeStatus = codePhaseSuccess
		codeAgentOut = fmt.Sprintf("Aider completed — %d file(s) changed.", len(changed))
	}
	rep.ChangeStatus = changeStatus
	rep.AiderExitCode = aiderExitCode

	sessionEndPhase("Code (Aider)", changeStatus, modelForRole(RoleCode))
	rep.Agents = append(rep.Agents, AgentResult{
		Agent:  "Code Agent",
		Status: changeStatus,
		Output: codeAgentOut,
		Files:  changed,
	})

	// ── Patch evidence (Issues 1 & 7) ────────────────────────────────────
	// Always print what changed (or didn't) so the terminal is never ambiguous.
	fmt.Println("\n[MERA] ========================================")
	switch changeStatus {
	case codePhaseSuccess:
		fmt.Printf("[MERA] Modified files (%d):\n", len(changed))
		for _, f := range changed {
			fmt.Println("  -", f)
		}
	case codePhaseNoChange:
		fmt.Println("[FAIL] No code changes were produced by Aider.")
		fmt.Println("[MERA] Possible causes:")
		fmt.Println("       - Task description too vague or model misunderstood context")
		fmt.Println("       - Aider made edits but then reverted them")
		fmt.Println("       - The change may already be implemented")
		fmt.Println("[MERA] Try:")
		fmt.Println("       mera -Plan <module> \"task\"   # re-analyze and refine")
		fmt.Println("       mera -Code <module> \"more specific task description\"")
	case codePhaseTimeout:
		fmt.Println("[FAIL] Code phase timed out — watchdog killed Aider.")
		fmt.Println("[MERA] Retry options:")
		fmt.Println("       mera -Replay                 # same context, fresh session")
		fmt.Println("       mera -Fast <module> \"task\"   # smaller model, faster response")
	case codePhaseFailed:
		if isBlocked {
			fmt.Println("[FAIL] BLOCKED_INTERACTIVE — Aider halted waiting for user input in headless mode.")
			fmt.Println("[MERA] This is a pipeline configuration error, not a model failure.")
			fmt.Println("[MERA] Diagnosis:")
			fmt.Printf("[MERA]   Log: %s\n", aiderLogPath())
			fmt.Println("[MERA]   Run: mera -AiderSmoke   # verify headless pipeline end-to-end")
		} else {
			fmt.Println("[FAIL] Aider exited with an error:", codeAgentOut)
		}
	}

	// For anything other than a successful change, record and exit now.
	// Do NOT run validation, patch safety, or diff review — that would be false success.
	if changeStatus != codePhaseSuccess {
		rep.Next = append(rep.Next, fmt.Sprintf("Code phase outcome: %s — no validation run.", changeStatus))
		rep.CompletedAt = time.Now().Format(time.RFC3339)
		writeReport(rep, "workflow")
		closeSession("Code phase " + changeStatus)
		if changeStatus == codePhaseNoChange {
			return fmt.Errorf("code phase produced no changes — task may need refinement")
		}
		return aiderErr
	}

	// ── Patch safety gate (only reached on codePhaseSuccess) ─────────────
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Running patch safety analysis...")
	sessionBeginPhase("Patch Safety")
	patch := analyzeDiff(target, task, approvedPaths)
	rep.Patch = &patch
	if patchErr := enforcePatchSafety(&patch); patchErr != nil {
		sessionEndPhase("Patch Safety", "blocked", "")
		rep.Next = append(rep.Next, "Patch safety blocked: "+patchErr.Error())
		rep.Next = append(rep.Next, "Run: mera -Rollback to undo changes.")
		rep.CompletedAt = time.Now().Format(time.RFC3339)
		writeReport(rep, "workflow")
		closeSession("Patch safety blocked: " + patchErr.Error())
		return patchErr
	}
	sessionEndPhase("Patch Safety", "passed", "")

	// ── Diff review + verdict enforcement ────────────────────────────────
	fmt.Println("\n[MERA] ========================================")
	sessionBeginPhase("Diff Review")
	diffReview := diffReviewAgent(target, task)
	sessionEndPhase("Diff Review", diffReview.Status, diffReview.Model)
	rep.Agents = append(rep.Agents, diffReview)
	printAgentSummary(diffReview)

	if err := enforceVerdict(diffReview); err != nil {
		rep.Next = append(rep.Next, "Diff verdict blocked validation: "+err.Error())
		rep.Next = append(rep.Next, "Run: mera -Rollback to undo changes.")
		rep.CompletedAt = time.Now().Format(time.RFC3339)
		writeReport(rep, "workflow")
		closeSession("Diff verdict blocked: " + err.Error())
		return err
	}

	// ── Post-code agents ─────────────────────────────────────────────────
	qa := qaAgent(target, task)
	rep.Agents = append(rep.Agents, qa)

	review := reviewAgent(target, task)
	rep.Agents = append(rep.Agents, review)
	printAgentSummary(review)

	// ── Validation phase ─────────────────────────────────────────────────
	validationPassed := false
	verdict := extractVerdictFromAgents(rep.Agents)

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Running validation pipeline...")
	validationPassed = runValidation(target, &rep)
	if validationPassed {
		rep.Next = append(rep.Next, "All validation passed. Review diff with `git diff`, smoke test, then commit manually.")
	} else {
		rep.Next = append(rep.Next, "Validation failed. Fix errors before committing.")
	}

	// ── Record outcome in project memory ─────────────────────────────────
	gateScore := confidenceScore(gates)
	recordOutcome(target, task, mode, scout.Files, changed, verdict, gateScore, validationPassed)

	// ── Sprint recommendations (skipped in FAST profile) ────────────────────
	if validationPassed && verdict == "APPROVE" && !ps.SkipSprint {
		fmt.Println("\n[MERA] ========================================")
		fmt.Println("[MERA] Generating sprint recommendations...")
		suggestions := suggestNextTasks(target, task, changed)
		if suggestions != "" {
			fmt.Println("\n[MERA] Suggested follow-up tasks:")
			fmt.Println(suggestions)
			rep.Next = append(rep.Next, "--- Sprint suggestions ---")
			for _, line := range strings.Split(suggestions, "\n") {
				if strings.TrimSpace(line) != "" {
					rep.Next = append(rep.Next, line)
				}
			}
		}
	}

	rep.CompletedAt = time.Now().Format(time.RFC3339)
	path := writeReport(rep, "workflow")
	fmt.Println("\n[MERA] Workflow report:", path)
	fmt.Println("\n[MERA] Next steps:")
	for i, n := range rep.Next {
		fmt.Printf("  %d. %s\n", i+1, n)
	}

	closeSession("")
	return nil
}

// dryRun runs the full analysis + gate pipeline without launching Aider.
// Produces a mission brief and a GO / CAUTION / NO-GO recommendation.
func dryRun(target, task string) error {
	mc := loadModelConfig()
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  DRY RUN — Analysis only, no code changes")
	fmt.Printf("[MERA]  Target         : %s\n", target)
	fmt.Printf("[MERA]  Task           : %s\n", task)
	fmt.Printf("[MERA]  Runtime profile: %s\n", mc.Profile)
	fmt.Printf("[MERA]  Model mode     : %s\n", modelModeLabel(mc))
	fmt.Println("[MERA] ========================================")

	// Full analysis phase.
	planner := plannerAgent(target, task)
	printAgentSummary(planner)

	architect := architectAgent(target, task)
	printAgentSummary(architect)

	scout := fileScoutAgent(target, task)
	printAgentSummary(scout)

	security := securityAgent(target, task)
	printAgentSummary(security)

	agents := []AgentResult{planner, architect, scout, security}

	// Gate report.
	gates := runConfidenceGates(target, agents)
	printGateReport(gates)

	// Build the session document Aider would receive — write it for inspection.
	sessionPath, err := buildSession("dryrun", target, task, "normal", true, agents)
	if err != nil {
		fmt.Println("[WARN] Could not write session document:", err)
	} else {
		fmt.Println("\n[MERA] Aider briefing written (inspect before running -Code):")
		fmt.Println("       ", sessionPath)
	}

	// Print the files that would be modified.
	if len(scout.Files) > 0 {
		fmt.Println("\n[MERA] Files that WOULD be passed to Aider:")
		for _, f := range scout.Files {
			rel, _ := filepath.Rel(root(), f)
			fmt.Println("  ->", rel)
		}
	}

	// Final recommendation.
	score := confidenceScore(gates)
	var blockingGate *GateResult
	for i := range gates {
		if !gates[i].Passed && gates[i].Hard {
			blockingGate = &gates[i]
			break
		}
	}

	fmt.Println("\n[MERA] ========================================")
	switch {
	case blockingGate != nil:
		fmt.Printf("[MERA]  Recommendation: NO-GO FAIL\n")
		fmt.Printf("[MERA]  Blocked by gate: %s\n", blockingGate.Name)
		fmt.Printf("[MERA]  Reason: %s\n", blockingGate.Reason)
		fmt.Println("[MERA] ----------------------------------------")
		// Actionable next steps based on which gate blocked.
		switch blockingGate.Name {
		case "File Confidence", "File Discovery":
			fmt.Println("[MERA]  The file selection was too uncertain. Try a more specific task description.")
			fmt.Println("[MERA]  Inspect scoring details:")
			fmt.Printf("[MERA]    mera -ExplainSelection %s %q\n", target, task)
			fmt.Println("[MERA]  Or refine the task with explicit file cues, e.g.:")
			refinedTask := suggestRefinedTask(target, task, scout)
			fmt.Printf("[MERA]    mera -DryRun %s %q\n", target, refinedTask)
		case "Boundary Check":
			fmt.Println("[MERA]  Files selected are outside the declared module boundary.")
			fmt.Println("[MERA]  Check: mera -Init  to review module boundaries.")
			fmt.Printf("[MERA]  Inspect selection: mera -ExplainSelection %s %q\n", target, task)
		default:
			fmt.Printf("[MERA]  Inspect selection: mera -ExplainSelection %s %q\n", target, task)
			fmt.Println("[MERA]  Resolve the gate failure above, then retry.")
		}
	case score >= 80:
		fmt.Printf("[MERA]  Recommendation: GO OK  (confidence %d%%)\n", score)
		fmt.Printf("[MERA]  Next: mera -Code %s %q\n", target, task)
	case score >= 60:
		fmt.Printf("[MERA]  Recommendation: CAUTION [!]  (confidence %d%%)\n", score)
		fmt.Printf("[MERA]  Review warnings, then: mera -Code %s %q\n", target, task)
	default:
		fmt.Printf("[MERA]  Recommendation: NO-GO FAIL  (confidence %d%%)\n", score)
		fmt.Println("[MERA]  Address failures before proceeding.")
		fmt.Printf("[MERA]  Inspect selection: mera -ExplainSelection %s %q\n", target, task)
	}
	fmt.Println("[MERA] ========================================")

	// Persist the dry-run report.
	rep := WorkflowReport{
		Task:        task,
		Target:      target,
		Mode:        "dryrun",
		Version:     BuildVersion,
		StartedAt:   time.Now().Format(time.RFC3339),
		CompletedAt: time.Now().Format(time.RFC3339),
		Agents:      agents,
		Validation:  map[string]bool{},
	}
	for _, g := range gates {
		rep.Validation["gate:"+g.Name] = g.Passed
	}
	path := writeReport(rep, "dryrun")
	fmt.Println("[MERA] Dry-run report:", path)

	return nil
}

// explainSelection runs File Scout with full evidence reporting and no code changes.
// Shows why each file was selected, historical influence, confidence scores, and exclusions.
func explainSelection(target, task string) error {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Explain Selection")
	fmt.Printf("[MERA]  Target: %s\n[MERA]  Task:   %s\n", target, task)
	fmt.Println("[MERA] ========================================")

	// Print indexing metadata before running the scout.
	fmt.Printf("[MERA] Index schema version : %d\n", repoIndexVersion)
	fmt.Printf("[MERA] Index path           : %s\n", repoIndexPath())

	// Count total repo files so we can report how many were excluded.
	allFiles := allRepoFiles()
	totalRepo := len(allFiles)

	scout := fileScoutAgent(target, task)

	if len(scout.Evidence) == 0 {
		fmt.Println("[MERA] File Scout found no files to explain.")
		fmt.Printf("[MERA] Repo contains %d indexed files — check 'mera -Diag' for index details.\n", totalRepo)
		return nil
	}

	// Exclusion summary.
	excluded := totalRepo - len(scout.Evidence)
	if excluded < 0 {
		excluded = 0
	}
	fmt.Printf("[MERA] Repo: %d files indexed, %d excluded from selection, %d shown below.\n",
		totalRepo, excluded, len(scout.Evidence))

	// Intent + scope classification — shown before the evidence table.
	intent := classifyTaskIntent(task)
	scope := classifyTaskScope(task)
	fmt.Printf("[MERA] Task intent          : %s\n", intent)
	fmt.Printf("[MERA] Task scope           : %s\n", scope)

	// Classification breakdown.
	backend, frontend, test := 0, 0, 0
	for _, ev := range scout.Evidence {
		switch classifyFile(ev.RelPath) {
		case "frontend":
			frontend++
		case "test":
			test++
		default:
			backend++
		}
	}
	fmt.Printf("[MERA] Classification: %d backend  %d frontend  %d test\n", backend, frontend, test)
	fmt.Println()

	printEvidenceReport(scout.Evidence)

	// Rejected candidates — only printed for BUGFIX_NARROW tasks.
	if len(scout.RejectedCandidates) > 0 {
		fmt.Println("\n[MERA] Excluded sibling candidates (scored but not selected):")
		for _, rc := range scout.RejectedCandidates {
			fmt.Printf("  [EXCL] %-65s  %3d%%  — %s\n", rc.RelPath, rc.Score, rc.Reason)
		}
	}

	// Historical influence summary.
	profile := loadProfile()
	if ts, ok := profile.TargetStats[target]; ok && ts.RunCount > 0 {
		rate := float64(ts.SuccessCount) / float64(ts.RunCount) * 100
		fmt.Println("\n[MERA] Historical Influence:")
		fmt.Printf("  Target %s: %d run(s), %.1f%% success rate, avg confidence %.0f%%\n",
			target, ts.RunCount, rate, ts.AvgConfidence)
		if len(ts.CommonFiles) > 0 {
			fmt.Println("  Commonly confirmed files:")
			for _, f := range ts.CommonFiles {
				fmt.Println("    ->", f)
			}
		}
	} else {
		fmt.Println("\n[MERA] No historical data for target:", target)
	}

	// Confidence summary.
	if len(scout.Evidence) > 0 {
		max, total := 0, 0
		for _, ev := range scout.Evidence {
			total += ev.Confidence
			if ev.Confidence > max {
				max = ev.Confidence
			}
		}
		avg := total / len(scout.Evidence)
		fmt.Printf("\n[MERA] Confidence summary: avg %d%%  max %d%%\n", avg, max)
		switch {
		case max < lowConfidenceThreshold:
			fmt.Printf("[WARN] Low confidence (%d%%) — consider refining the task before running -Code.\n", max)
		case avg < midConfidenceThreshold:
			fmt.Printf("[WARN] Medium confidence (%d%%) — review selection carefully.\n", avg)
		default:
			fmt.Printf("[OK]   Confidence looks good — ready for: mera -Code %s %q\n", target, task)
		}
	}

	fmt.Println("[MERA] ========================================")
	return nil
}

// explainDiff analyses the current working-tree diff without a MERA session context.
// Prints a full patch safety report so the user can understand what changed and why it was flagged.
func explainDiff(target, task string) error {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Explain Diff")
	fmt.Printf("[MERA]  Target: %s\n[MERA]  Task:   %s\n", target, task)
	fmt.Println("[MERA] ========================================")

	analysis := analyzeDiff(target, task, nil)

	if len(analysis.ChangedFiles) == 0 {
		fmt.Println("[MERA] No changed files found in working tree.")
		return nil
	}

	printPatchReport(analysis)
	return nil
}

// writePlan runs analysis agents and writes a structured execution plan to markdown.
func writePlan(target, task string) error {
	fmt.Println("[MERA] Generating execution plan...")

	planner := plannerAgent(target, task)
	architect := architectAgent(target, task)
	scout := fileScoutAgent(target, task)
	p := detectProject()

	var sb strings.Builder
	sb.WriteString("# MERA Execution Plan\n\n")
	sb.WriteString(fmt.Sprintf("**Target:** %s  \n**Task:** %s  \n**Generated:** %s\n\n",
		target, task, time.Now().Format(time.RFC3339)))

	sb.WriteString("## Implementation Steps\n\n")
	sb.WriteString(planner.Output + "\n\n")

	sb.WriteString("## Architecture Impact\n\n")
	sb.WriteString(architect.Output + "\n\n")

	sb.WriteString("## Files to Modify\n\n")
	if len(scout.Files) > 0 {
		for _, f := range scout.Files {
			sb.WriteString("- `" + f + "`\n")
		}
	} else {
		sb.WriteString("_File Scout could not identify specific files. Use `-Code` and let Aider discover them._\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Validation Commands\n\n")
	sb.WriteString(fmt.Sprintf("```\n%s\n%s\n%s\n```\n\n", p.Build, p.Test, p.FrontendBuild))

	sb.WriteString("## Next Action\n\n")
	sb.WriteString(fmt.Sprintf("```powershell\nmera -DryRun %s %q   # inspect gates first\nmera -Code %s %q\n```\n",
		target, task, target, task))

	path := filepath.Join(reportsDir(), "EXECUTION_PLAN.md")
	if e := writeNoBOM(path, []byte(sb.String())); e != nil {
		return e
	}
	fmt.Println("[OK] Plan written:", path)
	fmt.Println("\n[MERA] Implementation steps:")
	fmt.Println(planner.Output)
	return nil
}

func missionWizard() error {
	target := promptLine("Target module")
	role := promptLine("Your role")
	goal := promptLine("Goal / task")
	m := map[string]string{
		"id":        "mission-" + time.Now().Format("20060102-150405"),
		"target":    target,
		"role":      role,
		"goal":      goal,
		"createdAt": time.Now().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	path := filepath.Join(sessionsDir(), m["id"]+".json")
	if e := writeNoBOM(path, b); e != nil {
		return e
	}
	fmt.Println("[OK] Mission captured:", path)
	fmt.Printf("[MERA] Suggested: mera -DryRun %s %q\n", target, goal)
	return nil
}

// extractVerdictFromAgents finds the verdict from the Diff Review agent result.
func extractVerdictFromAgents(agents []AgentResult) string {
	for _, a := range agents {
		if a.Agent == "Diff Review" {
			return extractVerdict(a.Output)
		}
	}
	return ""
}

func writeReport(rep WorkflowReport, kind string) string {
	b, _ := json.MarshalIndent(rep, "", "  ")
	path := filepath.Join(reportsDir(), time.Now().Format("20060102-150405")+"-"+kind+"-report.json")
	_ = writeNoBOM(path, b)
	return path
}

func printAgentSummary(a AgentResult) {
	tag := strings.ToUpper(a.Status)
	if tag == "COMPLETED" {
		tag = "OK"
	}
	fmt.Printf("[%-8s] %-14s %s\n", tag, a.Agent+":", truncate(a.Output, 100))
}

// suggestRefinedTask builds a more specific task string when File Confidence is low.
// It adds evidence-based cues from the scout's top file (controller/route name etc.)
// to help the user re-run with higher scoring.
func suggestRefinedTask(target, task string, scout AgentResult) string {
	taskLower := strings.ToLower(task)

	// Auth / login pattern — suggest the most specific form.
	if strings.Contains(taskLower, "login") || strings.Contains(taskLower, "auth") {
		return fmt.Sprintf("Fix POST /api/v%s/%s/login Bad Request — check AuthController model binding and LoginRequest DTO",
			"1", strings.ToLower(target))
	}
	if strings.Contains(taskLower, "register") {
		return fmt.Sprintf("Fix POST /api/v%s/%s/register — validate RegisterRequest DTO and AuthController action",
			"1", strings.ToLower(target))
	}
	if strings.Contains(taskLower, "token") || strings.Contains(taskLower, "refresh") {
		return fmt.Sprintf("Fix token refresh in %s — check RefreshToken endpoint and JWT validation logic", target)
	}

	// If we got some evidence, anchor the task to the top file.
	if len(scout.Evidence) > 0 {
		topFile := filepath.Base(scout.Evidence[0].RelPath)
		return fmt.Sprintf("%s — focus on %s", task, topFile)
	}
	return task + " — add specific file name or endpoint path to improve targeting"
}
