package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const staleLockAge = 2 * time.Hour

// SessionLock is written to .mera/session.lock when a workflow session is active.
type SessionLock struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

func lockPath() string { return filepath.Join(meraDir(), "session.lock") }

// acquireSessionLock creates the lock file for sessionID.
// Returns an error if another non-stale session holds the lock.
// Automatically clears stale locks (older than staleLockAge).
func acquireSessionLock(sessionID string) error {
	path := lockPath()

	if b, err := os.ReadFile(path); err == nil {
		var existing SessionLock
		if json.Unmarshal(b, &existing) == nil {
			t, tErr := time.Parse(time.RFC3339, existing.StartedAt)
			if tErr == nil && time.Since(t) < staleLockAge {
				return fmt.Errorf(
					"another MERA session is active:\n"+
						"  Session : %s\n"+
						"  Started : %s\n"+
						"  Age     : %v\n"+
						"  Lock    : %s\n"+
						"Wait for it to finish, or delete the lock file if that session crashed.",
					existing.SessionID, existing.StartedAt,
					time.Since(t).Round(time.Second), path)
			}
			// Stale — log and remove.
			age := time.Since(t).Round(time.Minute)
			fmt.Printf("[WARN] Clearing stale session lock (id: %s, age: %v)\n",
				existing.SessionID, age)
			appendMeraLog("WARN", fmt.Sprintf("cleared stale lock id=%s age=%v", existing.SessionID, age))
		}
		// Malformed or stale — remove before acquiring.
		_ = os.Remove(path)
	}

	lock := SessionLock{
		SessionID: sessionID,
		PID:       os.Getpid(),
		StartedAt: time.Now().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(lock, "", "  ")
	return writeNoBOM(path, b)
}

// releaseSessionLock removes the session lock file.
// Safe to call multiple times; errors are silently ignored.
func releaseSessionLock() {
	_ = os.Remove(lockPath())
}

// readSessionLock reads the current lock without acquiring or releasing it.
// Returns nil if no lock exists or lock is unreadable.
func readSessionLock() *SessionLock {
	b, err := os.ReadFile(lockPath())
	if err != nil {
		return nil
	}
	var l SessionLock
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	return &l
}
