package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── Schema versions ────────────────────────────────────────────────────────────

const (
	ConfigSchemaVersion = 1
	ModelsSchemaVersion = 1
	meraLogMaxBytes     = 512 * 1024 // 512 KB
)

// ── Operational log ───────────────────────────────────────────────────────────

func meraLogPath() string { return filepath.Join(meraDir(), "mera.log") }

// appendMeraLog writes a timestamped log entry to .mera/mera.log.
// Rotates the log to mera.log.bak when it exceeds 512 KB.
func appendMeraLog(level, msg string) {
	path := meraLogPath()
	if fi, err := os.Stat(path); err == nil && fi.Size() >= meraLogMaxBytes {
		_ = os.Rename(path, path+".bak")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format("2006-01-02 15:04:05")
	_, _ = fmt.Fprintf(f, "[%s] %s  %s\n", ts, level, msg)
}

// ── mera -Logs ────────────────────────────────────────────────────────────────

// showLogs prints recent failures, fallbacks, and gate blocks from mera.log.
func showLogs() {
	path := meraLogPath()
	lines := tailFile(path, 200)
	if len(lines) == 0 {
		fmt.Println("[MERA] No log entries found at", path)
		return
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Recent Events  (.mera/mera.log)")
	fmt.Println("[MERA] ========================================")

	interesting := []string{"ERROR", "WARN", "FALLBACK", "GATE", "BLOCK", "FAIL", "TIMEOUT", "PATCH"}
	shown := 0
	for _, l := range lines {
		up := strings.ToUpper(l)
		for _, kw := range interesting {
			if strings.Contains(up, kw) {
				fmt.Println(" ", l)
				shown++
				break
			}
		}
	}
	if shown == 0 {
		fmt.Println("  (no failures, fallbacks, or gate events found in last 200 log lines)")
	}
	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA] Log file: %s\n", path)
}

// tailFile returns the last n lines of a file.
func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// ── mera -Diag ────────────────────────────────────────────────────────────────

// runDiag prints a full system diagnostic snapshot.
func runDiag() {
	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  System Diagnostics")
	fmt.Println("[MERA] ========================================")

	// Hardware
	ram := getRamGB()
	disk := getDiskFreeGB()
	fmt.Printf("\n  Hardware:\n")
	fmt.Printf("    RAM:            %.1f GB\n", ram)
	fmt.Printf("    Disk free:      %.1f GB\n", disk)

	// Tool versions
	tools := []string{"git", "aider", "ollama", "python", "pip", "dotnet", "node", "go"}
	fmt.Printf("\n  Tool versions:\n")
	for _, t := range tools {
		ver := toolVersion(t)
		if ver == "" {
			fmt.Printf("    %-10s  not found\n", t)
		} else {
			fmt.Printf("    %-10s  %s\n", t, ver)
		}
	}

	// PATH check — three strategies so "mera -Diag" is self-consistent even when
	// the user ran via full path rather than a PATH-resolved command.
	meraInPath := false
	meraPathDetail := ""
	if _, err := exec.LookPath("mera.exe"); err == nil {
		meraInPath = true
		meraPathDetail = "mera.exe resolved via PATH"
	} else if _, err := exec.LookPath("mera"); err == nil {
		meraInPath = true
		meraPathDetail = "mera resolved via PATH"
	} else if exe, exeErr := os.Executable(); exeErr == nil {
		exeDir := filepath.Dir(exe)
		for _, p := range filepath.SplitList(os.Getenv("PATH")) {
			if strings.EqualFold(filepath.Clean(p), filepath.Clean(exeDir)) {
				meraInPath = true
				meraPathDetail = exeDir
				break
			}
		}
		if !meraInPath {
			meraPathDetail = "not found — add to PATH: " + exeDir
		}
	}
	fmt.Printf("\n  PATH:\n")
	if meraInPath {
		fmt.Printf("    mera            in PATH  (%s)\n", meraPathDetail)
	} else {
		fmt.Printf("    mera            NOT in PATH  (%s)\n", meraPathDetail)
	}

	// Ollama API
	fmt.Printf("\n  Ollama API:\n")
	if ollamaAPIUp() {
		fmt.Printf("    Status:         reachable (localhost:11434)\n")
		models := listOllamaModels()
		fmt.Printf("    Models:         %d installed\n", len(models))
		for _, m := range models {
			fmt.Printf("                    %s\n", m)
		}
	} else {
		fmt.Printf("    Status:         NOT reachable\n")
	}

	// Config files
	fmt.Printf("\n  Config files:\n")
	cfgPath := filepath.Join(meraDir(), "config.json")
	modPath := modelConfigPath()
	printConfigStatus("config.json", cfgPath)
	printConfigStatus("models.json", modPath)

	// Active profile
	mc := loadModelConfig()
	fmt.Printf("\n  Active profile:   %s\n", mc.Profile)

	// Git
	fmt.Printf("\n  Git:\n")
	if _, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			fmt.Printf("    Git repo:       yes\n")
		} else {
			fmt.Printf("    Git repo:       not a git repository\n")
		}
	} else {
		fmt.Printf("    Git:            not found\n")
	}

	// MERA dirs
	fmt.Printf("\n  MERA directories:\n")
	for _, d := range []string{meraDir(), reportsDir(), snapshotsDir(), sessionsDir()} {
		rel, _ := filepath.Rel(root(), d)
		if _, err := os.Stat(d); err == nil {
			fmt.Printf("    %-20s OK\n", rel)
		} else {
			fmt.Printf("    %-20s MISSING\n", rel)
		}
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Run 'mera -Health' for weighted health score.")
}

func printConfigStatus(label, path string) {
	ok, detail := validateConfigFile(path)
	if ok {
		fmt.Printf("    %-20s  OK\n", label)
	} else {
		fmt.Printf("    %-20s  INVALID — %s\n", label, detail)
	}
}

// ── mera -Health ─────────────────────────────────────────────────────────────

type healthComponent struct {
	Name   string
	Weight int // relative weight in score
	Status string // "OK", "WARN", "FAIL"
	Detail string
}

// gatherHealthComponents checks each component and returns their status.
// Weights: OllamaAPI=3, CodeModel=2, Aider=2, ConfigFiles=2, Git=1, MeraBin=1, PATH=1
func gatherHealthComponents() []healthComponent {
	comps := []healthComponent{}

	// Ollama API (weight 3)
	if ollamaAPIUp() {
		comps = append(comps, healthComponent{"Ollama API", 3, "OK", "localhost:11434 reachable"})
	} else {
		comps = append(comps, healthComponent{"Ollama API", 3, "FAIL", "not reachable — run: ollama serve"})
	}

	// Code model (weight 2)
	mc := loadModelConfig()
	codeModel := mc.Models[RoleCode]
	if codeModel == "" {
		codeModel = loadConfig().DefaultModel
	}
	if ollamaAPIUp() && isModelAvailable(codeModel) {
		comps = append(comps, healthComponent{"Code model", 2, "OK", codeModel + " installed"})
	} else if !ollamaAPIUp() {
		comps = append(comps, healthComponent{"Code model", 2, "WARN", "cannot check — Ollama not running"})
	} else {
		comps = append(comps, healthComponent{"Code model", 2, "FAIL", codeModel + " not installed — run: ollama pull " + codeModel})
	}

	// Profile models (weight 2)
	if !ollamaAPIUp() {
		comps = append(comps, healthComponent{"Profile models", 2, "WARN", "cannot check — Ollama not running"})
	} else {
		allOK, missingCount, _ := checkProfileModelCompliance()
		mc2 := loadModelConfig()
		if allOK {
			comps = append(comps, healthComponent{"Profile models", 2, "OK", fmt.Sprintf("%s profile — all models ready", mc2.Profile)})
		} else {
			comps = append(comps, healthComponent{"Profile models", 2, "WARN",
				fmt.Sprintf("%s profile — %d model(s) missing (run: mera -Doctor)", mc2.Profile, missingCount)})
		}
	}

	// Aider (weight 2)
	if _, err := exec.LookPath("aider"); err == nil {
		comps = append(comps, healthComponent{"Aider", 2, "OK", "found in PATH"})
	} else {
		comps = append(comps, healthComponent{"Aider", 2, "FAIL", "not found — run: pip install aider-chat"})
	}

	// Config files (weight 2)
	cfgOK, cfgDetail := validateConfigFile(filepath.Join(meraDir(), "config.json"))
	modOK, modDetail := validateConfigFile(modelConfigPath())
	if cfgOK && modOK {
		comps = append(comps, healthComponent{"Config files", 2, "OK", "config.json and models.json valid"})
	} else if !cfgOK {
		comps = append(comps, healthComponent{"Config files", 2, "FAIL", "config.json: " + cfgDetail})
	} else {
		comps = append(comps, healthComponent{"Config files", 2, "WARN", "models.json: " + modDetail})
	}

	// Git (weight 1)
	if _, err := exec.LookPath("git"); err == nil {
		comps = append(comps, healthComponent{"Git", 1, "OK", "found in PATH"})
	} else {
		comps = append(comps, healthComponent{"Git", 1, "WARN", "not found — install git"})
	}

	// MERA binary (weight 1)
	if _, err := exec.LookPath("mera.exe"); err == nil {
		comps = append(comps, healthComponent{"MERA binary", 1, "OK", "mera.exe in PATH"})
	} else if _, err2 := exec.LookPath("mera"); err2 == nil {
		comps = append(comps, healthComponent{"MERA binary", 1, "OK", "mera in PATH"})
	} else {
		comps = append(comps, healthComponent{"MERA binary", 1, "WARN", "mera not in PATH — add install dir to PATH"})
	}

	// PATH / MERA dirs (weight 1)
	allDirsOK := true
	for _, d := range []string{meraDir(), reportsDir(), snapshotsDir(), sessionsDir()} {
		if _, err := os.Stat(d); err != nil {
			allDirsOK = false
			break
		}
	}
	if allDirsOK {
		comps = append(comps, healthComponent{"MERA dirs", 1, "OK", ".mera structure intact"})
	} else {
		comps = append(comps, healthComponent{"MERA dirs", 1, "WARN", "some .mera dirs missing — run: mera -Init"})
	}

	return comps
}

// healthScore computes a 0-100 percentage from component weights.
// OK=2 points, WARN=1, FAIL=0 per weight unit.
//
// Hard caps applied after weighted calculation:
//   - Any FAIL on a component with weight >= 2 (code model, Aider, etc.)  → max 40%
//   - Ollama API FAIL (weight 3 — nothing can work without it)             → max 20%
//
// This ensures a missing code model registers as CRITICAL, not "78% OK".
func healthScore(comps []healthComponent) int {
	total := 0
	earned := 0
	capScore := 100 // ceiling applied after any FAIL on a heavyweight component

	for _, c := range comps {
		total += c.Weight * 2
		switch c.Status {
		case "OK":
			earned += c.Weight * 2
		case "WARN":
			earned += c.Weight * 1
		case "FAIL":
			if c.Weight >= 3 && capScore > 20 {
				capScore = 20 // Ollama API or heavier: near-zero
			} else if c.Weight >= 2 && capScore > 40 {
				capScore = 40 // Code model, Aider, Config: hard CRITICAL ceiling
			}
		}
	}
	if total == 0 {
		return 0
	}
	raw := (earned * 100) / total
	if raw > capScore {
		return capScore
	}
	return raw
}

// runHealth prints the weighted health score with component breakdown.
func runHealth() {
	comps := gatherHealthComponents()
	score := healthScore(comps)

	grade := "CRITICAL"
	switch {
	case score >= 90:
		grade = "HEALTHY"
	case score >= 70:
		grade = "OK"
	case score >= 50:
		grade = "DEGRADED"
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA]  MERA HEALTH: %d%%  [%s]\n", score, grade)
	fmt.Println("[MERA] ========================================")

	fmt.Println("\n  Breakdown:")
	for _, c := range comps {
		icon := "OK  "
		if c.Status == "WARN" {
			icon = "WARN"
		} else if c.Status == "FAIL" {
			icon = "FAIL"
		}
		fmt.Printf("    [%-4s]  (wt:%d)  %-14s  %s\n", icon, c.Weight, c.Name, c.Detail)
	}

	// Recommendations for non-OK components
	var recs []string
	for _, c := range comps {
		if c.Status != "OK" {
			recs = append(recs, fmt.Sprintf("  %s: %s", c.Name, c.Detail))
		}
	}
	if len(recs) > 0 {
		fmt.Println("\n  Recommendations:")
		for _, r := range recs {
			fmt.Println("   ", r)
		}
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Run 'mera -Diag' for full system information.")
}

// ── Config validation ─────────────────────────────────────────────────────────

// validateConfigFile returns (valid, detail) for a JSON config file.
func validateConfigFile(path string) (bool, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, "not found"
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return false, "invalid JSON: " + err.Error()
	}
	return true, "ok"
}

// ── Ollama health (no side effects) ──────────────────────────────────────────

// ollamaAPIUp checks if the Ollama API is reachable without starting it.
func ollamaAPIUp() bool {
	out, err := exec.Command("ollama", "list").Output()
	if err == nil && len(out) > 0 {
		return true
	}
	// Fallback: HTTP check
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"try { $r=(New-Object Net.WebClient).DownloadString('http://localhost:11434/api/tags'); 'ok' } catch { 'fail' }")
	o, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(o)) == "ok"
}

// installedModelSet returns a set of installed Ollama model names (lowercased, both full and base forms).
func installedModelSet() map[string]bool {
	set := map[string]bool{}
	for _, m := range listOllamaModels() {
		ml := strings.ToLower(m)
		set[ml] = true
		if idx := strings.IndexByte(ml, ':'); idx >= 0 {
			set[ml[:idx]] = true
		}
	}
	return set
}

// checkProfileModelCompliance checks if every model assigned to a role in models.json
// is installed in the local Ollama instance.
// Returns (allInstalled, missingCount, missingModelNames).
func checkProfileModelCompliance() (bool, int, []string) {
	mc := loadModelConfig()
	set := installedModelSet()
	seen := map[string]bool{}
	var missing []string
	for _, model := range mc.Models {
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		if !isInModelSet(set, model) {
			missing = append(missing, model)
		}
	}
	return len(missing) == 0, len(missing), missing
}

// isInModelSet checks whether a model name (or its base name) exists in the set.
func isInModelSet(set map[string]bool, model string) bool {
	ml := strings.ToLower(model)
	if set[ml] {
		return true
	}
	if idx := strings.IndexByte(ml, ':'); idx >= 0 {
		return set[ml[:idx]]
	}
	return false
}

// ── Hardware info ─────────────────────────────────────────────────────────────

// getRamGB returns total installed RAM in GB.
// Primary: PowerShell CIM (works on Windows 10/11, immune to wmic deprecation).
// Fallback: wmic (Windows 8 / Server 2012 compatibility).
func getRamGB() float64 {
	// CIM path — returns raw byte count as a plain integer string.
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance -ClassName Win32_ComputerSystem -ErrorAction Stop).TotalPhysicalMemory").Output()
	if err == nil {
		raw := strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
		if n, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && n > 0 {
			return float64(n) / (1 << 30) // bytes → GB
		}
	}
	// wmic fallback — deprecated on Win11 but kept for older Windows.
	out2, err := exec.Command("wmic", "ComputerSystem", "get", "TotalPhysicalMemory", "/Value").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out2), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TotalPhysicalMemory=") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "TotalPhysicalMemory="))
			val = strings.ReplaceAll(val, "\r", "")
			if n, parseErr := strconv.ParseInt(val, 10, 64); parseErr == nil && n > 0 {
				return float64(n) / (1 << 30)
			}
		}
	}
	return 0
}

// getDiskFreeGB returns free disk space on the system drive in GB via PowerShell.
func getDiskFreeGB() float64 {
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"[Math]::Round((Get-PSDrive C).Free / 1GB, 1)")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	val := strings.TrimSpace(string(out))
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	return f
}

// toolVersion returns the first line of `<tool> --version`, or "" if not found.
func toolVersion(cmd string) string {
	out, err := exec.Command(cmd, "--version").Output()
	if err != nil {
		// some tools use -version or version
		out, err = exec.Command(cmd, "version").Output()
		if err != nil {
			return ""
		}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}
