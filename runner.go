package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
	model := modelForRole(RoleCode)
	mt := cfg.MapTokensNormal
	switch perf {
	case "fast":
		mt = cfg.MapTokensFast
	case "deep":
		mt = cfg.MapTokensDeep
	}

	session, e := buildSession(mode, target, task, perf, code, sessionAgents)
	if e != nil {
		return e
	}

	args := []string{
		"--model", "ollama/" + model,
		"--architect",
		"--read", session,
		"--no-auto-commits",
		"--no-dirty-commits",
		"--suggest-shell-commands",
		"--show-diffs",
		"--pretty",
		"--map-tokens", fmt.Sprint(mt),
	}

	for _, f := range targetFiles {
		args = append(args, "--file", f)
	}

	if len(targetFiles) > 0 {
		// --subtree-only limits Aider's repo-map to the targeted file subtree,
		// reducing noise and prompt size when working on a well-scoped fix.
		args = append(args, "--subtree-only")
		fmt.Printf("[MERA] Launching Aider — %d targeted files, map-tokens %d, subtree-only\n", len(targetFiles), mt)
	} else {
		fmt.Printf("[MERA] Launching Aider — repo-map mode, map-tokens %d\n", mt)
	}

	const aiderSilenceKill = 120 * time.Second
	return runInteractive(context.Background(), "aider", args, time.Duration(cfg.TimeoutSeconds)*time.Second, aiderSilenceKill)
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

// runInteractive runs a command with live stdout/stderr piped to the terminal.
// Shows a heartbeat if the process goes silent for HeartbeatSeconds.
// When silenceKill > 0, the process is terminated if no output is received for that duration.
func runInteractive(ctx context.Context, name string, args []string, timeout, silenceKill time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root()
	so, _ := cmd.StdoutPipe()
	se, _ := cmd.StderrPipe()
	cmd.Stdin = os.Stdin

	setActiveCmd(cmd)
	defer clearActiveCmd()

	if e := cmd.Start(); e != nil {
		return e
	}

	var (
		mu   sync.Mutex
		last = time.Now()
	)
	done := make(chan error, 1)

	pipe := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			mu.Lock()
			last = time.Now()
			mu.Unlock()
			fmt.Println(sc.Text())
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
			return e
		case <-ticker.C:
			mu.Lock()
			elapsed := time.Since(last).Round(time.Second)
			mu.Unlock()
			fmt.Println("[MERA] Still working... no output for", elapsed)
			// Hard kill after configurable silence period (Aider watchdog).
			if silenceKill > 0 && elapsed >= silenceKill {
				_ = cmd.Process.Kill()
				fmt.Printf("\n[MERA] Aider silent for %s — session terminated.\n", elapsed)
				fmt.Println("[MERA] Your session briefing is saved. To continue:")
				fmt.Println("[MERA]   mera -Replay                       # re-run with same context")
				fmt.Println("[MERA]   mera -Plan <module> \"task\"          # re-plan with lighter model")
				fmt.Println("[MERA]   mera -Fast <module> \"task\"          # use fast profile instead")
				return fmt.Errorf("aider terminated after %s of silence — session saved for replay", elapsed)
			}
			if elapsed > 2*time.Minute {
				fmt.Println("[WARN] Long silence. Ctrl+C if stuck, then retry with -Fast or -Plan.")
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return fmt.Errorf("command timed out: %s", name)
		}
	}
}
