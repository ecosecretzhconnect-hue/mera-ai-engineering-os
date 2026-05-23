package main

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxFilesForAider           = 8
	lowConfidenceThreshold     = 40 // BLOCK if highest file is below this
	midConfidenceThreshold     = 60 // WARN if average is below this
	targetModuleBoost          = 35 // Massive boost for files in target module directory (Phase 10.14)
	unrelatedModulePenalty     = -30 // Severe penalty for files in unrelated modules (Phase 10.14)
	exactSymbolMatchBoost      = 20 // Boost for exact filename match to symbol
	exactClassDefinitionBoost  = 25 // Boost for exact class definition
	exactMethodDefinitionBoost = 20 // Boost for exact method definition
)

// readFileContent reads a file's content safely, returning empty string on error.
func readFileContent(absPath string) (string, error) {
	data, err := ioutil.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FileEvidence is the per-file result of the confidence engine.
// Every file MERA selects must justify itself through this struct.
type FileEvidence struct {
	Path       string   `json:"path"`       // absolute path
	RelPath    string   `json:"relPath"`    // slash-normalized relative path
	Confidence int      `json:"confidence"` // 0–100
	Reasons    []string `json:"reasons"`
	Snippet    string   `json:"snippet"` // first matching line from the file
}

// authTaskKeywords are words in a task that indicate an authentication / login focus.
var authTaskKeywords = []string{
	"auth", "login", "logout", "signin", "signup", "register",
	"jwt", "token", "refresh", "password", "otp", "forgot",
	"reset", "credential", "bearer", "identity",
}

// authFileKeywords are filename tokens that strongly match auth-focused tasks.
// A filename hit adds a substantial score boost.
var authFileKeywords = []string{
	"authcontroller", "loginrequest", "registerrequest", "refreshtoken",
	"forgotpassword", "resetpassword", "otpcontroller", "jwtservice",
	"authservice", "identityservice", "tokenservice", "loginresponse",
	"authmodel", "usercontroller",
}

// ── Task intent classifier ────────────────────────────────────────────────────

// TaskIntent represents the primary intent category of a task.
// Used to apply precision scoring: social-auth penalty, exact-DTO boost, route awareness.
type TaskIntent string

const (
	IntentStandardLogin TaskIntent = "STANDARD_LOGIN"
	IntentSocialLogin   TaskIntent = "SOCIAL_LOGIN"
	IntentPasswordReset TaskIntent = "PASSWORD_RESET"
	IntentOTP           TaskIntent = "OTP"
	IntentRegistration  TaskIntent = "REGISTRATION"
	IntentTokenRefresh  TaskIntent = "TOKEN_REFRESH"
	IntentExternalAuth  TaskIntent = "EXTERNAL_AUTH"
	IntentAuthGeneral   TaskIntent = "AUTH_GENERAL"
	IntentGeneral       TaskIntent = "GENERAL"
)

// socialAuthProviders are provider-specific keywords that shift intent from
// STANDARD_LOGIN to SOCIAL_LOGIN when present in the task.
var socialAuthProviders = []string{
	"google", "microsoft", "meta", "facebook", "twitter", "github",
	"apple", "oauth", "social", "external",
}

// socialFileVariants are filename substrings that identify social/external auth DTOs.
// Files matching these patterns are strongly penalised when intent is STANDARD_LOGIN.
var socialFileVariants = []string{
	"googleloginrequest", "microsoftloginrequest", "metaloginrequest",
	"facebookloginrequest", "twitterloginrequest", "githubloginrequest",
	"appleloginrequest", "sociallogin", "oauthlogin", "externalauth",
	"externalloginrequest",
}

// ── Task scope classifier ─────────────────────────────────────────────────────

// TaskScope classifies how broad or narrow the task is.
// BUGFIX_NARROW triggers tighter file caps and endpoint-level relationship analysis.
type TaskScope string

const (
	ScopeBugfixNarrow TaskScope = "BUGFIX_NARROW" // Fix a specific endpoint error
	ScopeGeneral      TaskScope = "GENERAL"        // Feature, refactor, or broad task
)

// classifyTaskScope returns BUGFIX_NARROW when the task targets a specific, narrow bug.
// Phase 10.14: Generic detection for any domain (not just HTTP endpoints).
// Signals: task begins or contains "fix" AND includes specific indicators:
//   - HTTP error (400, 404, 500) or error keyword
//   - HTTP endpoint (POST, GET, PUT, PATCH, DELETE)
//   - Specific method/class name (capitalized word like "Calculator" or "LoginRequest")
//   - Generic bugfix keyword (null, nil, off-by-one, boundary, etc.)
func classifyTaskScope(task string) TaskScope {
	t := strings.ToLower(task)
	isFix := strings.HasPrefix(t, "fix ") || strings.Contains(t, " fix ")

	// HTTP-specific signals (auth, controllers, endpoints)
	hasError := strings.Contains(t, "400") || strings.Contains(t, "404") ||
		strings.Contains(t, "500") || strings.Contains(t, "bad request") ||
		strings.Contains(t, "not found") || strings.Contains(t, "bug") ||
		strings.Contains(t, "broken") || strings.Contains(t, "error")
	hasEndpoint := strings.Contains(t, "post ") || strings.Contains(t, "get ") ||
		strings.Contains(t, " put ") || strings.Contains(t, "patch ") ||
		strings.Contains(t, "delete ") || strings.Contains(t, "endpoint")

	// Generic signals (Phase 10.14)
	hasSpecificSymbols := len(extractSymbolsFromTask(task)) > 0 // "Fix Calculator Add method"
	hasGenericBugKeyword := containsAny(t, genericBugfixKeywords) // "null", "nil", "off-by-one", etc.
	hasMethodKeyword := containsAny(t, []string{"method", "function", "return"}) // "Fix X method/function"

	// BUGFIX_NARROW triggers when:
	// 1. HTTP-specific fix (existing logic), OR
	// 2. Generic fix with specific symbols (Phase 10.14), OR
	// 3. Generic fix with method/function keyword + bugfix keyword
	if isFix && (hasError || hasEndpoint || (hasSpecificSymbols && hasMethodKeyword) || hasGenericBugKeyword) {
		return ScopeBugfixNarrow
	}

	return ScopeGeneral
}

// classifyTaskIntent returns the primary intent of a task string.
// More-specific checks run first so they take precedence over generic ones.
func classifyTaskIntent(task string) TaskIntent {
	t := strings.ToLower(task)

	// Social login — provider keywords override all standard-login signals.
	for _, p := range socialAuthProviders {
		if strings.Contains(t, p) {
			return IntentSocialLogin
		}
	}
	// External auth / SSO.
	if strings.Contains(t, "saml") || strings.Contains(t, " sso") || strings.Contains(t, "external auth") {
		return IntentExternalAuth
	}
	// Password reset.
	if strings.Contains(t, "password reset") || strings.Contains(t, "forgot password") ||
		strings.Contains(t, "reset password") || strings.Contains(t, "forgotpassword") {
		return IntentPasswordReset
	}
	// OTP / 2FA.
	if strings.Contains(t, "otp") || strings.Contains(t, "two factor") ||
		strings.Contains(t, "2fa") || strings.Contains(t, "verification code") ||
		strings.Contains(t, "one-time") {
		return IntentOTP
	}
	// Registration.
	if strings.Contains(t, "register") || strings.Contains(t, "signup") ||
		strings.Contains(t, "sign up") || strings.Contains(t, "registration") {
		return IntentRegistration
	}
	// Token refresh.
	if strings.Contains(t, "refresh token") || strings.Contains(t, "token refresh") ||
		strings.Contains(t, "refresh jwt") {
		return IntentTokenRefresh
	}
	// Standard login — covers: login, authcontroller, loginrequest, signin, 400 bad request in an auth context.
	if strings.Contains(t, "login") || strings.Contains(t, "loginrequest") ||
		strings.Contains(t, "authcontroller") || strings.Contains(t, "signin") {
		return IntentStandardLogin
	}
	// Generic auth.
	for _, kw := range authTaskKeywords {
		if strings.Contains(t, kw) {
			return IntentAuthGeneral
		}
	}
	return IntentGeneral
}

// scoreFile computes a 0–100 confidence score for a single candidate file.
//
// Scoring breakdown (before clamping):
//   - Excluded path (.claude, .mera, etc.)                       : 0  (hard-zero, immediately returned)
//   - Directory contains target module name                      : +15
//   - Filename contains target module name                       : +10
//   - Filename is a known controller/service/DTO                 : up to +20
//   - Filename matches auth keywords (auth task)                 : +20
//   - Filename contains task keywords                            : up to +20
//   - Historical success (time-decayed)                          : up to +20
//   - Historical rejection penalty                               : up to –15
//   - Content keyword matches                                    : up to +20
//   - Auth content signals (auth task only)                      : up to +10
//   - [Intent] Social-auth variant in STANDARD_LOGIN task        : –30
//   - [Intent] Exact DTO name match (LoginRequest → LoginRequest): +15
//   - [Intent] Partial DTO variant (GoogleLoginRequest etc.)     : –20
//   - [Intent] Controller route match [HttpPost("login")]        : +15
//   - Frontend file in backend task                              : –15
//   - Test file when task is not test-focused                    : –10
//
// The score is always clamped to [0, 100].
func scoreFile(c candidateFile, target, task string) FileEvidence {
	rel := filepath.ToSlash(c.relPath)

	// ── Hard exclusion — must be checked before any scoring ─────────────
	// Any file whose full path matches an excluded directory is immediately
	// returned with confidence 0 and will be removed by the final sanitizer.
	if isExcludedPath(rel) {
		return FileEvidence{
			Path:       c.absPath,
			RelPath:    rel,
			Confidence: 0,
			Reasons:    []string{"excluded path — never shown to Aider"},
		}
	}

	score := 0
	var reasons []string

	targetLower := strings.ToLower(target)
	nameLower := strings.ToLower(filepath.Base(c.relPath))
	dirLower := strings.ToLower(filepath.ToSlash(filepath.Dir(c.relPath)))
	taskLower := strings.ToLower(task)
	taskWords := significantWords(task)

	// ── Directory relevance ──────────────────────────────────────────────
	if strings.Contains(dirLower, targetLower) {
		score += 15
		reasons = append(reasons, fmt.Sprintf("lives in target module directory (%s)", target))
	}

	// ── Filename: target module keyword ─────────────────────────────────
	if strings.Contains(nameLower, targetLower) {
		score += 10
		reasons = append(reasons, fmt.Sprintf("filename contains target keyword '%s'", target))
	}

	// ── Filename: known file types (controller/service/repository/dto/request) ──
	fileTypePts := 0
	var fileTypeHints []string
	knownTypes := []struct {
		kw  string
		pts int
	}{
		{"controller", 15}, {"service", 10}, {"repository", 10},
		{"request", 8}, {"response", 6}, {"dto", 8}, {"handler", 8},
		{"manager", 6}, {"provider", 6}, {"store", 5},
	}
	for _, kt := range knownTypes {
		if strings.Contains(nameLower, kt.kw) {
			fileTypePts += kt.pts
			fileTypeHints = append(fileTypeHints, kt.kw)
		}
	}
	if fileTypePts > 20 {
		fileTypePts = 20
	}
	if fileTypePts > 0 {
		score += fileTypePts
		reasons = append(reasons, fmt.Sprintf("file type: %s", strings.Join(fileTypeHints, "/")))
	}

	// ── Auth task: strong filename boost ────────────────────────────────
	isAuthTask := false
	for _, kw := range authTaskKeywords {
		if strings.Contains(taskLower, kw) {
			isAuthTask = true
			break
		}
	}
	if isAuthTask {
		for _, kw := range authFileKeywords {
			if strings.Contains(nameLower, kw) {
				score += 20
				reasons = append(reasons, "auth-task filename match: "+kw)
				break
			}
		}
	}

	// ── Filename: task keywords (cap at 20) ──────────────────────────────
	fileNamePts := 0
	var fileNameHits []string
	for _, w := range taskWords {
		if strings.Contains(nameLower, w) && fileNamePts < 20 {
			fileNamePts += 8
			fileNameHits = append(fileNameHits, w)
		}
	}
	if fileNamePts > 20 {
		fileNamePts = 20
	}
	if fileNamePts > 0 {
		score += fileNamePts
		reasons = append(reasons, fmt.Sprintf("filename matches task keywords: %s", strings.Join(fileNameHits, ", ")))
	}

	// ── PHASE 10.14: Target module exact boost ────────────────────────────
	// Massive boost when file is directly in target module directory
	// Uses simple top-level folder matching (generic, works for any project structure)
	fileModule := topLevelModuleName(rel)
	targetLowerModule := strings.ToLower(target)

	if fileModule != "" && fileModule == targetLowerModule {
		score += targetModuleBoost
		reasons = append(reasons, fmt.Sprintf("file in target module directory (%s) (+%d)", target, targetModuleBoost))
	}

	// ── PHASE 10.14: Multi-symbol matching ────────────────────────────────
	// Extract symbols from task (e.g., "Fix Calculator Add" → ["Calculator", "Add"])
	// Boost files containing exact class/method definitions or matching symbol names
	symbols := extractSymbolsFromTask(task)
	if len(symbols) > 0 {
		// Read file content for symbol analysis
		var contentForSymbolMatch string
		if content, err := readFileContent(c.absPath); err == nil {
			contentForSymbolMatch = content
		}

		// Check for multi-symbol match: how many task symbols appear in filename/content?
		if contentForSymbolMatch != "" {
			symbolScore := scoreMultiSymbolMatch(contentForSymbolMatch, symbols)
			if symbolScore > 0 {
				score += symbolScore
				symbolsFound := findAllSymbolsInContent(contentForSymbolMatch, symbols)
				reasons = append(reasons, fmt.Sprintf("multi-symbol match: %d/%d symbols found (+%d)",
					symbolsFound, len(symbols), symbolScore))
			}

			// Check for exact class/method definitions in file content
			exactMatches := extractExactMatches(contentForSymbolMatch, symbols)
			if len(exactMatches) > 0 {
				score += exactClassDefinitionBoost
				reasons = append(reasons, fmt.Sprintf("exact class/method definition: %s (+%d)",
					strings.Join(exactMatches, ", "), exactClassDefinitionBoost))
			}
		}

		// Check for exact symbol in filename (quick win)
		for _, sym := range symbols {
			if strings.Contains(nameLower, strings.ToLower(sym)) {
				score += exactSymbolMatchBoost
				reasons = append(reasons, fmt.Sprintf("exact symbol in filename: %s (+%d)", sym, exactSymbolMatchBoost))
				break // Avoid double-counting
			}
		}
	}

	// ── PHASE 10.14: Unrelated module penalty ────────────────────────────
	// Heavily penalize files in other modules when target is specified
	if fileModule != "" && fileModule != targetLowerModule {
		// Common unrelated modules that should be penalized
		unrelatedModules := []string{"admin", "finance", "audit", "logging", "infrastructure", "shared"}
		for _, other := range unrelatedModules {
			if fileModule == strings.ToLower(other) {
				score += unrelatedModulePenalty
				reasons = append(reasons, fmt.Sprintf("file in unrelated module (%s) — not target (%s) (%d)",
					other, target, unrelatedModulePenalty))
				break
			}
		}
	}

	// ── Historical weighting (time-decayed) ──────────────────────────────
	profile := loadProfile()

	var decayedSuccess, decayedReject float64
	for _, o := range profile.Outcomes {
		w := outcomeWeight(o.Timestamp)
		for _, f := range o.FilesChanged {
			frel, _ := filepath.Rel(root(), f)
			if filepath.ToSlash(frel) == rel {
				if o.Verdict == "APPROVE" && o.ValidationPassed {
					decayedSuccess += w
				} else if o.Verdict == "REJECT" {
					decayedReject += w
				}
			}
		}
	}
	if decayedSuccess > 0 {
		pts := int(decayedSuccess * 4)
		if pts > 20 {
			pts = 20
		}
		score += pts
		reasons = append(reasons, fmt.Sprintf("successful in %.1f weighted previous session(s)", decayedSuccess))
	}
	if decayedReject > 0 {
		penalty := int(decayedReject * 5)
		if penalty > 15 {
			penalty = 15
		}
		score -= penalty
		reasons = append(reasons, fmt.Sprintf("rejected in %.1f weighted session(s) (-%d pts)", decayedReject, penalty))
	}

	// ── Content keyword match (cap at 20) ────────────────────────────────
	content := sampleFile(c.absPath, 60) // read more lines for better signal
	contentLower := strings.ToLower(content)
	contentPts := 0
	var contentHits []string
	for _, w := range taskWords {
		if strings.Contains(contentLower, w) && contentPts < 20 {
			contentPts += 4
			contentHits = append(contentHits, w)
		}
	}
	if contentPts > 20 {
		contentPts = 20
	}
	if contentPts > 0 {
		score += contentPts
		reasons = append(reasons, fmt.Sprintf("content contains: %s", strings.Join(contentHits, ", ")))
	}

	// ── Auth content signals (auth task, cap at 10) ──────────────────────
	if isAuthTask {
		authContentKws := []string{
			"authcontroller", "loginrequest", "[httppost", "frombody",
			"jwtbearer", "addauthentication", "claimsprincipal",
			"signinasync", "checkpassword", "generatetoken",
		}
		authContentPts := 0
		var authContentHits []string
		for _, kw := range authContentKws {
			if strings.Contains(contentLower, kw) && authContentPts < 10 {
				authContentPts += 5
				authContentHits = append(authContentHits, kw)
			}
		}
		if authContentPts > 0 {
			score += authContentPts
			reasons = append(reasons, fmt.Sprintf("auth content signals: %s", strings.Join(authContentHits, ", ")))
		}
	}

	// ── Intent-precision scoring ─────────────────────────────────────────
	// Classify the task so we can penalise wrong auth sub-variants and boost
	// exact matches. This runs after all keyword scoring so penalties are net.
	intent := classifyTaskIntent(task)

	if intent == IntentStandardLogin {
		// Check whether this file is a social/external auth DTO variant.
		isSocialVariant := false
		for _, v := range socialFileVariants {
			if strings.Contains(nameLower, v) {
				isSocialVariant = true
				break
			}
		}
		if isSocialVariant {
			score -= 30
			reasons = append(reasons, "social-auth variant — not relevant to standard login (-30)")
		}

		// DTO exact-match preference: when the task names a specific DTO,
		// boost the precise file and penalise all partial-name variants.
		if strings.Contains(taskLower, "loginrequest") {
			ext := filepath.Ext(nameLower)
			baseName := strings.TrimSuffix(nameLower, ext)
			if baseName == "loginrequest" {
				score += 15
				reasons = append(reasons, "exact DTO name match: LoginRequest (+15)")
			} else if !isSocialVariant &&
				(strings.HasSuffix(baseName, "loginrequest") || strings.HasPrefix(baseName, "loginrequest")) {
				// Non-social variant but still not an exact DTO match (e.g. LoginRequestExtended).
				score -= 20
				reasons = append(reasons, "partial DTO variant — task targets plain LoginRequest (-20)")
			}
		}

		// Controller-route awareness: boost the file that implements the login endpoint.
		// Checks for [HttpPost("login")] annotation (case-insensitive via contentLower).
		if strings.Contains(taskLower, "post") || strings.Contains(taskLower, "authcontroller") {
			if strings.Contains(contentLower, `[httppost("login")]`) ||
				(strings.Contains(contentLower, "[httppost") && strings.Contains(contentLower, `"login"`)) {
				score += 15
				reasons = append(reasons, `controller route match: [HttpPost("login")] (+15)`)
			}
		}
	}

	// ── Domain mismatch penalty ───────────────────────────────────────────
	fileClass := classifyFile(c.relPath)
	taskDomain := inferTaskDomain(task)

	if fileClass == "frontend" && taskDomain == "backend" {
		score -= 15
		reasons = append(reasons, "frontend file — backend-focused task (-15)")
	}
	if fileClass == "test" {
		if !strings.Contains(taskLower, "test") && !strings.Contains(taskLower, "spec") {
			score -= 10
			reasons = append(reasons, "test file — task is not test-focused (-10)")
		}
	}

	// ── Clamp ────────────────────────────────────────────────────────────
	score = clamp(score, 0, 100)

	return FileEvidence{
		Path:       c.absPath,
		RelPath:    rel,
		Confidence: score,
		Reasons:    reasons,
		Snippet:    findEvidenceSnippet(content, taskWords),
	}
}

// findEvidenceSnippet returns the first line of content that contains any keyword.
func findEvidenceSnippet(content string, keywords []string) string {
	for _, line := range strings.Split(content, "\n") {
		ll := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(ll, kw) {
				trimmed := strings.TrimSpace(line)
				if len(trimmed) > 120 {
					trimmed = trimmed[:120] + "…"
				}
				return trimmed
			}
		}
	}
	return ""
}

// outcomeWeight returns a time-decay multiplier: 1.0 (< 7d) → 0.7 (< 30d) → 0.4 (< 90d) → 0.2 (older).
func outcomeWeight(timestamp string) float64 {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return 0.5
	}
	days := time.Since(t).Hours() / 24
	switch {
	case days < 7:
		return 1.0
	case days < 30:
		return 0.7
	case days < 90:
		return 0.4
	default:
		return 0.2
	}
}

// mergeWithOllamaSelection boosts files that Ollama endorsed, then caps at maxFilesForAider.
// Ollama endorsement adds +15 to confidence, with "selected by AI analysis" as reason.
func mergeWithOllamaSelection(ollamaRel []string, scored []FileEvidence) []FileEvidence {
	ollamaSet := map[string]bool{}
	for _, r := range ollamaRel {
		ollamaSet[filepath.ToSlash(filepath.Clean(r))] = true
	}
	for i := range scored {
		if ollamaSet[filepath.ToSlash(scored[i].RelPath)] {
			scored[i].Confidence = clamp(scored[i].Confidence+15, 0, 100)
			scored[i].Reasons = append(scored[i].Reasons, "selected by AI file analysis")
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Confidence > scored[j].Confidence
	})
	lim := effectiveFileLimit()
	if len(scored) > lim {
		scored = scored[:lim]
	}
	return scored
}

// enforceFileLimit warns and trims if too many files would be passed to Aider.
// The limit comes from the active performance profile.
// Returns the (possibly trimmed) evidence list and whether truncation occurred.
func enforceFileLimit(evidence []FileEvidence) ([]FileEvidence, bool) {
	lim := effectiveFileLimit()
	if len(evidence) <= lim {
		return evidence, false
	}
	sort.Slice(evidence, func(i, j int) bool {
		return evidence[i].Confidence > evidence[j].Confidence
	})
	fmt.Printf("[WARN] File Scout returned %d files — capping at %d per %s profile (highest confidence kept).\n",
		len(evidence), lim, activeProfile())
	return evidence[:lim], true
}

// printEvidenceReport prints a human-readable evidence table for a set of files.
func printEvidenceReport(evidence []FileEvidence) {
	if len(evidence) == 0 {
		fmt.Println("[MERA] No files selected.")
		return
	}
	fmt.Println("\n[MERA] File Scout Evidence Report:")
	fmt.Println("[MERA] ========================================")
	for i, ev := range evidence {
		bar := confidenceBar(ev.Confidence)
		fileClass := classifyFile(ev.RelPath)
		fmt.Printf("\n  %d. %s\n     Confidence: %d%% %s  [%s]\n", i+1, ev.RelPath, ev.Confidence, bar, fileClass)
		for _, r := range ev.Reasons {
			fmt.Printf("     • %s\n", r)
		}
		if ev.Snippet != "" {
			fmt.Printf("     -> %s\n", ev.Snippet)
		}
	}
	fmt.Println("\n[MERA] ========================================")
}

// approveFiles shows the evidence report and asks the user to approve, edit, or cancel.
// Returns the final list of absolute file paths approved for Aider.
func approveFiles(evidence []FileEvidence) ([]string, error) {
	printEvidenceReport(evidence)

	fmt.Println("[MERA] Approve file selection before launching Aider.")
	answer := strings.ToUpper(strings.TrimSpace(promptLine("  [YES] proceed / [EDIT] modify list / [CANCEL] abort")))

	switch answer {
	case "YES", "Y":
		fmt.Printf("[OK]  %d file(s) approved.\n", len(evidence))
		return evidencePaths(evidence), nil
	case "EDIT":
		return editFileList(evidence)
	default:
		return nil, fmt.Errorf("file selection cancelled by user")
	}
}

// editFileList lets the user remove files from the selected list by number.
func editFileList(evidence []FileEvidence) ([]string, error) {
	fmt.Println("\n[MERA] Current file list:")
	for i, ev := range evidence {
		fmt.Printf("  %d. [%d%%] %s\n", i+1, ev.Confidence, ev.RelPath)
	}
	raw := promptLine("Enter number(s) to REMOVE (e.g. 2,3), or Enter to keep all")
	raw = strings.TrimSpace(raw)

	toRemove := map[int]bool{}
	if raw != "" {
		for _, s := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err == nil && n >= 1 && n <= len(evidence) {
				toRemove[n-1] = true
			}
		}
	}

	var kept []FileEvidence
	for i, ev := range evidence {
		if !toRemove[i] {
			kept = append(kept, ev)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("all files removed — selection cancelled")
	}
	fmt.Printf("[OK]  %d file(s) approved after edit.\n", len(kept))
	return evidencePaths(kept), nil
}

// confidenceBar returns a small ASCII bar for visual display.
func confidenceBar(pct int) string {
	filled := pct / 10
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
	label := "LOW"
	if pct >= midConfidenceThreshold {
		label = "MED"
	}
	if pct >= 75 {
		label = "HIGH"
	}
	return fmt.Sprintf("[%s] %s", bar, label)
}

func evidencePaths(ev []FileEvidence) []string {
	out := make([]string, len(ev))
	for i, e := range ev {
		out[i] = e.Path
	}
	return out
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
