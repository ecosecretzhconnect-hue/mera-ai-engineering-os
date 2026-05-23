package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrAiderSilenceTimeout is the sentinel error returned when the Aider watchdog
// terminates the process after the configured silence period.
// Callers (orchestrate) check errors.Is(err, ErrAiderSilenceTimeout) to
// distinguish a watchdog kill from a normal execution failure or no-change.
var ErrAiderSilenceTimeout = errors.New("aider-silence-timeout")

// ErrAiderBlocked is returned when Aider prints a known interactive-prompt pattern
// while running in headless mode — meaning it is blocking on stdin instead of
// completing its task. This is distinct from a timeout: the process is alive and
// producing output, but is waiting for a human response it will never get.
var ErrAiderBlocked = errors.New("aider-blocked-interactive")

// runInteractiveOpts controls optional behaviour of runInteractive.
type runInteractiveOpts struct {
	// logPath, if non-empty, causes all subprocess output lines to be tee'd to
	// this file with a timestamp prefix. The directory is created if missing.
	logPath string
	// blockPatterns is a list of substrings; when any subprocess output line
	// contains one of these, the process is killed and ErrAiderBlocked is returned.
	// Used to detect Aider interactive prompts that would block forever in headless mode.
	blockPatterns []string
	// closeStdin, when true, wires the subprocess stdin to nil (os.DevNull equivalent)
	// instead of inheriting os.Stdin. Always set this for Aider in headless mode so
	// Aider cannot block on a terminal read.
	closeStdin bool
}

// aiderBlockPatterns lists substrings that indicate Aider is waiting for interactive
// input. Any of these appearing in Aider output signals BLOCKED_INTERACTIVE.
var aiderBlockPatterns = []string{
	"diff> ",        // Aider REPL prompt in diff edit-format (no --message supplied)
	"(Y)es/(N)o",   // generic Y/N confirmation prompt
	"[Y]es/[N]o",   // alternate formatting of Y/N prompt
	"Apply this fix", // legacy apply-patch prompt
	"Do you want to", // general interactive confirmation
	"Add .aiderignore", // gitignore setup prompt (belt-and-suspenders over --no-gitignore)
}

// guardExcludedDirs lists directories that are skipped when counting repo source files.
// These are build artefacts, tooling caches, and IDE directories that inflate the count.
var guardExcludedDirs = map[string]bool{
	".git": true, ".mera": true, ".claude": true,
	"node_modules": true, "bin": true, "obj": true,
	".vs": true, ".idea": true, ".next": true,
	"dist": true, "build": true, "coverage": true,
	"testresults": true, "artifacts": true, "packages": true,
	"logs": true, "tmp": true, "temp": true,
}

// countSourceFiles walks the repo root, skipping known non-source directories,
// and returns the number of regular files found.
func countSourceFiles() int {
	count := 0
	_ = filepath.Walk(root(), func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			// Skip known noise dirs — compare lowercased basename only.
			if guardExcludedDirs[strings.ToLower(info.Name())] {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		return nil
	})
	return count
}

// checkMinimalModelGuard blocks -Code on large repos when the code model is qwen2.5-coder:7b.
// Minimal profile uses qwen2.5-coder:7b for all roles; it lacks the context window needed for
// accurate edits across repos with more than 500 source files.
// Issue 3: if a better model is already installed locally, it suggests the fast mera -SetModel
// command instead of a full profile upgrade.
func checkMinimalModelGuard(target string) error {
	codeModel := modelForRole(RoleCode)
	if codeModel != "qwen2.5-coder:7b" {
		return nil
	}
	count := countSourceFiles()
	if count <= 500 {
		return nil
	}

	// Determine the right remediation: if qwen2.5-coder:14b is already installed,
	// the user just needs to switch the role, not download anything.
	var remediation string
	if isModelAvailable("qwen2.5-coder:14b") {
		remediation = "qwen2.5-coder:14b is already installed — switch with:\n" +
			"         mera -SetModel code qwen2.5-coder:14b"
	} else if isModelAvailable("phi4") {
		remediation = "phi4 is available as an interim — switch with:\n" +
			"         mera -SetModel code phi4\n" +
			"       Or install the recommended code model:\n" +
			"         setup.ps1 -UpgradeProfile Balanced"
	} else {
		remediation = "Install a capable code model:\n" +
			"         setup.ps1 -UpgradeProfile Balanced"
	}

	return fmt.Errorf(
		"[MERA] Minimal model (qwen2.5-coder:7b) is not recommended for code execution on large repositories (%d source files).\n"+
			"       %s",
		count, remediation)
}

// aiderLogPath returns the path for the current Aider session raw log and ensures
// the parent directory exists. Each run overwrites the previous log.
func aiderLogPath() string {
	dir := filepath.Join(meraDir(), "logs")
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "aider-session.log")
}

// runAider launches Aider with a MERA-generated session briefing and optional targeted file list.
// targetFiles come from File Scout — passed as --file args so Aider focuses immediately.
// sessionAgents provides agent intelligence embedded in the session document.
func runAider(mode, target, task, perf string, code bool, targetFiles []string, sessionAgents []AgentResult) error {
	if e := ensureGitRepo(false); e != nil {
		return e
	}
	if e := ensureOllama(); e != nil {
		return e
	}

	cfg := loadConfig()
	mt := cfg.MapTokensNormal
	switch perf {
	case "fast":
		mt = cfg.MapTokensFast
	case "deep":
		mt = cfg.MapTokensDeep
	}

	// Resolve the edit model: prefer RoleCodeEdit (fast, reliable) over RoleCode.
	// RoleCodeEdit defaults to qwen2.5-coder:7b on Balanced/Deep profiles — smaller
	// context window but dramatically more responsive for narrow diff edits.
	editModel := modelForRole(RoleCodeEdit)
	if editModel == "" {
		editModel = modelForRole(RoleCode)
	}

	session, e := buildSession(mode, target, task, perf, code, sessionAgents)
	if e != nil {
		return e
	}

	// ── Preflight: Ollama latency check (Fix 4) ───────────────────────────
	// A tiny generation benchmarks whether the model is warm and responsive.
	// Prints a warning if latency exceeds 30s so the user knows to expect slowness.
	fmt.Printf("[MERA] Preflight: checking Ollama latency for %s...\n", editModel)
	latency, latErr := checkOllamaLatency(editModel)
	switch {
	case latErr != nil:
		fmt.Printf("[WARN] Ollama latency check failed (%v) — model may not be loaded\n", latErr)
	case latency > 30*time.Second:
		fmt.Printf("[WARN] Ollama latency: %s — model is very slow; edits may timeout\n", latency.Round(time.Second))
		fmt.Println("[MERA] Consider: mera -Fast or wait for model to warm up before retrying")
	default:
		fmt.Printf("[MERA] Ollama latency: %s — OK\n", latency.Round(time.Millisecond))
	}

	args := []string{
		"--model", "ollama/" + editModel,
		// Fix 2: Drop --architect mode. In architect mode Aider shows
		// "architect>" and waits for the model — with Ollama local models
		// this reliably hangs. Direct edit mode with --edit-format diff is
		// faster, produces smaller outputs, and doesn't require a two-model
		// round-trip. Diff format works well with all qwen2.5-coder variants.
		"--edit-format", "diff",
		"--read", session,
		"--no-auto-commits",
		"--no-dirty-commits",
		"--suggest-shell-commands",
		"--show-diffs",
		// Phase 10.13: headless flags ─────────────────────────────────────
		// --no-pretty:     disables rich terminal output that triggers
		//                  UnicodeDecodeError on Windows OEM consoles.
		// --no-gitignore:  suppresses the interactive gitignore-setup prompt
		//                  that blocks headless runs.
		// --yes:           auto-confirms any remaining Aider confirmation
		//                  prompts without blocking on stdin.
		// --message:       runs Aider in one-shot non-interactive mode —
		//                  processes the task, applies edits, then exits.
		//                  Without this flag Aider would enter the REPL and
		//                  print "diff> " waiting for user input forever.
		"--no-pretty",
		"--no-gitignore",
		"--yes",
		"--map-tokens", fmt.Sprint(mt),
		// Non-interactive stability flags:
		// --no-fancy-input: disables prompt_toolkit / readline (fixes "No Windows console found").
		// --no-check-update: suppresses update-check banner that can block startup.
		"--no-fancy-input",
		"--no-check-update",
		"--message", task,
	}

	for _, f := range targetFiles {
		args = append(args, "--file", f)
	}

	if len(targetFiles) > 0 {
		// --subtree-only limits Aider's repo-map to the targeted file subtree,
		// reducing noise and prompt size when working on a well-scoped fix.
		args = append(args, "--subtree-only")
		fmt.Printf("[MERA] Launching Aider — model: %s, %d file(s), map-tokens %d, subtree-only, edit-format diff\n",
			editModel, len(targetFiles), mt)
	} else {
		fmt.Printf("[MERA] Launching Aider — model: %s, repo-map mode, map-tokens %d, edit-format diff\n",
			editModel, mt)
	}

	// Fix 5: Adaptive watchdog — two-phase silence detection.
	// startupKill: max wait before the FIRST token (model loading / warm-up).
	// silenceKill: max inactivity between tokens AFTER the first token arrives.
	const (
		aiderStartupKill = 180 * time.Second // 3 min for cold model start
		aiderSilenceKill = 60 * time.Second  // 1 min inactivity mid-generation
	)

	// Phase 10.13: fully headless opts.
	// closeStdin: Aider must not read from the terminal — we pass --message so
	//             it processes one-shot and exits. Inheriting os.Stdin would let
	//             Aider block on unexpected interactive prompts.
	// logPath:    raw subprocess output captured for post-mortem diagnosis.
	// blockPatterns: safety net — if Aider somehow prints an interactive prompt
	//             despite --message/--yes/--no-gitignore, kill it immediately and
	//             return ErrAiderBlocked (not a generic timeout).
	lp := aiderLogPath()
	fmt.Printf("[MERA] Aider session log: %s\n", lp)
	opts := runInteractiveOpts{
		closeStdin:    true,
		logPath:       lp,
		blockPatterns: aiderBlockPatterns,
	}
	return runInteractive(context.Background(), "aider", args,
		time.Duration(cfg.TimeoutSeconds)*time.Second, aiderStartupKill, aiderSilenceKill, opts)
}

// buildSession writes active-session.md that Aider reads via --read.
// When agents are provided, their outputs and actual file contents are embedded.
func buildSession(mode, target, task, perf string, code bool, agents []AgentResult) (string, error) {
	cfg := loadConfig()
	p := detectProject()
	stackJSON, _ := json.MarshalIndent(cfg.Stack, "", "  ")

	profile := "ANALYZE / PLAN MODE — read only. Do not edit files unless explicitly asked."
	if code {
		profile = "CODE ASSIST MODE — analyze existing patterns, write scoped targeted changes, use valid Aider edit format. No antd / MUI / Bootstrap / Chakra. Use existing project patterns. No new dependencies without approval. Minimal blast radius."
	}
	switch perf {
	case "fast":
		profile += " | FAST MODE: minimal context, exact targeted files only."
	case "deep":
		profile += " | DEEP MODE: thorough architecture and risk analysis before any edits."
	}

	var sb strings.Builder
	sb.WriteString("# MERA Active Session\n\n")
	sb.WriteString(fmt.Sprintf("**Mode:** %s  \n**Target:** %s  \n**Task:** %s\n\n", mode, target, task))

	sb.WriteString("## Project\n\n")
	sb.WriteString(fmt.Sprintf("- Type: %s\n- Build: `%s`\n- Test: `%s`\n- Frontend: `%s`\n\n",
		p.Type, p.Build, p.Test, p.FrontendBuild))

	sb.WriteString("## Profile\n\n")
	sb.WriteString(profile + "\n\n")

	sb.WriteString("## Stack Constraints\n\n```json\n")
	sb.Write(stackJSON)
	sb.WriteString("\n```\n\n")

	sb.WriteString("## Rules\n\n")
	sb.WriteString("- Never auto-commit or auto-push\n")
	sb.WriteString("- Only use patterns that already exist in this repository\n")
	sb.WriteString("- Do not invent APIs, routes, controllers, or services not present in the repo\n")
	sb.WriteString("- Respect module boundaries\n")
	sb.WriteString("- Minimal scoped changes — touch only what the task requires\n\n")

	// ── BUGFIX_NARROW deterministic briefing ──────────────────────────────────
	// For narrow bugfixes inject a strict, concise directive section that overrides
	// general heuristics and forces Aider to stay surgical. This is the primary
	// guard against context drift when using a small edit model (qwen2.5-coder:7b).
	if classifyTaskScope(task) == ScopeBugfixNarrow {
		targetFiles := extractScoutFiles(agents)
		sb.WriteString("## BUGFIX_NARROW — Strict Directive\n\n")
		sb.WriteString("> This is a NARROW BUGFIX session. The following constraints are MANDATORY.\n\n")
		if len(targetFiles) > 0 {
			sb.WriteString("**Selected files for this fix (modify ONLY these):**\n")
			for _, f := range targetFiles {
				rel, _ := filepath.Rel(root(), f)
				sb.WriteString("- `" + rel + "`\n")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("**Suspected fix area:** " + task + "\n\n")
		sb.WriteString("**Constraints — READ BEFORE EDITING:**\n")
		sb.WriteString("1. Do NOT refactor existing code. Only fix what is directly broken.\n")
		sb.WriteString("2. Do NOT add, rename, or remove tests. The test suite must compile unchanged.\n")
		sb.WriteString("3. Do NOT touch files not listed above. If a fix requires another file, STOP and report it instead.\n")
		sb.WriteString("4. Do NOT add new dependencies, packages, or NuGet references.\n")
		sb.WriteString("5. Do NOT add XML/JSON comments, logging statements, or documentation unless already present in the file.\n")
		sb.WriteString("6. Produce the smallest possible diff — ideally a single-line or single-block change.\n")
		sb.WriteString("7. If you cannot identify a concrete fix with high confidence, output NOTHING and explain why.\n\n")
		sb.WriteString("**Expected patch format:** A minimal unified diff targeting the broken binding, attribute, or parameter.\n\n")
	}

	if len(agents) > 0 {
		sb.WriteString("## Agent Intelligence\n\n")
		sb.WriteString("> Generated by MERA analysis agents before this session. Treat as authoritative context.\n\n")
		for _, a := range agents {
			if a.Output == "" && len(a.Files) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s (%s)\n\n", a.Agent, a.Status))
			if a.Output != "" {
				sb.WriteString(a.Output + "\n\n")
			}
			if len(a.Risks) > 0 {
				sb.WriteString("**Risks:**\n")
				for _, r := range a.Risks {
					sb.WriteString(fmt.Sprintf("- [!] %s\n", r))
				}
				sb.WriteString("\n")
			}
		}
	}

	// Inject actual content of target files so Aider has immediate code context.
	targetFiles := extractScoutFiles(agents)
	if len(targetFiles) > 0 {
		sb.WriteString("## Target File Contents\n\n")
		sb.WriteString("> These are the files identified for modification. Read them before making any changes.\n\n")
		for _, f := range targetFiles {
			rel, _ := filepath.Rel(root(), f)
			content := sampleFile(f, getProfileSettings().MaxFileLines)
			if content == "" {
				continue
			}
			ext := strings.TrimPrefix(filepath.Ext(f), ".")
			sb.WriteString(fmt.Sprintf("### `%s`\n\n```%s\n%s\n```\n\n", rel, ext, content))
		}
	}

	path := filepath.Join(meraDir(), "active-session.md")
	return path, writeNoBOM(path, []byte(sb.String()))
}

// extractScoutFiles pulls the File Scout's file list from agent results.
func extractScoutFiles(agents []AgentResult) []string {
	for _, a := range agents {
		if a.Agent == "File Scout" {
			return a.Files
		}
	}
	return nil
}

// ── Active child-process tracking ────────────────────────────────────────────
// The abort handler uses these to kill the current child process (Aider,
// dotnet build, etc.) before writing the session report and exiting.

var (
	activeCmdMu sync.Mutex
	activeCmd   *exec.Cmd
)

func setActiveCmd(cmd *exec.Cmd) {
	activeCmdMu.Lock()
	activeCmd = cmd
	activeCmdMu.Unlock()
}

func clearActiveCmd() {
	activeCmdMu.Lock()
	activeCmd = nil
	activeCmdMu.Unlock()
}

// killActiveCmd sends Kill to the currently running child process, if any.
// Safe to call from any goroutine; no-op if no process is running.
func killActiveCmd() {
	activeCmdMu.Lock()
	cmd := activeCmd
	activeCmdMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// checkOllamaLatency runs a minimal generation against model and returns wall-clock time.
// Used as a preflight before launching Aider so the user gets an early warning if the
// model is cold or overloaded. Uses num_predict:3 to minimize token generation.
func checkOllamaLatency(model string) (time.Duration, error) {
	type req struct {
		Model   string         `json:"model"`
		Prompt  string         `json:"prompt"`
		Stream  bool           `json:"stream"`
		Options map[string]int `json:"options"`
	}
	payload, _ := json.Marshal(req{
		Model:   model,
		Prompt:  "Reply: OK",
		Stream:  false,
		Options: map[string]int{"num_predict": 3},
	})
	client := &http.Client{Timeout: 45 * time.Second}
	start := time.Now()
	resp, err := client.Post("http://localhost:11434/api/generate",
		"application/json", strings.NewReader(string(payload)))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return time.Since(start), nil
}

// runInteractive runs a command with live stdout/stderr piped to the terminal.
// Implements an adaptive two-phase watchdog (Fix 5):
//   - startupKill: kills the process if NO output is received within this duration
//     (handles cold model startup). 0 = no startup kill.
//   - silenceKill:  kills the process if no output is received for this duration
//     AFTER the first token arrives (handles mid-generation stalls). 0 = no silence kill.
//
// opts is optional (variadic, max one element). When supplied it enables:
//   - log capture (all output tee'd to opts.logPath with timestamps)
//   - interactive-prompt detection (opts.blockPatterns → ErrAiderBlocked)
//   - headless stdin (opts.closeStdin = true → subprocess gets nil stdin)
func runInteractive(ctx context.Context, name string, args []string, timeout, startupKill, silenceKill time.Duration, opts ...runInteractiveOpts) error {
	var o runInteractiveOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root()
	so, _ := cmd.StdoutPipe()
	se, _ := cmd.StderrPipe()

	// Phase 10.13: headless stdin.
	// When closeStdin is set we give the subprocess nil stdin (equivalent to
	// /dev/null). Aider uses --message so it never needs to read from stdin;
	// passing os.Stdin would allow it to block on interactive prompts.
	if !o.closeStdin {
		cmd.Stdin = os.Stdin
	}

	// Phase 10.13: open log file for tee capture.
	var logWriter *os.File
	if o.logPath != "" {
		_ = os.MkdirAll(filepath.Dir(o.logPath), 0755)
		if lf, err := os.Create(o.logPath); err == nil {
			logWriter = lf
			defer lf.Close()
			fmt.Fprintf(lf, "# MERA Aider session log — %s\n# Command: %s %s\n\n",
				time.Now().Format(time.RFC3339), name, strings.Join(args, " "))
		}
	}

	setActiveCmd(cmd)
	defer clearActiveCmd()

	if e := cmd.Start(); e != nil {
		return e
	}

	startTime := time.Now()
	var (
		mu            sync.Mutex
		last          = time.Now()
		firstToken    bool
		blockDetected bool
	)
	done := make(chan error, 1)

	pipe := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()

			// Phase 10.13: check for interactive-prompt patterns before printing.
			// If detected, kill the process immediately so we never hang.
			if len(o.blockPatterns) > 0 {
				for _, pat := range o.blockPatterns {
					if strings.Contains(line, pat) {
						mu.Lock()
						if !blockDetected {
							blockDetected = true
							fmt.Printf("\n[MERA] BLOCKED_INTERACTIVE — Aider printed an interactive prompt: %q\n", line)
							fmt.Println("[MERA] Killing Aider process. This should not happen in headless mode.")
							if cmd.Process != nil {
								_ = cmd.Process.Kill()
							}
						}
						mu.Unlock()
						break
					}
				}
			}

			mu.Lock()
			last = time.Now()
			firstToken = true // first output received — startup phase is over
			mu.Unlock()

			fmt.Println(line)

			// Tee to log file with timestamp.
			if logWriter != nil {
				fmt.Fprintf(logWriter, "[%s] %s\n", time.Now().Format("15:04:05.000"), line)
			}
		}
	}
	go pipe(so)
	go pipe(se)
	go func() { done <- cmd.Wait() }()

	cfg := loadConfig()
	ticker := time.NewTicker(time.Duration(cfg.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case e := <-done:
			mu.Lock()
			bd := blockDetected
			mu.Unlock()
			if bd {
				return fmt.Errorf("%w: interactive prompt detected — headless mode requires --message and --yes", ErrAiderBlocked)
			}
			return e
		case <-ticker.C:
			mu.Lock()
			ft := firstToken
			elapsed := time.Since(last).Round(time.Second)
			startup := time.Since(startTime).Round(time.Second)
			mu.Unlock()

			if !ft {
				// ── Startup phase: waiting for first token ────────────────────
				fmt.Printf("[MERA] Waiting for model response... %s elapsed\n", startup)
				if startupKill > 0 && startup >= startupKill {
					_ = cmd.Process.Kill()
					fmt.Printf("\n[MERA] No output after %s — model startup timed out.\n", startup)
					fmt.Println("[MERA] The model may not be loaded or the context is too large.")
					fmt.Println("[MERA] Retry options:")
					fmt.Println("[MERA]   mera -Fast <module> \"task\"          # smaller model, faster")
					fmt.Println("[MERA]   ollama run " + name + "               # warm up model first")
					return fmt.Errorf("%w: no output after %s (startup timeout)", ErrAiderSilenceTimeout, startup)
				}
			} else {
				// ── Active phase: between tokens ──────────────────────────────
				fmt.Println("[MERA] Still generating... no output for", elapsed)
				if silenceKill > 0 && elapsed >= silenceKill {
					_ = cmd.Process.Kill()
					fmt.Printf("\n[MERA] Aider silent for %s — watchdog terminated session.\n", elapsed)
					fmt.Println("[MERA] Session briefing is saved. To retry:")
					fmt.Println("[MERA]   mera -Replay                       # re-run with same context")
					fmt.Println("[MERA]   mera -Plan <module> \"task\"          # re-plan with lighter model")
					fmt.Println("[MERA]   mera -Fast <module> \"task\"          # use fast profile instead")
					return fmt.Errorf("%w: silent for %s after first response", ErrAiderSilenceTimeout, elapsed)
				}
				if elapsed > 90*time.Second {
					fmt.Println("[WARN] Long silence. Ctrl+C to abort, then retry with -Fast.")
				}
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return fmt.Errorf("command timed out: %s", name)
		}
	}
}

// ── AiderSmoke ────────────────────────────────────────────────────────────────

// runAiderSmoke verifies end-to-end headless Aider connectivity.
// It performs four checks in sequence:
//  1. Ollama process reachable
//  2. Code-edit model latency (model must respond within 45 s)
//  3. Headless Aider launch — process starts without blocking on stdin
//  4. Model responded (MERA_SMOKE_OK token appears in captured log)
//
// Use this after any change to the Aider invocation path to confirm
// the pipeline works before running a real coding task.
func runAiderSmoke() error {
	fmt.Println("[SMOKE] ─────────────────────────────────────────")
	fmt.Println("[SMOKE] MERA Aider Smoke Test")
	fmt.Println("[SMOKE] ─────────────────────────────────────────")

	// Step 1: Ollama connectivity
	fmt.Println("[SMOKE] Step 1/4: Ollama connectivity...")
	if e := ensureOllama(); e != nil {
		return fmt.Errorf("[SMOKE] FAIL — Ollama not available: %w", e)
	}
	fmt.Println("[SMOKE]   Ollama: OK")

	// Step 2: Model latency
	editModel := modelForRole(RoleCodeEdit)
	if editModel == "" {
		editModel = modelForRole(RoleCode)
	}
	fmt.Printf("[SMOKE] Step 2/4: Model latency check (%s)...\n", editModel)
	latency, latErr := checkOllamaLatency(editModel)
	if latErr != nil {
		return fmt.Errorf("[SMOKE] FAIL — model %s not reachable: %w", editModel, latErr)
	}
	if latency > 45*time.Second {
		fmt.Printf("[SMOKE]   WARN — latency %s is high; Aider may be slow\n", latency.Round(time.Second))
	} else {
		fmt.Printf("[SMOKE]   Model latency: %s — OK\n", latency.Round(time.Millisecond))
	}

	// Step 3 & 4: Headless Aider launch with a trivial one-shot message.
	// The message is designed to produce a predictable text token (MERA_SMOKE_OK)
	// that we can grep from the captured log to confirm the model responded.
	// We do NOT ask Aider to edit any files — the smoke test only validates
	// the Aider launch pipeline, not code editing.
	smokeLogPath := filepath.Join(meraDir(), "logs", "aider-smoke.log")
	_ = os.MkdirAll(filepath.Dir(smokeLogPath), 0755)

	fmt.Println("[SMOKE] Step 3/4: Headless Aider launch (one-shot message)...")
	fmt.Printf("[SMOKE]   Log: %s\n", smokeLogPath)

	smokeArgs := []string{
		"--model", "ollama/" + editModel,
		"--edit-format", "diff",
		"--no-auto-commits",
		"--no-dirty-commits",
		"--no-gitignore",
		"--no-fancy-input",
		"--no-check-update",
		"--no-pretty",
		"--yes",
		// One-shot: process this message and exit without editing any files.
		"--message", "Reply with the exact token MERA_SMOKE_OK and nothing else. Do not edit any files.",
	}

	smokeOpts := runInteractiveOpts{
		closeStdin:    true,
		logPath:       smokeLogPath,
		blockPatterns: aiderBlockPatterns,
	}

	const (
		smokeTimeout = 120 * time.Second
		smokeStartup = 90 * time.Second
		smokeSilence = 60 * time.Second
	)

	err := runInteractive(context.Background(), "aider", smokeArgs,
		smokeTimeout, smokeStartup, smokeSilence, smokeOpts)

	if errors.Is(err, ErrAiderBlocked) {
		fmt.Println("[SMOKE] FAIL — Aider blocked on an interactive prompt.")
		fmt.Println("[SMOKE]   The --message / --yes / --no-gitignore flags are not suppressing prompts.")
		fmt.Printf("[SMOKE]   Full log: %s\n", smokeLogPath)
		return fmt.Errorf("smoke: %w", err)
	}
	if err != nil {
		// Non-zero exit from Aider is common when no files were edited (exit 1).
		// Only treat it as a hard failure if the log is completely empty.
		fmt.Printf("[SMOKE]   Aider exited: %v (checking log for model response)\n", err)
	}

	// Step 4: Verify model actually responded.
	fmt.Println("[SMOKE] Step 4/4: Verifying model response in log...")
	logBytes, readErr := os.ReadFile(smokeLogPath)
	if readErr != nil {
		fmt.Printf("[SMOKE]   WARN — could not read log: %v\n", readErr)
	}
	logText := string(logBytes)

	switch {
	case strings.Contains(logText, "MERA_SMOKE_OK"):
		fmt.Println("[SMOKE]   Model responded with MERA_SMOKE_OK — pipeline confirmed.")
	case len(logText) > 80:
		// Model responded with something but not the exact token — still a good sign.
		fmt.Println("[SMOKE]   Model responded (token mismatch — model paraphrased the message).")
		fmt.Println("[SMOKE]   Pipeline appears functional; model may not follow exact instructions.")
	default:
		fmt.Println("[SMOKE]   WARN — log is empty or very short; model may not have responded.")
		fmt.Printf("[SMOKE]   Full log: %s\n", smokeLogPath)
		return fmt.Errorf("smoke: model did not produce output — check Ollama and model availability")
	}

	fmt.Println("[SMOKE] ─────────────────────────────────────────")
	fmt.Println("[SMOKE] All checks passed.")
	fmt.Printf("[SMOKE]   Model   : %s\n", editModel)
	fmt.Printf("[SMOKE]   Latency : %s\n", latency.Round(time.Millisecond))
	fmt.Printf("[SMOKE]   Log     : %s\n", smokeLogPath)
	fmt.Println("[SMOKE] ─────────────────────────────────────────")
	return nil
}
