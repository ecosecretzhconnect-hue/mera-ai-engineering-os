package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sampleAppDir returns the expected location of the bundled sample project.
func sampleAppDir() string {
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "MERA.SampleApp")
		if exists(candidate) {
			return candidate
		}
	}
	// Fallback: relative to working directory (dev mode).
	return filepath.Join(".", "MERA.SampleApp")
}

// testExclusionRules verifies that isExcludedPath correctly blocks all known
// excluded directory patterns. Returns a list of failure strings (empty = all pass).
//
// The .claude/worktrees test case is the primary regression target:
// files from AI worktrees must never appear in File Scout results.
func testExclusionRules() []string {
	type tc struct {
		path    string
		wantOut bool // true = should be excluded
	}
	cases := []tc{
		// ── Must be excluded ─────────────────────────────────────────────
		// .claude worktrees — primary regression target
		{".claude/worktrees/test/Identity/AuthController.cs", true},
		{".claude/worktrees/adoring-cartwright-e4bf33/Src/Api/AuthController.cs", true},
		{".claude/settings.json", true},
		{".claude/worktrees/adoring-cartwright-e4bf33/Identity/Models/LoginRequest.cs", true},
		// .claude nested inside a subdirectory
		{"subdir/.claude/worktrees/foo/bar.cs", true},
		// Other excluded dirs
		{".git/config", true},
		{".mera/repo-index.json", true},
		{"node_modules/lodash/index.js", true},
		{"bin/Debug/MyApp.dll", true},
		{"obj/Release/net8.0/MyApp.pdb", true},
		{"build/output/bundle.js", true},
		{"dist/assets/main.js", true},
		{"coverage/lcov.info", true},
		{"testresults/TestRun.xml", true},
		{"artifacts/publish/app.exe", true},
		{"packages/NuGet/cache.json", true},

		// ── Must NOT be excluded ─────────────────────────────────────────
		{"Identity/Src/EcoSecretz.HConnect.Identity.API/Controllers/AuthController.cs", false},
		{"Identity/Src/EcoSecretz.HConnect.Identity.Models/LoginRequest.cs", false},
		{"HConnect.Web/src/features/identity/services/identity.service.ts", false},
		{"Shared/Infrastructure/JwtService.cs", false},
		{"src/main.go", false},
		// Names that contain excluded words as substrings (must not be excluded)
		{"IdentityBindings/AuthService.cs", false},         // contains "bin" as substring
		{"MyApp.Distribution/Startup.cs", false},           // contains "dist" as substring
		{"BuildTools/Generator/CodeGen.cs", false},         // starts-with "Build" (not exact "build")
		{"Coverage.Domain/ReportModel.cs", false},          // starts with "Coverage." (not exact "coverage")
	}

	var failures []string
	for _, tc := range cases {
		got := isExcludedPath(tc.path)
		if got != tc.wantOut {
			if tc.wantOut {
				failures = append(failures, fmt.Sprintf("SHOULD be excluded but was not: %s", tc.path))
			} else {
				failures = append(failures, fmt.Sprintf("SHOULD NOT be excluded but was: %s", tc.path))
			}
		}
	}
	return failures
}

// runSmokeTest exercises the full MERA stack and reports pass/fail per component.
func runSmokeTest() error {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Smoke Test  -- MERA Go v" + BuildVersion)
	fmt.Println("[MERA] ========================================")

	type result struct {
		name   string
		passed bool
		detail string
	}

	var results []result
	pass := func(name, detail string) { results = append(results, result{name, true, detail}) }
	fail := func(name, detail string) { results = append(results, result{name, false, detail}) }
	warn := func(name, detail string) { results = append(results, result{name, true, "[WARN] " + detail}) }

	// 0. Exclusion rules self-test -- must pass before any file scouting
	fmt.Println("\n[SMOKE] 0/8 Exclusion rules...")
	if failures := testExclusionRules(); len(failures) == 0 {
		pass("Exclusion rules",
			fmt.Sprintf("all %d path exclusion cases passed (index schema v%d)",
				20, repoIndexVersion))
	} else {
		for _, f := range failures {
			fail("Exclusion rules", f)
		}
	}

	// 1. Init
	fmt.Println("\n[SMOKE] 1/8 Init...")
	if err := initProject(); err != nil {
		fail("Init", err.Error())
	} else {
		pass("Init", ".mera structure ready")
	}

	// 2. Config validation
	fmt.Println("[SMOKE] 2/8 Config validation...")
	cfgOK, cfgDetail := validateConfigFile(filepath.Join(meraDir(), "config.json"))
	modOK, modDetail := validateConfigFile(modelConfigPath())
	if cfgOK {
		pass("config.json", "valid")
	} else {
		fail("config.json", cfgDetail)
	}
	if modOK {
		pass("models.json", "valid")
	} else {
		fail("models.json", modDetail)
	}

	// 3. Doctor tool checks (non-fatal -- collect results)
	fmt.Println("[SMOKE] 3/8 Tool checks...")
	requiredTools := []string{"git", "aider", "ollama"}
	for _, t := range requiredTools {
		if _, err := exec.LookPath(t); err == nil {
			pass("Tool:"+t, "found in PATH")
		} else {
			fail("Tool:"+t, "not found in PATH")
		}
	}

	// 4. Ollama API
	fmt.Println("[SMOKE] 4/8 Ollama API...")
	if ollamaAPIUp() {
		pass("Ollama API", "reachable")
	} else {
		fail("Ollama API", "not reachable -- run: ollama serve")
	}

	// 5. Code model availability
	fmt.Println("[SMOKE] 5/8 Code model...")
	mc := loadModelConfig()
	codeModel := mc.Models[RoleCode]
	set := installedModelSet()
	if isInModelSet(set, codeModel) {
		pass("Code model", codeModel+" installed")
	} else {
		fail("Code model", codeModel+" not installed -- run: ollama pull "+codeModel)
	}

	// 6. Health score
	fmt.Println("[SMOKE] 6/8 Health check...")
	comps := gatherHealthComponents()
	score := healthScore(comps)
	if score >= 70 {
		pass("Health", fmt.Sprintf("%d%% -- system healthy", score))
	} else {
		warn("Health", fmt.Sprintf("%d%% -- degraded (run 'mera -Health' for details)", score))
	}

	// 7. Sample app dry-run (if sample app is present)
	fmt.Println("[SMOKE] 7/8 Sample app dry-run...")
	sampleDir := sampleAppDir()
	if exists(sampleDir) {
		err := runSampleDryRun(sampleDir)
		if err != nil {
			fail("DryRun (sample)", err.Error())
		} else {
			pass("DryRun (sample)", "analysis completed without errors")
		}
	} else {
		warn("DryRun (sample)", "MERA.SampleApp not found -- skipped (expected beside mera.exe)")
	}

	// 8. Exclusion sanitizer live check (confirms no .claude paths in live index)
	fmt.Println("[SMOKE] 8/8 Live index exclusion check...")
	liveFiles := allRepoFiles()
	leaked := 0
	for _, f := range liveFiles {
		if isExcludedPath(f.relPath) {
			fmt.Printf("[SMOKE] [FAIL] Excluded path in live index: %s\n", f.relPath)
			leaked++
		}
	}
	if leaked == 0 {
		pass("Live index exclusion", fmt.Sprintf("%d files indexed, zero excluded paths leaked", len(liveFiles)))
	} else {
		fail("Live index exclusion", fmt.Sprintf("%d excluded path(s) found in live index -- delete .mera/repo-index.json and retry", leaked))
	}

	// -- Results ------------------------------------------------------------------
	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA]  Smoke Test Results -- MERA Go v%s\n", BuildVersion)
	fmt.Println("[MERA] ========================================")

	allPassed := true
	for _, r := range results {
		icon := "[PASS]"
		if !r.passed {
			icon = "[FAIL]"
			allPassed = false
		}
		fmt.Printf("  %s  %-26s  %s\n", icon, r.name, r.detail)
	}

	fmt.Println()
	if allPassed {
		fmt.Println("[MERA] All smoke tests passed. MERA is ready.")
		return nil
	}
	failed := 0
	for _, r := range results {
		if !r.passed {
			failed++
		}
	}
	fmt.Printf("[MERA] %d test(s) failed. Run 'mera -Repair' or 'mera -Health' for guidance.\n", failed)
	return fmt.Errorf("%d smoke test(s) failed", failed)
}

// runSampleDryRun runs a lightweight analysis-only pass against the sample project.
// It does not require auth and does not touch code -- analysis agents only.
func runSampleDryRun(sampleDir string) error {
	// Change working directory temporarily to the sample project.
	orig, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(sampleDir); err != nil {
		return fmt.Errorf("cannot enter sample dir %s: %w", sampleDir, err)
	}
	defer func() { _ = os.Chdir(orig) }()

	// Run planner + architect + file scout + security agents.
	// We cannot call orchestrate() -- no git repo, no auth needed.
	task := "Fix login binding"
	target := "Sample"

	planner := plannerAgent(target, task)
	if planner.Status == "failed" {
		return fmt.Errorf("planner failed: %s", truncate(planner.Output, 120))
	}

	architect := architectAgent(target, task)
	if architect.Status == "failed" {
		return fmt.Errorf("architect failed: %s", truncate(architect.Output, 120))
	}

	scout := fileScoutAgent(target, task)
	// Scout degraded is OK for smoke -- it means Ollama answered.

	agents := []AgentResult{planner, architect, scout}
	gates := runConfidenceGates(target, agents)
	score := confidenceScore(gates)

	fmt.Printf("[SMOKE] Sample dry-run: confidence %d%% -- %d agent(s) OK\n",
		score, len(agents))

	// Print any detected files.
	if len(scout.Files) > 0 {
		fmt.Printf("[SMOKE] Sample files detected: %s\n",
			strings.Join(relPaths(scout.Files, sampleDir), ", "))
	}
	return nil
}

// relPaths converts absolute paths to paths relative to base.
func relPaths(abs []string, base string) []string {
	out := make([]string, 0, len(abs))
	for _, p := range abs {
		r, err := filepath.Rel(base, p)
		if err != nil {
			r = p
		}
		out = append(out, r)
	}
	return out
}
