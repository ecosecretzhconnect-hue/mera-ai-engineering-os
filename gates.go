package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// GateResult is the outcome of a single pre-flight check.
type GateResult struct {
	Name   string
	Passed bool
	Reason string
	Hard   bool // Hard=true → block execution. Hard=false → warn, allow override.
}

// runConfidenceGates evaluates all pre-flight gates against the analysis agent results.
// Gates cover: file discovery, boundary safety, security risk, blast radius, agent quality.
func runConfidenceGates(target string, agents []AgentResult) []GateResult {
	var gates []GateResult

	// Gate 1: File Scout must have found at least one file.
	// Launching Aider with zero targeted files means it operates blind on the full repo.
	scout := findAgent(agents, "File Scout")
	{
		g := GateResult{Name: "File Discovery", Hard: true}
		if scout == nil || len(scout.Files) == 0 {
			g.Passed = false
			g.Reason = "File Scout found 0 files — Aider would operate blind on the full repo"
		} else {
			g.Passed = true
			g.Reason = fmt.Sprintf("%d files identified by File Scout", len(scout.Files))
		}
		gates = append(gates, g)
	}

	// Gate 2: Boundary check on the files File Scout identified.
	{
		g := GateResult{Name: "Boundary Check", Hard: true}
		var relPaths []string
		if scout != nil {
			r := root()
			for _, f := range scout.Files {
				rel, _ := filepath.Rel(r, f)
				relPaths = append(relPaths, filepath.ToSlash(rel))
			}
		}
		if !boundaryCheck(target, relPaths, false) {
			g.Passed = false
			g.Reason = "Identified files violate module boundary rules"
		} else {
			g.Passed = true
			g.Reason = "No boundary violations in identified files"
		}
		gates = append(gates, g)
	}

	// Gate 3: Security risk level from the security agent.
	{
		g := GateResult{Name: "Security Risk", Hard: false}
		sec := findAgent(agents, "Security")
		if sec != nil && strings.Contains(strings.ToUpper(sec.Output), "RISK: HIGH") {
			g.Passed = false
			g.Reason = "Security agent flagged HIGH risk — manual review required before proceeding"
		} else if sec != nil && len(sec.Risks) > 0 {
			g.Passed = false
			g.Reason = "Pattern-based risks: " + strings.Join(sec.Risks, "; ")
		} else {
			g.Passed = true
			g.Reason = "No security flags raised"
		}
		gates = append(gates, g)
	}

	// Gate 4: Blast radius of the identified files.
	{
		g := GateResult{Name: "Blast Radius", Hard: false}
		var relFiles []string
		if scout != nil {
			r := root()
			for _, f := range scout.Files {
				rel, _ := filepath.Rel(r, f)
				relFiles = append(relFiles, rel)
			}
		}
		br := blastRadius(relFiles)
		switch {
		case br >= 4:
			g.Passed = false
			g.Reason = fmt.Sprintf("Blast radius %d/4 — system-wide impact (auth, config, or gateway files)", br)
		case br >= 3:
			g.Passed = false
			g.Reason = fmt.Sprintf("Blast radius %d/4 — cross-module impact (controllers, services, routes)", br)
		default:
			g.Passed = true
			g.Reason = fmt.Sprintf("Blast radius %d/4 — contained change", br)
		}
		gates = append(gates, g)
	}

	// Gate 5: Agent quality — were any agents degraded or failed?
	{
		g := GateResult{Name: "Agent Quality", Hard: false}
		var degraded, failed int
		for _, a := range agents {
			switch a.Status {
			case "degraded":
				degraded++
			case "failed":
				failed++
			}
		}
		switch {
		case failed > 0:
			g.Passed = false
			g.Reason = fmt.Sprintf("%d agent(s) failed — analysis may be significantly incomplete", failed)
		case degraded > 0:
			g.Passed = false
			g.Reason = fmt.Sprintf("%d agent(s) degraded — analysis used fallbacks (Ollama may be slow)", degraded)
		default:
			g.Passed = true
			g.Reason = "All agents completed successfully"
		}
		gates = append(gates, g)
	}

	// Gate 6: File Confidence — block if no file has confidence above the low threshold.
	{
		g := GateResult{Name: "File Confidence", Hard: true}
		if scout == nil || len(scout.Evidence) == 0 {
			g.Passed = false
			g.Reason = "File Scout produced no evidence — cannot assess confidence"
		} else {
			maxConf, totalConf := 0, 0
			for _, ev := range scout.Evidence {
				totalConf += ev.Confidence
				if ev.Confidence > maxConf {
					maxConf = ev.Confidence
				}
			}
			avgConf := totalConf / len(scout.Evidence)
			switch {
			case maxConf < lowConfidenceThreshold:
				g.Hard = true
				g.Passed = false
				g.Reason = fmt.Sprintf("Highest file confidence %d%% < %d%% threshold — selection too uncertain to proceed", maxConf, lowConfidenceThreshold)
			case avgConf < midConfidenceThreshold:
				g.Hard = false
				g.Passed = false
				g.Reason = fmt.Sprintf("Average file confidence %d%% — review selection before proceeding", avgConf)
			default:
				g.Passed = true
				g.Reason = fmt.Sprintf("File confidence OK (avg: %d%%, max: %d%%)", avgConf, maxConf)
			}
		}
		gates = append(gates, g)
	}

	// Gate 7: Learning — check if a similar task previously failed or was rejected.
	{
		g := GateResult{Name: "Learning Check", Hard: false}
		warning := getPastFailureWarning(target, func() string {
			for _, a := range agents {
				if a.Agent == "Planner" {
					return a.Output
				}
			}
			return ""
		}())
		// Also check task string itself.
		if warning == "" {
			warning = getPastFailureWarning(target, taskText(agents))
		}
		if warning != "" {
			g.Passed = false
			g.Reason = warning
		} else {
			g.Passed = true
			g.Reason = "No similar past failures in project memory"
		}
		gates = append(gates, g)
	}

	// Hotspot warnings are informational — not a blocking gate, just appended to reasons.
	if scout != nil {
		for _, w := range getHotspotWarnings(scout.Files) {
			gates = append(gates, GateResult{
				Name:   "File Hotspot",
				Passed: false,
				Reason: w,
				Hard:   false,
			})
		}
	}

	return gates
}

// confidenceScore returns 0-100 based on passed gates.
func confidenceScore(gates []GateResult) int {
	if len(gates) == 0 {
		return 0
	}
	passed := 0
	for _, g := range gates {
		if g.Passed {
			passed++
		}
	}
	return (passed * 100) / len(gates)
}

func confidenceLabel(score int) string {
	switch {
	case score >= 80:
		return "HIGH — safe to proceed"
	case score >= 60:
		return "MEDIUM — review warnings before proceeding"
	default:
		return "LOW — address failures before proceeding"
	}
}

func printGateReport(gates []GateResult) {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Confidence Gate Report")
	fmt.Println("[MERA] ========================================")
	for _, g := range gates {
		var tag string
		switch {
		case g.Passed:
			tag = "PASS  "
		case g.Hard:
			tag = "BLOCK "
		default:
			tag = "WARN  "
		}
		fmt.Printf("  [%-6s] %-22s %s\n", tag, g.Name+":", g.Reason)
	}
	score := confidenceScore(gates)
	fmt.Printf("\n  Overall confidence: %d%% — %s\n", score, confidenceLabel(score))
	fmt.Println("[MERA] ========================================")
}

// enforceGates acts on gate results:
//   - Any Hard gate that failed → return error immediately (no override possible)
//   - Soft gate failures → print warnings, require "PROCEED" confirmation
func enforceGates(gates []GateResult) error {
	// Hard blocks first — no override.
	for _, g := range gates {
		if !g.Passed && g.Hard {
			fmt.Printf("\n[BLOCK] %s: %s\n", g.Name, g.Reason)
			fmt.Println("[MERA]  Cannot proceed. Resolve the issue and retry.")
			return fmt.Errorf("blocked by gate: %s", g.Name)
		}
	}

	// Collect soft warnings.
	var warnings []GateResult
	for _, g := range gates {
		if !g.Passed && !g.Hard {
			warnings = append(warnings, g)
		}
	}

	if len(warnings) > 0 {
		fmt.Println("\n[WARN] Gate warnings require acknowledgement:")
		for _, w := range warnings {
			fmt.Printf("  - %s: %s\n", w.Name, w.Reason)
		}
		score := confidenceScore(gates)
		fmt.Printf("\n[WARN] Confidence: %d%% — %s\n", score, confidenceLabel(score))
		answer := promptLine("\nType PROCEED to continue with warnings acknowledged, or Enter to cancel")
		if strings.ToUpper(strings.TrimSpace(answer)) != "PROCEED" {
			return errors.New("cancelled at confidence gate check")
		}
		fmt.Println("[OK]  Proceeding with warnings acknowledged.")
	}

	return nil
}

// extractVerdict parses the VERDICT line from a diff review output.
func extractVerdict(output string) string {
	for _, line := range strings.Split(output, "\n") {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if strings.Contains(upper, "VERDICT:") {
			switch {
			case strings.Contains(upper, "APPROVE"):
				return "APPROVE"
			case strings.Contains(upper, "REJECT"):
				return "REJECT"
			case strings.Contains(upper, "NEEDS_REVIEW"):
				return "NEEDS_REVIEW"
			}
		}
	}
	return ""
}

// enforceVerdict acts on the diff review verdict:
//   - APPROVE      → proceed normally
//   - NEEDS_REVIEW → warn, require "PROCEED" to continue
//   - REJECT       → block, require "OVERRIDE" to force through (or recommend rollback)
func enforceVerdict(diffReview AgentResult) error {
	verdict := extractVerdict(diffReview.Output)

	switch verdict {
	case "APPROVE":
		fmt.Println("[OK]  Diff approved by review agent — proceeding to validation.")
		return nil

	case "NEEDS_REVIEW":
		fmt.Println("\n[WARN] Review agent flagged issues that need attention before committing.")
		fmt.Println("[WARN] Review the diff output above carefully.")
		answer := promptLine("Type PROCEED to continue to validation, or Enter to cancel")
		if strings.ToUpper(strings.TrimSpace(answer)) != "PROCEED" {
			return errors.New("cancelled after NEEDS_REVIEW verdict — no validation run")
		}
		fmt.Println("[OK]  Proceeding to validation with warnings noted.")
		return nil

	case "REJECT":
		fmt.Println("\n[FAIL] Review agent REJECTED the diff.")
		fmt.Println("[MERA] Recommended action: mera -Rollback to undo all changes.")
		answer := promptLine("Type OVERRIDE to force validation anyway, or Enter to accept rollback recommendation")
		if strings.ToUpper(strings.TrimSpace(answer)) == "OVERRIDE" {
			fmt.Println("[WARN] Forcing validation despite REJECT — human takes responsibility.")
			return nil
		}
		return errors.New("diff rejected by review agent — run mera -Rollback to undo changes")

	default:
		// No parseable verdict — don't block, but flag it.
		fmt.Println("[WARN] Could not parse a verdict from the diff review. Manual review recommended before committing.")
		return nil
	}
}

// taskText extracts a short task summary from agent outputs for learning gate checks.
func taskText(agents []AgentResult) string {
	for _, a := range agents {
		if a.Agent == "Planner" && a.Output != "" {
			if len(a.Output) > 120 {
				return a.Output[:120]
			}
			return a.Output
		}
	}
	return ""
}

// findAgent returns a pointer to the first agent result matching name, or nil.
func findAgent(agents []AgentResult, name string) *AgentResult {
	for i := range agents {
		if agents[i].Agent == name {
			return &agents[i]
		}
	}
	return nil
}
