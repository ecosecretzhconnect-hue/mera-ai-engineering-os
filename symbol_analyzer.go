package main

import (
	"regexp"
	"strings"
)

// ── Symbol Analysis Module ─────────────────────────────────────────────────
// Provides language-agnostic extraction of class/method/type references from source code.
// Used by Phase 10.14 for exact symbol matching in files.

// SymbolMatch represents a single symbol detected in a file.
type SymbolMatch struct {
	Symbol   string // The symbol name (e.g., "Calculator", "Add")
	Type     string // "class", "method", "function", "interface"
	Line     int    // Line number where found (if available)
	Strength int    // Confidence 0-100 (100 = exact definition, 50 = reference)
}

// findSymbolMatches scans file content for occurrences of symbols.
// Returns a map of symbol → []SymbolMatch for all symbols found in the content.
func findSymbolMatches(content string, symbols []string) map[string][]SymbolMatch {
	results := make(map[string][]SymbolMatch)

	for _, symbol := range symbols {
		var matches []SymbolMatch

		// Check for exact class/type definitions (highest strength = 100)
		if count := detectClassDefinitions(content, symbol); count > 0 {
			matches = append(matches, SymbolMatch{
				Symbol:   symbol,
				Type:     "class",
				Strength: 100,
			})
		}

		// Check for exact method/function definitions (strength = 95)
		if count := detectMethodDefinitions(content, symbol); count > 0 {
			matches = append(matches, SymbolMatch{
				Symbol:   symbol,
				Type:     "method",
				Strength: 95,
			})
		}

		// Check for references to the symbol in code (strength = 60)
		// Look for patterns like: symbol.something, new Symbol, Symbol.method()
		refPatterns := []string{
			`\b` + symbol + `\s*\(`,        // Function call
			`\b` + symbol + `\s*\.`,        // Property/method access
			`new\s+` + symbol + `\b`,       // Constructor
			`:\s*` + symbol + `\b`,         // Type annotation
			`<` + symbol + `>`,             // Generic type
			`\(\*` + symbol + `\)`,         // Go pointer cast
		}

		for _, pattern := range refPatterns {
			re, err := regexp.Compile(`(?i)` + pattern)
			if err != nil {
				continue
			}

			matches := re.FindAllStringIndex(content, -1)
			if len(matches) > 0 {
				// Count unique references (rough dedup by checking lines)
				seenLines := make(map[int]bool)
				for _, match := range matches {
					lineNum := countNewlinesBefore(content, match[0])
					if !seenLines[lineNum] {
						results[symbol] = append(results[symbol], SymbolMatch{
							Symbol:   symbol,
							Type:     "reference",
							Line:     lineNum,
							Strength: 60,
						})
						seenLines[lineNum] = true
					}
				}
			}
		}

		if len(matches) > 0 {
			results[symbol] = matches
		}
	}

	return results
}

// countNewlinesBefore counts the number of newlines before position `pos` in content.
func countNewlinesBefore(content string, pos int) int {
	if pos > len(content) {
		pos = len(content)
	}
	return strings.Count(content[:pos], "\n") + 1
}

// scoreSymbolMatches computes a composite score based on symbol match strength and count.
// Higher strength and more matches = higher score.
func scoreSymbolMatches(matches map[string][]SymbolMatch) int {
	if len(matches) == 0 {
		return 0
	}

	totalStrength := 0
	totalMatches := 0

	for _, matchList := range matches {
		for _, m := range matchList {
			totalStrength += m.Strength
			totalMatches++
		}
	}

	if totalMatches == 0 {
		return 0
	}

	// Average strength, capped at 100
	avgStrength := totalStrength / totalMatches
	if avgStrength > 100 {
		avgStrength = 100
	}

	// Boost based on match count (more symbols found = higher confidence)
	// Scale: 1-2 symbols found → base score, 3+ → +10 bonus
	countBonus := 0
	if len(matches) >= 3 {
		countBonus = 10
	} else if len(matches) >= 2 {
		countBonus = 5
	}

	return avgStrength + countBonus
}

// ── Multi-Symbol Scoring ─────────────────────────────────────────────────

// findAllSymbolsInContent scans file content for all provided symbols.
// Returns count of distinct symbols found in the file.
// High count indicates file is strongly relevant to all mentioned symbols.
func findAllSymbolsInContent(content string, symbols []string) int {
	found := 0
	contentLower := strings.ToLower(content)

	for _, sym := range symbols {
		symLower := strings.ToLower(sym)
		if strings.Contains(contentLower, symLower) {
			found++
		}
	}

	return found
}

// scoreMultiSymbolMatch computes a score for how many target symbols appear in a file.
// Used for multi-symbol matching (e.g., both "Calculator" and "Add" in same file).
// Returns:
//   - 0 points if no symbols found
//   - 10-40 points based on how many symbols are present
func scoreMultiSymbolMatch(content string, symbols []string) int {
	if len(symbols) == 0 {
		return 0
	}

	found := findAllSymbolsInContent(content, symbols)

	// Scoring:
	// 0 symbols found: 0
	// 1 symbol found: 10
	// 2 symbols found: 25
	// 3+ symbols found: 40
	switch found {
	case 0:
		return 0
	case 1:
		return 10
	case 2:
		return 25
	default:
		return 40
	}
}

// extractExactMatches returns symbols that have exact class or method definitions in the content.
func extractExactMatches(content string, symbols []string) []string {
	var exact []string

	for _, symbol := range symbols {
		classCount := detectClassDefinitions(content, symbol)
		methodCount := detectMethodDefinitions(content, symbol)

		if classCount > 0 || methodCount > 0 {
			exact = append(exact, symbol)
		}
	}

	return exact
}
