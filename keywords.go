package main

import (
	"regexp"
	"strings"
)

// ── Generic Bugfix Keywords ────────────────────────────────────────────────
// Used for BUGFIX_NARROW detection across all domains (not just auth)

var genericBugfixKeywords = []string{
	"null", "nil", "undefined", "error", "fail", "crash", "panic",
	"boundary", "edge case", "off-by-one", "race condition", "deadlock",
	"memory leak", "overflow", "underflow", "timeout",
	"incorrect", "wrong", "broken", "malformed", "corrupt",
}

var genericMethodKeywords = []string{
	"method", "function", "handler", "endpoint", "route",
	"calculate", "compute", "parse", "convert", "format",
	"validate", "verify", "check", "assert",
}

var architectureKeywords = []string{
	"refactor", "redesign", "rewrite", "migration", "breaking",
	"deprecated", "performance", "scalability", "monitoring", "observability",
}

// ── Intent Classification Constants ─────────────────────────────────────
// Generic domain-agnostic intent types

type GenericTaskIntent string

const (
	IntentSingleComponentFix  GenericTaskIntent = "SINGLE_COMPONENT_FIX"  // "Fix Calculator Add method"
	IntentServiceLayerFix     GenericTaskIntent = "SERVICE_LAYER_FIX"     // "Fix in XService"
	IntentCrossCuttingFix     GenericTaskIntent = "CROSS_CUTTING_FIX"     // "Fix in all controllers"
	IntentGenericUnclassified GenericTaskIntent = "GENERAL"               // No clear pattern
)

// ── Symbol Extraction ──────────────────────────────────────────────────────

// extractSymbolsFromTask extracts capitalized words (class/method names) from a task string.
// Example: "Fix Calculator Add method" → ["Calculator", "Add"]
// Filters out common English words and abbreviations.
func extractSymbolsFromTask(task string) []string {
	// Find all capitalized words
	re := regexp.MustCompile(`[A-Z][a-zA-Z0-9]*`)
	symbols := re.FindAllString(task, -1)

	// Filter out common English words
	commonWords := map[string]bool{
		"Fix": true, "The": true, "A": true, "An": true, "And": true,
		"Or": true, "In": true, "By": true, "To": true, "From": true,
		"For": true, "With": true, "On": true, "At": true, "If": true,
	}

	var result []string
	seen := make(map[string]bool)
	for _, s := range symbols {
		// Skip if: already seen, is a common word, or too short
		if seen[s] || commonWords[s] || len(s) <= 2 {
			continue
		}
		seen[s] = true
		result = append(result, s)
	}
	return result
}

// containsSymbol checks if a file path or content contains a symbol (case-insensitive).
func containsSymbol(text, symbol string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(symbol))
}

// extractTopLevelModule extracts the top-level module name from a relative path.
// Example: "Admin/Controllers/UserController.cs" → "Admin"
// Example: "MeraTest/Calculator.cs" → "MeraTest"
func extractTopLevelModule(relPath string) string {
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(relPath, "\\", "/")), "/")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

// isSameModule checks if two module names refer to the same module.
// Examples:
//   isSameModule("MeraTest", "MeraTest") → true
//   isSameModule("identity", "Identity") → true
//   isSameModule("admin", "AdminService") → true (partial match)
func isSameModule(mod1, mod2 string) bool {
	m1 := strings.ToLower(mod1)
	m2 := strings.ToLower(mod2)
	return m1 == m2 ||
		strings.Contains(m1, m2) ||
		strings.Contains(m2, m1)
}

// isCommonInfrastructure returns true if a module name indicates infrastructure/shared code.
func isCommonInfrastructure(module string) bool {
	infra := []string{
		"logging", "monitoring", "metrics", "config", "shared",
		"common", "utility", "util", "helper", "base",
	}
	mod := strings.ToLower(module)
	for _, i := range infra {
		if strings.Contains(mod, i) {
			return true
		}
	}
	return false
}

// containsAny checks if a string contains any of the given substrings (case-insensitive).
func containsAny(text string, keywords []string) bool {
	t := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(t, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// ── Generic Intent Classification ──────────────────────────────────────────

// classifyTaskIntentGeneric performs domain-agnostic intent classification.
// Returns IntentSingleComponentFix, IntentServiceLayerFix, IntentCrossCuttingFix, or GENERAL.
func classifyTaskIntentGeneric(task, target string) GenericTaskIntent {
	t := strings.ToLower(task)

	// Single component fix: task mentions specific class/method + "Fix"
	symbols := extractSymbolsFromTask(task)
	if strings.HasPrefix(t, "fix ") && len(symbols) > 0 {
		// Check if task is clearly about a single component
		if !containsAny(t, []string{"all ", "every ", "multiple ", "throughout", "everywhere"}) {
			return IntentSingleComponentFix
		}
	}

	// Service layer fix: mentions "service" + specific service name
	if strings.Contains(t, "service") && len(symbols) > 0 {
		// If it's not cross-cutting, it's a service-layer fix
		if !containsAny(t, []string{"all ", "every ", "multiple ", "throughout"}) {
			return IntentServiceLayerFix
		}
	}

	// Cross-cutting fix: "all", "every", "multiple", "throughout"
	if containsAny(t, []string{"all ", "every ", "multiple ", "throughout", "everywhere"}) {
		return IntentCrossCuttingFix
	}

	return IntentGenericUnclassified
}

// ── Method/Class Detection (Simple Regex) ──────────────────────────────────

// detectClassDefinitions scans content for class definitions matching a symbol.
// Supports C#, Go, Python, JavaScript patterns.
// Returns the count of matches found.
func detectClassDefinitions(content, symbol string) int {
	patterns := []string{
		`(?i)class\s+` + symbol + `\b`,           // C#: class Calculator
		`(?i)type\s+` + symbol + `\s+struct\b`,   // Go: type Calculator struct
		`(?i)type\s+` + symbol + `\s+interface\b`, // Go: type Calculator interface
		`(?i)class\s+` + symbol + `(\(|:|\s)`,    // Python: class Calculator:
		`(?i)(?:export\s+)?(?:class|interface)\s+` + symbol + `\b`, // TypeScript/JS
	}

	count := 0
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		if re.MatchString(content) {
			count++
		}
	}
	return count
}

// detectMethodDefinitions scans content for method/function definitions matching a symbol.
// Returns the count of matches found.
func detectMethodDefinitions(content, symbol string) int {
	patterns := []string{
		`(?i)public\s+\w+\s+` + symbol + `\s*\(`,           // C#: public int Add(
		`(?i)private\s+\w+\s+` + symbol + `\s*\(`,          // C#: private int Add(
		`(?i)func\s+\(.*?\)\s+` + symbol + `\s*\(`,         // Go: func (c *Calc) Add(
		`(?i)func\s+` + symbol + `\s*\(`,                   // Go: func Add(
		`(?i)def\s+` + symbol + `\s*\(`,                    // Python: def Add(
		`(?i)(?:async\s+)?(?:function|const)\s+` + symbol + `\s*(?:\(|=)`, // JS/TS
	}

	count := 0
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		if re.MatchString(content) {
			count++
		}
	}
	return count
}

// ── Noise Filtering (Generic) ──────────────────────────────────────────────

var genericNoiseKeywords = []string{
	// Infrastructure/meta files
	"swagger", "openapi", "schema", "entity-framework", "migration",
	"appsettings", "configuration", "startup",

	// Framework boilerplate
	"middleware", "interceptor", "filter", "validator",
	"exception", "error-handler", "logging",

	// Testing/mocks
	"test", "mock", "fixture", "stub", "example",

	// Documentation
	"readme", "example", "demo", "sample",
}

// isNoiseFile checks if a file should be filtered out for BUGFIX_NARROW tasks.
func isNoiseFile(relPath string) bool {
	nameLower := strings.ToLower(relPath)
	for _, nw := range genericNoiseKeywords {
		if strings.Contains(nameLower, nw) {
			return true
		}
	}
	return false
}
