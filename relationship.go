package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// RejectedCandidate records a scored file that was excluded from the final selection.
// Shown in mera -ExplainSelection to explain WHY a file was not chosen.
type RejectedCandidate struct {
	RelPath string `json:"relPath"`
	Score   int    `json:"score"`
	Reason  string `json:"reason"`
}

// endpointKeyword extracts the primary REST endpoint name from a lowercased task string.
// Returns the first matching keyword: login, register, refresh, logout, reset, profile, etc.
func endpointKeyword(taskLower string) string {
	for _, ep := range []string{
		"login", "register", "refresh", "logout", "reset",
		"profile", "password", "verify", "confirm",
	} {
		if strings.Contains(taskLower, ep) {
			return ep
		}
	}
	return ""
}

// findEndpointReferences reads a controller file and returns two ref maps:
//
//   - dtoRefs:     lowercase base-names of types found in the target endpoint's signature/body
//   - serviceRefs: lowercase service/interface names injected via private readonly fields
//     (also adds the implementation name when an interface name like IXxx is found)
//
// Both maps are used by applyRelationshipBoosts to boost referenced files and penalise
// sibling DTOs that are not consumed by the target endpoint.
func findEndpointReferences(controllerPath, endpointKw string) (dtoRefs map[string]bool, serviceRefs map[string]bool) {
	data, err := os.ReadFile(controllerPath)
	if err != nil {
		return nil, nil
	}

	lines := strings.Split(string(data), "\n")
	dtoRefs = map[string]bool{}
	serviceRefs = map[string]bool{}

	// ── Phase 1: class-level DI field declarations ────────────────────────
	// Scan "private readonly IXxx _xxx;" lines to discover injected services.
	for _, line := range lines {
		ll := strings.ToLower(strings.TrimSpace(line))
		if !strings.Contains(ll, "private") || !strings.Contains(ll, "readonly") {
			continue
		}
		words := strings.Fields(strings.TrimSpace(line))
		for i, w := range words {
			if strings.ToLower(w) == "readonly" && i+1 < len(words) {
				typeName := strings.Trim(words[i+1], "<>;,()")
				if len(typeName) < 3 {
					continue
				}
				tl := strings.ToLower(typeName)
				serviceRefs[tl] = true
				// Add implementation name alongside interface (IAuthService → authservice).
				if strings.HasPrefix(typeName, "I") && len(typeName) > 1 &&
					unicode.IsUpper(rune(typeName[1])) {
					serviceRefs[tl[1:]] = true
				}
			}
		}
	}

	// ── Phase 2: target endpoint method body ─────────────────────────────
	// Locate the [HttpPost("endpointKw")] annotation and scan the method body
	// to collect DTO parameter types actually consumed by this endpoint.
	if endpointKw == "" {
		return dtoRefs, serviceRefs
	}

	inTarget := false
	depth := 0
	enteredBody := false

	for _, line := range lines {
		ll := strings.ToLower(strings.TrimSpace(line))

		if !inTarget {
			// Match [HttpPost("login")] or similar — case-insensitive.
			if strings.Contains(ll, "[http") && strings.Contains(ll, endpointKw) {
				inTarget = true
			}
			continue
		}

		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")
		if opens > 0 {
			enteredBody = true
		}
		depth += opens - closes

		// Extract PascalCase identifiers — likely type names used in the method.
		for _, word := range tokenizeLine(line) {
			if len(word) >= 4 && unicode.IsUpper(rune(word[0])) {
				dtoRefs[strings.ToLower(word)] = true
			}
		}

		// Stop once the method body has fully closed.
		if enteredBody && depth <= 0 {
			break
		}
	}

	return dtoRefs, serviceRefs
}

// tokenizeLine splits a source line into alpha-numeric word tokens.
func tokenizeLine(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// applyRelationshipBoosts adjusts FileEvidence scores based on endpoint reference analysis.
// Only applied when scope is ScopeBugfixNarrow.
//
// Rules:
//
//	Service in serviceRefs + is implementation → +10  "service used by endpoint"
//	Service in serviceRefs + is interface       → –5   "interface — implementation preferred"
//	DTO in dtoRefs                              → +10  "DTO referenced by endpoint"
//	Auth DTO NOT in dtoRefs (sibling)           → –15  "not referenced by endpoint"
func applyRelationshipBoosts(
	scored []FileEvidence,
	dtoRefs map[string]bool,
	serviceRefs map[string]bool,
	scope TaskScope,
	intent TaskIntent,
) []FileEvidence {
	if scope != ScopeBugfixNarrow || (len(dtoRefs) == 0 && len(serviceRefs) == 0) {
		return scored
	}

	// Build interface→implementation map from the candidate list.
	// If both "iauthservice" and "authservice" are in the set, the "i" prefixed one is the interface.
	baseNames := map[string]bool{}
	for _, ev := range scored {
		b := strings.ToLower(strings.TrimSuffix(filepath.Base(ev.RelPath), filepath.Ext(ev.RelPath)))
		baseNames[b] = true
	}
	interfaceSet := map[string]bool{}
	for b := range baseNames {
		if strings.HasPrefix(b, "i") && len(b) > 2 && baseNames[b[1:]] {
			interfaceSet[b] = true // "iauthservice" is an interface because "authservice" also exists
		}
	}

	for i := range scored {
		ext := filepath.Ext(scored[i].RelPath)
		baseName := strings.ToLower(strings.TrimSuffix(filepath.Base(scored[i].RelPath), ext))

		// ── Service scoring ───────────────────────────────────────────────
		if serviceRefs[baseName] {
			if interfaceSet[baseName] {
				// Interface file: small penalty — prefer the implementation for bug fixes.
				scored[i].Confidence = clamp(scored[i].Confidence-5, 0, 100)
				scored[i].Reasons = append(scored[i].Reasons,
					"service referenced (interface — implementation preferred for bugfix) (-5)")
			} else {
				// Concrete service implementation: boost it.
				scored[i].Confidence = clamp(scored[i].Confidence+10, 0, 100)
				scored[i].Reasons = append(scored[i].Reasons,
					"service used by login endpoint (+10)")
			}
			continue
		}

		// ── DTO scoring ───────────────────────────────────────────────────
		if dtoRefs[baseName] {
			scored[i].Confidence = clamp(scored[i].Confidence+10, 0, 100)
			scored[i].Reasons = append(scored[i].Reasons,
				"DTO referenced by login endpoint signature (+10)")
			continue
		}

		// ── Sibling auth DTO penalty ──────────────────────────────────────
		// Penalise auth DTOs that are NOT referenced by the target endpoint.
		if intent == IntentStandardLogin || intent == IntentAuthGeneral {
			for _, kw := range []string{"request", "response", "dto"} {
				if strings.Contains(baseName, kw) {
					scored[i].Confidence = clamp(scored[i].Confidence-15, 0, 100)
					scored[i].Reasons = append(scored[i].Reasons,
						"auth DTO not referenced by login endpoint — sibling excluded (-15)")
					break
				}
			}
		}
	}

	return scored
}

// rejectionReason returns a human-readable explanation for why a candidate was excluded.
func rejectionReason(ev FileEvidence, scope TaskScope, intent TaskIntent) string {
	baseName := strings.ToLower(strings.TrimSuffix(filepath.Base(ev.RelPath), filepath.Ext(ev.RelPath)))
	if scope == ScopeBugfixNarrow {
		for _, kw := range []string{"request", "response", "dto"} {
			if strings.Contains(baseName, kw) {
				return "auth DTO not referenced by login endpoint — BUGFIX_NARROW"
			}
		}
		if strings.HasPrefix(baseName, "i") && len(baseName) > 2 {
			return "interface file — BUGFIX_NARROW prefers implementation"
		}
		return "below BUGFIX_NARROW file cap — lower endpoint relevance"
	}
	return fmt.Sprintf("confidence %d%% — below top-%d threshold", ev.Confidence, effectiveFileLimit())
}
