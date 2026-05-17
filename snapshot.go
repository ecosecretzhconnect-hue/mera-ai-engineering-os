package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Snapshot struct {
	SHA       string `json:"sha"`
	Branch    string `json:"branch"`
	CreatedAt string `json:"createdAt"`
}

func ensureGitRepo(allowMain bool) error {
	if e := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); e != nil {
		return errors.New("not inside a git repository")
	}
	branch := strings.TrimSpace(capture("git", "rev-parse", "--abbrev-ref", "HEAD"))
	if (branch == "main" || branch == "master") && !allowMain {
		return fmt.Errorf("you are on %s — create a feature branch first", branch)
	}
	return nil
}

func createSnapshot() error {
	s := Snapshot{
		SHA:       strings.TrimSpace(capture("git", "rev-parse", "HEAD")),
		Branch:    strings.TrimSpace(capture("git", "rev-parse", "--abbrev-ref", "HEAD")),
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	path := filepath.Join(snapshotsDir(), time.Now().Format("20060102-150405")+"-snapshot.json")
	if e := writeNoBOM(path, b); e != nil {
		return e
	}
	fmt.Println("[OK] Snapshot created:", path)
	return nil
}

func rollback() error {
	files, _ := filepath.Glob(filepath.Join(snapshotsDir(), "*-snapshot.json"))
	if len(files) == 0 {
		return errors.New("no snapshots found — run -Snapshot first")
	}
	sort.Strings(files)

	var s Snapshot
	b, _ := os.ReadFile(files[len(files)-1])
	_ = json.Unmarshal(b, &s)

	fmt.Printf("[WARN] Rollback target: SHA=%s  Branch=%s  Created=%s\n", s.SHA, s.Branch, s.CreatedAt)
	fmt.Println("[WARN] This will discard ALL uncommitted changes and untracked files.")
	if promptLine("Type ROLLBACK to confirm") != "ROLLBACK" {
		fmt.Println("[WARN] Rollback cancelled.")
		return nil
	}
	_ = runInteractive(context.Background(), "git", []string{"reset", "--hard", s.SHA}, 0, 0, 0)
	_ = runInteractive(context.Background(), "git", []string{"clean", "-fdx"}, 0, 0, 0)
	fmt.Println("[OK] Rollback complete.")
	return nil
}

// changedFiles returns relative paths of files modified relative to HEAD.
// Uses "git diff --name-only HEAD" so only tracked file changes are counted
// (untracked files shown as "??" in git status are excluded).
// Falls back to --cached (staged only) if HEAD doesn't exist yet (brand-new repo).
func changedFiles() []string {
	out := strings.TrimSpace(capture("git", "diff", "--name-only", "HEAD"))
	if out == "" {
		// Brand-new repo or clean working tree — try staged changes only.
		out = strings.TrimSpace(capture("git", "diff", "--name-only", "--cached"))
	}
	var fs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			fs = append(fs, l)
		}
	}
	return fs
}
