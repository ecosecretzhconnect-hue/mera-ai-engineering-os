package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ── Role constants ─────────────────────────────────────────────────────────────

const (
	RolePlanner       = "planner"
	RoleArchitect     = "architect"
	RoleFileScout     = "filescout"
	RoleCode          = "code"
	// RoleCodeEdit is the model used by Aider for the actual edit step.
	// Intentionally separated from RoleCode so a faster / more reliable
	// code-edit model (e.g. qwen2.5-coder:7b) can be assigned while keeping
	// a larger analytical model for FileScout and planning roles.
	// Falls back to RoleCode when not explicitly configured.
	RoleCodeEdit      = "codeedit"
	RoleDiffReview    = "diffreview"
	RoleSecurity      = "security"
	RoleSprintAdvisor = "sprintadvisor"
)

// ── Performance profiles ───────────────────────────────────────────────────────

type PerformanceProfile string

const (
	ProfileFast   PerformanceProfile = "FAST"
	ProfileNormal PerformanceProfile = "NORMAL"
	ProfileDeep   PerformanceProfile = "DEEP"
	ProfileStrict PerformanceProfile = "STRICT"
)

// ── Model config ───────────────────────────────────────────────────────────────

// ModelConfig stores per-role model assignments and the active performance profile.
// Persisted to .mera/models.json.
type ModelConfig struct {
	SchemaVersion int                `json:"schemaVersion"`
	Models        map[string]string  `json:"models"`
	Profile       PerformanceProfile `json:"profile"`
}

func modelConfigPath() string { return filepath.Join(meraDir(), "models.json") }

// defaultModelConfig is the "Balanced" production default used when writing a new models.json
// via initProject. It is NOT used as a runtime fallback — see safeDefaultModelConfig.
func defaultModelConfig() ModelConfig {
	return ModelConfig{
		SchemaVersion: ModelsSchemaVersion,
		Models: map[string]string{
			RolePlanner:       "phi4",
			RoleArchitect:     "llama3.1:8b",
			RoleFileScout:     "phi4",
			RoleCode:          "qwen2.5-coder:14b",
			RoleCodeEdit:      "qwen2.5-coder:7b", // fast, reliable Aider edit model
			RoleDiffReview:    "deepseek-coder-v2",
			RoleSecurity:      "llama3.1:8b",
			RoleSprintAdvisor: "phi4",
		},
		Profile: ProfileNormal,
	}
}

// safeDefaultModelConfig returns the minimal safe configuration used when models.json
// is absent or unrecoverable. Uses a single small model for all roles.
func safeDefaultModelConfig() ModelConfig {
	const m = "qwen2.5-coder:7b"
	return ModelConfig{
		SchemaVersion: ModelsSchemaVersion,
		Profile:       ProfileFast,
		Models: map[string]string{
			RolePlanner: m, RoleArchitect: m, RoleFileScout: m,
			RoleCode: m, RoleCodeEdit: m, RoleDiffReview: m, RoleSecurity: m, RoleSprintAdvisor: m,
		},
	}
}

func loadModelConfig() ModelConfig {
	path := modelConfigPath()
	bakPath := path + ".bak"

	b, err := os.ReadFile(path)
	if err != nil {
		// File absent — return safe minimal, not the production default.
		return safeDefaultModelConfig()
	}

	var mc ModelConfig
	if json.Unmarshal(b, &mc) != nil {
		fmt.Fprintf(os.Stderr, "[WARN] models.json is corrupt — attempting recovery from backup\n")
		appendMeraLog("WARN", "models.json corrupt, trying backup")
		var bkMC ModelConfig
		if bak, bakErr := os.ReadFile(bakPath); bakErr == nil && json.Unmarshal(bak, &bkMC) == nil {
			fmt.Fprintf(os.Stderr, "[WARN] Recovered models from models.json.bak\n")
			appendMeraLog("WARN", "recovered models from .bak")
			return bkMC
		}
		fmt.Fprintf(os.Stderr, "[WARN] Backup unavailable or also corrupt — using safe minimal defaults\n")
		appendMeraLog("WARN", "models recovery failed, using safe minimal defaults")
		return safeDefaultModelConfig()
	}

	// Initialise Models map if nil.
	if mc.Models == nil {
		mc.Models = map[string]string{}
	}

	// Fill any missing roles using the most-common model already in the config.
	// This preserves the user's profile intent instead of injecting arbitrary defaults.
	// Special case: RoleCodeEdit always falls back to "qwen2.5-coder:7b" (not the primary
	// model) because it is specifically the fast/reliable Aider edit model — using a large
	// analytical model here defeats its purpose and was the source of a latency regression
	// when older models.json files were migrated without the codeedit key.
	primaryModel := "qwen2.5-coder:7b"
	for _, m := range mc.Models {
		if m != "" {
			primaryModel = m
			break
		}
	}
	roleDefaults := map[string]string{
		RoleCodeEdit: "qwen2.5-coder:7b", // always small + fast — never fall back to analytical model
	}
	allRoles := []string{RolePlanner, RoleArchitect, RoleFileScout, RoleCode, RoleCodeEdit, RoleDiffReview, RoleSecurity, RoleSprintAdvisor}
	for _, role := range allRoles {
		if mc.Models[role] == "" {
			if d, ok := roleDefaults[role]; ok {
				mc.Models[role] = d
			} else {
				mc.Models[role] = primaryModel
			}
		}
	}

	// If profile is missing, default to FAST (not NORMAL) — conservative.
	if mc.Profile == "" {
		mc.Profile = ProfileFast
	}

	// Schema migration — backup before rewriting.
	if mc.SchemaVersion < ModelsSchemaVersion {
		_ = os.WriteFile(bakPath, b, 0644)
		mc.SchemaVersion = ModelsSchemaVersion
		if b2, err := json.MarshalIndent(mc, "", "  "); err == nil {
			_ = writeNoBOM(path, b2)
		}
	}
	return mc
}

func saveModelConfig(mc ModelConfig) error {
	b, err := json.MarshalIndent(mc, "", "  ")
	if err != nil {
		return err
	}
	return writeNoBOM(modelConfigPath(), b)
}

// ── Profile settings ───────────────────────────────────────────────────────────

// ProfileSettings drives timeouts, context depth, and agent policy for a given profile.
type ProfileSettings struct {
	AgentTimeout        time.Duration // non-streaming Ollama calls (intent score, etc.)
	FileScoutTimeout    time.Duration // File Scout specifically — shorter to avoid 120s hangs
	StreamTimeout       time.Duration // streaming Ollama calls (Planner, Architect, etc.)
	NarrowBugfixTimeout time.Duration // hard cap on any Ollama call during BUGFIX_NARROW fast path
	MaxFiles            int           // max files passed to Aider
	MaxFileLines        int           // max lines injected per file in session document
	SkipSprint          bool          // skip sprint suggestions for speed
	ExtraGating         bool          // STRICT: additional acknowledgement before code phase
	DeeperArchitect     bool          // DEEP/STRICT: extended architect analysis prompt
}

func profileSettings(p PerformanceProfile) ProfileSettings {
	switch p {
	case ProfileFast:
		return ProfileSettings{
			AgentTimeout:        60 * time.Second,
			FileScoutTimeout:    45 * time.Second, // hard cap: never hang 120s in FAST
			StreamTimeout:       30 * time.Second,
			NarrowBugfixTimeout: 30 * time.Second, // FAST already capped; apply same
			MaxFiles:            3,
			MaxFileLines:        60,
			SkipSprint:          true,
			ExtraGating:         false,
			DeeperArchitect:     false,
		}
	case ProfileDeep:
		return ProfileSettings{
			AgentTimeout:        300 * time.Second,
			FileScoutTimeout:    300 * time.Second,
			StreamTimeout:       180 * time.Second,
			NarrowBugfixTimeout: 90 * time.Second, // deeper analysis allowed in DEEP
			MaxFiles:            8,
			MaxFileLines:        200,
			SkipSprint:          false,
			ExtraGating:         false,
			DeeperArchitect:     true,
		}
	case ProfileStrict:
		return ProfileSettings{
			AgentTimeout:        300 * time.Second,
			FileScoutTimeout:    300 * time.Second,
			StreamTimeout:       180 * time.Second,
			NarrowBugfixTimeout: 90 * time.Second,
			MaxFiles:            8,
			MaxFileLines:        150,
			SkipSprint:          false,
			ExtraGating:         true,
			DeeperArchitect:     true,
		}
	default: // NORMAL
		return ProfileSettings{
			AgentTimeout:        120 * time.Second,
			FileScoutTimeout:    120 * time.Second,
			StreamTimeout:       90 * time.Second,
			NarrowBugfixTimeout: 45 * time.Second, // hard cap: no NORMAL agent blocks >45s for narrow bugfix
			MaxFiles:            6,
			MaxFileLines:        120,
			SkipSprint:          false,
			ExtraGating:         false,
			DeeperArchitect:     false,
		}
	}
}

// activeProfile returns the currently configured performance profile.
func activeProfile() PerformanceProfile { return loadModelConfig().Profile }

// getProfileSettings returns the ProfileSettings for the active profile.
func getProfileSettings() ProfileSettings { return profileSettings(activeProfile()) }

// effectiveFileLimit returns the profile-capped file limit for Aider targeting.
func effectiveFileLimit() int { return getProfileSettings().MaxFiles }

// ── AIProvider interface ───────────────────────────────────────────────────────

// AIProvider abstracts local and future cloud AI backends.
// OllamaProvider is the default; OpenRouter/Groq/Gemini can implement this interface.
type AIProvider interface {
	Generate(model, prompt string, stream bool, timeout time.Duration) (string, error)
	IsAvailable() bool
	Name() string
}

// OllamaProvider implements AIProvider for the local Ollama server.
type OllamaProvider struct{}

func (o OllamaProvider) Generate(model, prompt string, stream bool, timeout time.Duration) (string, error) {
	if stream {
		return ollamaStream(model, prompt, timeout)
	}
	return ollamaCall(model, prompt, timeout)
}

func (o OllamaProvider) IsAvailable() bool { return ensureOllama() == nil }
func (o OllamaProvider) Name() string       { return "Ollama (local)" }

// defaultProvider is the active AI backend. Swap this to route to a cloud provider.
var defaultProvider AIProvider = OllamaProvider{}

// ── Model resolution ───────────────────────────────────────────────────────────

// listOllamaModels returns all model names installed in the local Ollama instance.
func listOllamaModels() []string {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:11434/api/tags")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &result) != nil {
		return nil
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names
}

// isModelAvailable checks whether a model is installed in the local Ollama instance.
// Matching is prefix-based: "phi4" matches "phi4:latest".
func isModelAvailable(model string) bool {
	modelLower := strings.ToLower(model)
	for _, m := range listOllamaModels() {
		ml := strings.ToLower(m)
		if ml == modelLower || strings.HasPrefix(ml, modelLower+":") {
			return true
		}
	}
	return false
}

// modelForRole resolves the model to use for a given agent role.
//
// Fallback chain:
//  1. Configured role model (if available in Ollama)
//  2. Default role model (if different and available)
//  3. First available Ollama model
//  4. config.json DefaultModel (last resort — may fail at Ollama level, never crashes)
func modelForRole(role string) string {
	mc := loadModelConfig()
	assigned := mc.Models[role]

	if assigned != "" && isModelAvailable(assigned) {
		return assigned
	}
	if assigned != "" {
		fmt.Printf("[WARN] Model %q for role %q not found — trying fallbacks\n", assigned, role)
		appendMeraLog("FALLBACK", "model="+assigned+" for role="+role+" not installed — trying fallbacks")
	}

	// Fallback 1: default for this role
	defModel := defaultModelConfig().Models[role]
	if defModel != "" && defModel != assigned && isModelAvailable(defModel) {
		fmt.Printf("[WARN] Using default role model: %s\n", defModel)
		appendMeraLog("FALLBACK", "role="+role+" using default model="+defModel)
		return defModel
	}

	// Fallback 2: first available model
	available := listOllamaModels()
	if len(available) > 0 {
		fmt.Printf("[WARN] Falling back to first available model: %s\n", available[0])
		appendMeraLog("FALLBACK", "role="+role+" using first-available model="+available[0])
		return available[0]
	}

	// Fallback 3: config default (offline — Ollama will surface the error)
	cfg := loadConfig()
	return cfg.DefaultModel
}

// generateForRole calls the active AI provider with the role's model and profile timeout.
// The File Scout role uses FileScoutTimeout (shorter) to prevent 120-second hangs in FAST mode.
// Returns (output, model-used, error).
func generateForRole(role, prompt string, stream bool) (string, string, error) {
	model := modelForRole(role)
	ps := getProfileSettings()
	var timeout time.Duration
	switch {
	case stream:
		timeout = ps.StreamTimeout
	case role == RoleFileScout:
		timeout = ps.FileScoutTimeout
	default:
		timeout = ps.AgentTimeout
	}
	out, err := defaultProvider.Generate(model, prompt, stream, timeout)
	return out, model, err
}

// generateForRoleCapped is identical to generateForRole but enforces a hard maximum timeout.
// Used for BUGFIX_NARROW tasks where no analysis agent should block longer than
// NarrowBugfixTimeout regardless of the active profile.
// If maxTimeout is 0 the cap is ignored and profileTimeout wins.
func generateForRoleCapped(role, prompt string, stream bool, maxTimeout time.Duration) (string, string, error) {
	model := modelForRole(role)
	ps := getProfileSettings()
	var baseTimeout time.Duration
	switch {
	case stream:
		baseTimeout = ps.StreamTimeout
	case role == RoleFileScout:
		baseTimeout = ps.FileScoutTimeout
	default:
		baseTimeout = ps.AgentTimeout
	}
	timeout := baseTimeout
	if maxTimeout > 0 && maxTimeout < baseTimeout {
		timeout = maxTimeout
		fmt.Printf("[MERA] %s timeout capped at %s for BUGFIX_NARROW (profile: %s)\n",
			role, maxTimeout.Round(time.Second), activeProfile())
	}
	out, err := defaultProvider.Generate(model, prompt, stream, timeout)
	return out, model, err
}

// modelModeLabel returns a human-readable label describing the model configuration mode.
// "Single-model fallback" is printed when all roles share one model (e.g. Minimal install).
func modelModeLabel(mc ModelConfig) string {
	first := ""
	for _, m := range mc.Models {
		if m == "" {
			continue
		}
		if first == "" {
			first = m
		} else if m != first {
			return "Multi-model"
		}
	}
	if first == "" {
		return "No models configured"
	}
	return "Single-model fallback (" + first + ")"
}

// ── Doctor model check ─────────────────────────────────────────────────────────

// checkModelsForDoctor prints per-role model availability and a profile compliance summary.
// Called from runDoctor() to surface missing models before the user runs -Code.
func checkModelsForDoctor() {
	mc := loadModelConfig()
	available := listOllamaModels()

	// Build a fast-lookup set — both "phi4" and "phi4:latest" forms.
	availSet := map[string]bool{}
	for _, m := range available {
		ml := strings.ToLower(m)
		availSet[ml] = true
		if idx := strings.IndexByte(m, ':'); idx >= 0 {
			availSet[strings.ToLower(m[:idx])] = true
		}
	}

	fmt.Printf("  Active profile: %s\n", mc.Profile)
	roles := []string{RolePlanner, RoleArchitect, RoleFileScout, RoleCode, RoleCodeEdit, RoleDiffReview, RoleSecurity, RoleSprintAdvisor}

	seen := map[string]bool{}
	var missingUniq []string
	for _, role := range roles {
		model := mc.Models[role]
		ml := strings.ToLower(model)
		if availSet[ml] || availSet[strings.SplitN(ml, ":", 2)[0]] {
			fmt.Printf("[OK]   %-14s model: %s\n", role, model)
		} else {
			fmt.Printf("[WARN] %-14s model missing: %s\n", role, model)
			if !seen[model] {
				seen[model] = true
				missingUniq = append(missingUniq, model)
			}
		}
	}

	// Profile compliance summary.
	fmt.Println()
	if len(missingUniq) == 0 {
		fmt.Printf("[OK]   %s profile — all models installed\n", mc.Profile)
	} else {
		fmt.Printf("[WARN] %s profile incomplete — %d model(s) missing:\n", mc.Profile, len(missingUniq))
		for _, m := range missingUniq {
			fmt.Printf("         ollama pull %s\n", m)
		}
	}
}

// ── CLI commands ───────────────────────────────────────────────────────────────

// printModels displays the full model configuration and profile settings.
func printModels() {
	mc := loadModelConfig()
	available := listOllamaModels()

	availSet := map[string]bool{}
	for _, m := range available {
		ml := strings.ToLower(m)
		availSet[ml] = true
		if idx := strings.IndexByte(m, ':'); idx >= 0 {
			availSet[strings.ToLower(m[:idx])] = true
		}
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA]  Model Configuration")
	fmt.Println("[MERA] ========================================")
	fmt.Printf("  Active profile: %s\n\n", mc.Profile)

	roles := []string{RolePlanner, RoleArchitect, RoleFileScout, RoleCode, RoleCodeEdit, RoleDiffReview, RoleSecurity, RoleSprintAdvisor}
	for _, role := range roles {
		model := mc.Models[role]
		ml := strings.ToLower(model)
		status := "OK"
		if !availSet[ml] && !availSet[strings.SplitN(ml, ":", 2)[0]] {
			status = "MISSING"
		}
		fmt.Printf("  %-14s -> %-30s %s\n", role+":", model, status)
	}

	ps := getProfileSettings()
	fmt.Println("\n  Profile settings:")
	fmt.Printf("    Stream timeout  : %v\n", ps.StreamTimeout)
	fmt.Printf("    Agent timeout   : %v\n", ps.AgentTimeout)
	fmt.Printf("    Max files       : %d\n", ps.MaxFiles)
	fmt.Printf("    Max file lines  : %d\n", ps.MaxFileLines)
	fmt.Printf("    Skip sprint     : %v\n", ps.SkipSprint)
	fmt.Printf("    Extra gating    : %v\n", ps.ExtraGating)
	fmt.Printf("    Deeper architect: %v\n", ps.DeeperArchitect)

	if len(available) > 0 {
		sort.Strings(available)
		fmt.Println("\n  Available local models:")
		for _, m := range available {
			fmt.Println("   ", m)
		}
	}
	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA] Config: %s\n", modelConfigPath())
}

// setModelForRole assigns a model to an agent role and persists the change.
func setModelForRole(role, model string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	validRoles := map[string]bool{
		RolePlanner: true, RoleArchitect: true, RoleFileScout: true,
		RoleCode: true, RoleCodeEdit: true, RoleDiffReview: true, RoleSecurity: true, RoleSprintAdvisor: true,
	}
	if !validRoles[role] {
		return fmt.Errorf("unknown role %q\nValid roles: planner, architect, filescout, code, codeedit, diffreview, security, sprintadvisor", role)
	}

	mc := loadModelConfig()
	old := mc.Models[role]
	mc.Models[role] = model
	if err := saveModelConfig(mc); err != nil {
		return err
	}

	fmt.Printf("[OK]  %s: %s -> %s\n", role, old, model)
	if !isModelAvailable(model) {
		fmt.Printf("[WARN] %q is not installed in Ollama.\n", model)
		fmt.Printf("[WARN] Pull it first: ollama pull %s\n", model)
	}
	return nil
}

// setActiveProfile changes the performance profile and persists it.
func setActiveProfile(profile string) error {
	p := PerformanceProfile(strings.ToUpper(strings.TrimSpace(profile)))
	valid := map[PerformanceProfile]bool{
		ProfileFast: true, ProfileNormal: true, ProfileDeep: true, ProfileStrict: true,
	}
	if !valid[p] {
		return fmt.Errorf("unknown profile %q\nValid profiles: FAST, NORMAL, DEEP, STRICT", profile)
	}

	mc := loadModelConfig()
	old := mc.Profile
	mc.Profile = p
	if err := saveModelConfig(mc); err != nil {
		return err
	}

	ps := profileSettings(p)
	fmt.Printf("[OK]  Profile: %s -> %s\n", old, p)
	fmt.Printf("      Timeouts: stream=%v, agent=%v | Max files: %d | Skip sprint: %v\n",
		ps.StreamTimeout, ps.AgentTimeout, ps.MaxFiles, ps.SkipSprint)
	return nil
}

// printProfileMode prints the active profile settings in detail.
func printProfileMode() {
	mc := loadModelConfig()
	ps := getProfileSettings()

	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA]  Runtime profile  : %s\n", mc.Profile)
	fmt.Printf("[MERA]  Model mode       : %s\n", modelModeLabel(mc))
	fmt.Println("[MERA] ========================================")

	profiles := []PerformanceProfile{ProfileFast, ProfileNormal, ProfileDeep, ProfileStrict}
	descriptions := map[PerformanceProfile]string{
		ProfileFast:   "lightweight models, small context, reduced scans — optimised for speed",
		ProfileNormal: "balanced — default for everyday engineering tasks",
		ProfileDeep:   "larger context, extended analysis, better architectural reasoning",
		ProfileStrict: "maximum safety gating, deeper analysis, explicit confirmations required",
	}

	for _, p := range profiles {
		marker := "  "
		if p == mc.Profile {
			marker = "> "
		}
		fmt.Printf("\n %s%s — %s\n", marker, p, descriptions[p])
		s := profileSettings(p)
		fmt.Printf("    stream=%v  agent=%v  files=%d  lines=%d  sprint-skip=%v  extra-gate=%v\n",
			s.StreamTimeout, s.AgentTimeout, s.MaxFiles, s.MaxFileLines, s.SkipSprint, s.ExtraGating)
	}

	fmt.Println()
	fmt.Printf("  Active settings:\n")
	fmt.Printf("    Stream timeout       : %v\n", ps.StreamTimeout)
	fmt.Printf("    Agent timeout        : %v\n", ps.AgentTimeout)
	fmt.Printf("    File Scout timeout   : %v\n", ps.FileScoutTimeout)
	fmt.Printf("    Max files            : %d\n", ps.MaxFiles)
	fmt.Printf("    Max file lines       : %d\n", ps.MaxFileLines)
	fmt.Printf("    Skip sprint          : %v\n", ps.SkipSprint)
	fmt.Printf("    Extra gating         : %v\n", ps.ExtraGating)
	fmt.Printf("    Deeper architect     : %v\n", ps.DeeperArchitect)

	fmt.Println("\n[MERA] ========================================")
	fmt.Println("[MERA] Change with: mera -SetProfile FAST|NORMAL|DEEP|STRICT")
	if modelModeLabel(mc) != "Multi-model" {
		fmt.Println("[MERA] Single-model mode: upgrade with 'ollama pull phi4 && ollama pull llama3.1:8b'")
		fmt.Println("[MERA]   then: mera -SetProfile NORMAL")
	}
}
