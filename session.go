package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Data model ────────────────────────────────────────────────────────────────

// PhaseEvent records a single workflow phase within a session.
type PhaseEvent struct {
	Phase      string `json:"phase"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Model      string `json:"model,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// ExecutionSession is created once per workflow run and persisted under .mera/sessions/.
type ExecutionSession struct {
	ID          string       `json:"id"`
	Version     string       `json:"version"`
	StartedAt   string       `json:"startedAt"`
	EndedAt     string       `json:"endedAt,omitempty"`
	Command     string       `json:"command"`
	Target      string       `json:"target"`
	Mode        string       `json:"mode"`
	Task        string       `json:"task"`
	Timeline    []PhaseEvent `json:"timeline"`
	AbortReason string       `json:"abortReason,omitempty"`
	PartialDiff string       `json:"partialDiffPath,omitempty"`
}

// ── Package-level session state ───────────────────────────────────────────────

var (
	activeSess   *ExecutionSession
	activeSessMu sync.Mutex
	phaseStartTime time.Time
	currentPhase   string

	hbMu   sync.Mutex
	hbStop chan struct{}
)

// ── Session lifecycle ─────────────────────────────────────────────────────────

// beginSession acquires the session lock, creates a new ExecutionSession,
// registers it as active, and prints the session ID.
// Fatal if the lock cannot be acquired (another session is active).
func beginSession(command, target, mode, task string) *ExecutionSession {
	id := newSessionID()

	// Acquire exclusive session lock before creating any state.
	if err := acquireSessionLock(id); err != nil {
		fmt.Fprintln(os.Stderr, "[FAIL]", err)
		os.Exit(1)
	}

	s := &ExecutionSession{
		ID:        id,
		Version:   BuildVersion,
		StartedAt: time.Now().Format(time.RFC3339),
		Command:   command,
		Target:    target,
		Mode:      mode,
		Task:      task,
	}
	_ = os.MkdirAll(sessionDir(id), 0755)

	activeSessMu.Lock()
	activeSess = s
	activeSessMu.Unlock()

	fmt.Printf("[MERA] Session: %s\n", id)
	appendMeraLog("SESSION", "begin id="+id+" command="+command+" target="+target+" mode="+mode)
	return s
}

// closeSession finalises the session, writes all files, prunes old sessions,
// releases the lock, and clears the active session pointer.
// Pass abortReason="" for a normal close. Safe to call multiple times.
func closeSession(abortReason string) {
	activeSessMu.Lock()
	s := activeSess
	stopHeartbeat()
	activeSessMu.Unlock()

	if s == nil {
		return
	}

	s.EndedAt = time.Now().Format(time.RFC3339)
	if abortReason != "" {
		s.AbortReason = abortReason
	}
	writeSessionFiles(s)

	// Prune old sessions before releasing the lock so cleanup is serialised.
	cfg := loadConfig()
	pruneOldSessions(cfg.MaxSessions)

	releaseSessionLock()

	activeSessMu.Lock()
	activeSess = nil
	activeSessMu.Unlock()

	appendMeraLog("SESSION", fmt.Sprintf("end id=%s abort=%q", s.ID, abortReason))
}

// ── Phase tracking ────────────────────────────────────────────────────────────

// sessionBeginPhase records phase start, prints the observability header,
// and starts the heartbeat goroutine.
func sessionBeginPhase(phase string) {
	activeSessMu.Lock()
	currentPhase = phase
	phaseStartTime = time.Now()
	s := activeSess
	activeSessMu.Unlock()

	elapsed := "00:00"
	if s != nil {
		if t, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
			e := time.Since(t).Round(time.Second)
			elapsed = fmt.Sprintf("%02d:%02d", int(e.Minutes()), int(e.Seconds())%60)
		}
	}
	id := ""
	if s != nil {
		id = s.ID
	}
	fmt.Printf("\n[MERA] Phase: %-22s Status: Running   Elapsed: %s\n", phase, elapsed)

	if id != "" {
		appendMeraLog("PHASE", "begin phase="+phase+" session="+id)
	}

	cfg := loadConfig()
	interval := time.Duration(cfg.HeartbeatSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	startHeartbeat(phase, interval)
}

// sessionEndPhase stops the heartbeat and appends the phase event to the timeline.
func sessionEndPhase(phase, status, model string) {
	stopHeartbeat()

	activeSessMu.Lock()
	defer activeSessMu.Unlock()

	if activeSess == nil {
		return
	}

	now := time.Now()
	ev := PhaseEvent{
		Phase:      phase,
		Status:     status,
		StartedAt:  phaseStartTime.Format(time.RFC3339),
		EndedAt:    now.Format(time.RFC3339),
		DurationMs: now.Sub(phaseStartTime).Milliseconds(),
		Model:      model,
	}
	activeSess.Timeline = append(activeSess.Timeline, ev)
	appendMeraLog("PHASE", fmt.Sprintf("end phase=%s status=%s duration=%dms model=%s",
		phase, status, ev.DurationMs, model))
}

// ── Heartbeat ─────────────────────────────────────────────────────────────────

func startHeartbeat(phase string, interval time.Duration) {
	hbMu.Lock()
	defer hbMu.Unlock()
	// Close any existing heartbeat before starting a new one.
	if hbStop != nil {
		select {
		case <-hbStop: // already closed
		default:
			close(hbStop)
		}
	}
	hbStop = make(chan struct{})
	start := time.Now()
	stop := hbStop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Printf("[MERA] Still working... (%s) elapsed %v\n",
					phase, time.Since(start).Round(time.Second))
			}
		}
	}()
}

// stopHeartbeat closes the heartbeat channel. Safe to call while NOT holding
// activeSessMu (it acquires hbMu independently).
func stopHeartbeat() {
	hbMu.Lock()
	defer hbMu.Unlock()
	if hbStop == nil {
		return
	}
	select {
	case <-hbStop: // already closed
	default:
		close(hbStop)
	}
	hbStop = nil
}

// ── Abort handling ────────────────────────────────────────────────────────────

// setupAbortHandler installs a SIGINT (Ctrl+C) handler.
// On interrupt: kills the active child process, captures partial diff,
// writes session files, releases the session lock, and exits with code 2.
func setupAbortHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		fmt.Println("\n[MERA] Interrupted by user.")

		// Kill child process first (Aider, dotnet build, etc.).
		killActiveCmd()

		activeSessMu.Lock()
		s := activeSess
		stopHeartbeat()
		activeSessMu.Unlock()

		if s != nil {
			s.AbortReason = "User interrupted (Ctrl+C)"
			s.EndedAt = time.Now().Format(time.RFC3339)
			capturePartialDiff(s)
			writeSessionFiles(s)
			appendMeraLog("SESSION", "aborted id="+s.ID+" reason=Ctrl+C")
		}

		releaseSessionLock()
		fmt.Println("[MERA] Session saved. Consider: mera -Rollback")
		os.Exit(2)
	}()
}

// ── Session files ─────────────────────────────────────────────────────────────

func sessionDir(id string) string { return filepath.Join(sessionsDir(), id) }

func capturePartialDiff(s *ExecutionSession) {
	if s.PartialDiff != "" {
		return // already captured
	}
	diff := capture("git", "diff", "HEAD")
	if diff == "" {
		diff = capture("git", "diff")
	}
	if diff == "" {
		return
	}
	path := filepath.Join(sessionDir(s.ID), "partial-diff.patch")
	_ = writeNoBOM(path, []byte(diff))
	s.PartialDiff = path
	fmt.Printf("[MERA] Partial diff saved: %s\n", path)
}

func writeSessionFiles(s *ExecutionSession) {
	dir := sessionDir(s.ID)
	_ = os.MkdirAll(dir, 0755)

	// summary.json
	if b, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = writeNoBOM(filepath.Join(dir, "summary.json"), b)
	}

	// timeline.log
	var lines []string
	for _, ev := range s.Timeline {
		dur := ""
		if ev.DurationMs > 0 {
			dur = fmt.Sprintf(" (%dms)", ev.DurationMs)
		}
		mdl := ""
		if ev.Model != "" {
			mdl = " [" + ev.Model + "]"
		}
		lines = append(lines, fmt.Sprintf("[%s] %-22s %s%s%s",
			shortTime(ev.StartedAt), ev.Phase, ev.Status, dur, mdl))
	}
	_ = writeNoBOM(filepath.Join(dir, "timeline.log"),
		[]byte(strings.Join(lines, "\n")+"\n"))

	// diff: partial on abort, full on success
	if s.AbortReason != "" {
		capturePartialDiff(s)
	} else {
		diff := capture("git", "diff", "HEAD")
		if diff == "" {
			diff = capture("git", "diff")
		}
		if diff != "" {
			_ = writeNoBOM(filepath.Join(dir, "diff.patch"), []byte(diff))
		}
	}

	// report.md
	_ = writeNoBOM(filepath.Join(dir, "report.md"), []byte(buildSessionMD(s)))

	fmt.Printf("[MERA] Session files: %s\n", dir)
}

func buildSessionMD(s *ExecutionSession) string {
	var sb strings.Builder
	sb.WriteString("# MERA Session Report\n\n")
	sb.WriteString(fmt.Sprintf("**Session:** `%s`  \n", s.ID))
	sb.WriteString(fmt.Sprintf("**Version:** MERA Go v%s  \n", s.Version))
	sb.WriteString(fmt.Sprintf("**Command:** %s  \n", s.Command))
	sb.WriteString(fmt.Sprintf("**Target:** %s  \n", s.Target))
	sb.WriteString(fmt.Sprintf("**Mode:** %s  \n", s.Mode))
	sb.WriteString(fmt.Sprintf("**Task:** %s  \n", s.Task))
	sb.WriteString(fmt.Sprintf("**Started:** %s  \n", s.StartedAt))
	if s.EndedAt != "" {
		if st, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
			if et, err2 := time.Parse(time.RFC3339, s.EndedAt); err2 == nil {
				sb.WriteString(fmt.Sprintf("**Ended:** %s  (duration: %v)  \n",
					s.EndedAt, et.Sub(st).Round(time.Second)))
			}
		}
	}
	if s.AbortReason != "" {
		sb.WriteString(fmt.Sprintf("**Aborted:** %s  \n", s.AbortReason))
	}
	sb.WriteString("\n## Timeline\n\n")
	for _, ev := range s.Timeline {
		dur := ""
		if ev.DurationMs > 0 {
			dur = fmt.Sprintf(" (%dms)", ev.DurationMs)
		}
		mdl := ""
		if ev.Model != "" {
			mdl = " [" + ev.Model + "]"
		}
		sb.WriteString(fmt.Sprintf("- **%s** — %s%s%s\n", ev.Phase, ev.Status, dur, mdl))
	}
	if s.PartialDiff != "" {
		sb.WriteString(fmt.Sprintf("\n## Partial Diff\n\nSaved to: `%s`\n", s.PartialDiff))
	}
	return sb.String()
}

// shortTime returns HH:MM:SS from an RFC3339 timestamp string.
func shortTime(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("15:04:05")
	}
	return ts
}

// ── Session retention ─────────────────────────────────────────────────────────

// pruneOldSessions deletes the oldest session directories when the total count
// exceeds max. Session IDs embed a timestamp so lexicographic sort = chronological.
// Does not delete the directory of any currently active session.
func pruneOldSessions(max int) {
	if max <= 0 {
		max = 50
	}
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= max {
		return
	}
	sort.Strings(dirs) // lexicographic == chronological for SESSION-YYYYMMDD-... IDs

	// Guard: skip deletion of the currently active session's directory.
	activeSessMu.Lock()
	activeID := ""
	if activeSess != nil {
		activeID = activeSess.ID
	}
	activeSessMu.Unlock()

	toDelete := dirs[:len(dirs)-max]
	deleted := 0
	for _, name := range toDelete {
		if name == activeID {
			continue // never delete the active session
		}
		dir := filepath.Join(sessionsDir(), name)
		if err := os.RemoveAll(dir); err == nil {
			deleted++
			appendMeraLog("SESSION", "pruned old session: "+name)
		}
	}
	if deleted > 0 {
		fmt.Printf("[MERA] Session cleanup: removed %d old session(s), retained %d\n", deleted, max)
	}
}

// ── Session replay ────────────────────────────────────────────────────────────

// replaySession prints a formatted replay of a saved session by ID or unambiguous prefix.
func replaySession(id string) error {
	dir, _, err := resolveSessionDir(id)
	if err != nil {
		return err
	}

	summaryPath := filepath.Join(dir, "summary.json")
	b, readErr := os.ReadFile(summaryPath)
	if readErr != nil {
		return fmt.Errorf("[WARN] summary.json missing for session %q — session may be incomplete", id)
	}
	var s ExecutionSession
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("session summary is corrupt (%s): %w", summaryPath, err)
	}

	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA]  Session Replay\n")
	fmt.Println("[MERA] ========================================")
	fmt.Printf("\n  ID      : %s\n", s.ID)
	fmt.Printf("  Version : MERA Go v%s\n", s.Version)
	fmt.Printf("  Command : %s\n", s.Command)
	fmt.Printf("  Target  : %s\n", s.Target)
	fmt.Printf("  Mode    : %s\n", s.Mode)
	fmt.Printf("  Task    : %s\n", s.Task)
	fmt.Printf("  Started : %s\n", s.StartedAt)

	if s.EndedAt != "" {
		if st, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
			if et, err2 := time.Parse(time.RFC3339, s.EndedAt); err2 == nil {
				fmt.Printf("  Ended   : %s  (duration: %v)\n",
					s.EndedAt, et.Sub(st).Round(time.Second))
			}
		}
	}
	if s.AbortReason != "" {
		fmt.Printf("  ABORTED : %s\n", s.AbortReason)
	}

	fmt.Println("\n  Timeline:")
	if len(s.Timeline) == 0 {
		fmt.Println("    (no phases recorded)")
	}
	for i, ev := range s.Timeline {
		icon := "[OK]  "
		if ev.Status == "failed" || ev.Status == "degraded" || ev.Status == "blocked" {
			icon = "[WARN]"
		}
		dur := ""
		if ev.DurationMs > 0 {
			dur = fmt.Sprintf(" (%dms)", ev.DurationMs)
		}
		mdl := ""
		if ev.Model != "" {
			mdl = " [" + ev.Model + "]"
		}
		fmt.Printf("  %2d. %s %-22s %s%s%s\n", i+1, icon, ev.Phase, ev.Status, dur, mdl)
	}

	// Artifact inventory — warn on missing expected files.
	type artifact struct {
		name     string
		path     string
		required bool
	}
	artifacts := []artifact{
		{"summary.json", filepath.Join(dir, "summary.json"), true},
		{"timeline.log", filepath.Join(dir, "timeline.log"), true},
		{"report.md", filepath.Join(dir, "report.md"), true},
		{"diff.patch", filepath.Join(dir, "diff.patch"), false},
		{"partial-diff.patch", filepath.Join(dir, "partial-diff.patch"), false},
	}

	fmt.Println("\n  Session artifacts:")
	for _, a := range artifacts {
		if exists(a.path) {
			fmt.Printf("    [OK]   %s\n", a.path)
		} else if a.required {
			fmt.Printf("    [WARN] %s — missing\n", a.name)
		}
	}

	// Show tail of partial diff if present.
	partialPath := filepath.Join(dir, "partial-diff.patch")
	if exists(partialPath) {
		fmt.Println("\n  Partial diff (last 40 lines):")
		lines := tailFile(partialPath, 40)
		for _, l := range lines {
			fmt.Println("    " + l)
		}
	}

	fmt.Println("\n[MERA] ========================================")
	return nil
}

// listSessions prints all stored session IDs with retention policy info.
func listSessions() {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		fmt.Println("[MERA] No sessions directory found. Run 'mera -Init' first.")
		return
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}

	cfg := loadConfig()
	maxSess := cfg.MaxSessions
	if maxSess <= 0 {
		maxSess = 50
	}

	// Check active lock
	lock := readSessionLock()

	fmt.Println("\n[MERA] ========================================")
	fmt.Printf("[MERA]  Sessions  (%d stored, retain last %d)\n", len(dirs), maxSess)
	fmt.Println("[MERA] ========================================")

	if lock != nil {
		t, err := time.Parse(time.RFC3339, lock.StartedAt)
		age := ""
		if err == nil {
			age = fmt.Sprintf("  age: %v", time.Since(t).Round(time.Second))
		}
		fmt.Printf("\n  [ACTIVE] %s%s\n", lock.SessionID, age)
	}

	if len(dirs) == 0 {
		fmt.Println("\n  (no sessions stored)")
	} else {
		fmt.Println()
		sort.Strings(dirs)
		for i := len(dirs) - 1; i >= 0; i-- {
			marker := "  "
			if lock != nil && dirs[i] == lock.SessionID {
				marker = "> "
			}
			fmt.Printf("  %s%s\n", marker, dirs[i])
		}
	}

	fmt.Println()
	fmt.Println("  Replay : mera -Replay <session-id>")
	fmt.Printf("  Policy : oldest sessions auto-pruned after %d are stored\n", maxSess)
	fmt.Println("[MERA] ========================================")
}

// resolveSessionDir finds a session directory by exact ID or unambiguous prefix.
// Returns an error if the prefix matches multiple sessions (ambiguous).
func resolveSessionDir(id string) (dir, resolvedID string, err error) {
	exact := filepath.Join(sessionsDir(), id)
	if exists(exact) {
		return exact, id, nil
	}

	entries, readErr := os.ReadDir(sessionsDir())
	if readErr != nil {
		return "", "", fmt.Errorf("cannot read sessions directory: %w", readErr)
	}

	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), id) {
			matches = append(matches, e.Name())
		}
	}

	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("session %q not found — run 'mera -Sessions' to list sessions", id)
	case 1:
		d := filepath.Join(sessionsDir(), matches[0])
		return d, matches[0], nil
	default:
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("ambiguous prefix %q matches %d sessions:\n", id, len(matches)))
		for _, m := range matches {
			sb.WriteString("  " + m + "\n")
		}
		sb.WriteString("Use a longer prefix or the full session ID.")
		return "", "", fmt.Errorf("%s", sb.String())
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func newSessionID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("SESSION-%s-%s",
		time.Now().Format("20060102-150405"),
		strings.ToUpper(hex.EncodeToString(b)))
}
