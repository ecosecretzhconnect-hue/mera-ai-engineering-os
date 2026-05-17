package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func blastRadius(fs []string) int {
	r := 1
	for _, f := range fs {
		l := strings.ToLower(f)
		switch {
		case strings.Contains(l, "appsettings") || strings.Contains(l, "program.cs") ||
			strings.Contains(l, ".csproj") || strings.Contains(l, "migration") ||
			strings.Contains(l, "auth") || strings.Contains(l, "security") ||
			strings.Contains(l, "gateway"):
			if r < 4 {
				r = 4
			}
		case strings.Contains(l, "controller") || strings.Contains(l, "service") ||
			strings.Contains(l, "repository") || strings.Contains(l, "routes") ||
			strings.Contains(l, "api") || strings.Contains(l, "page.tsx"):
			if r < 3 {
				r = 3
			}
		default:
			if r < 2 {
				r = 2
			}
		}
	}
	return r
}

func boundaryCheck(target string, fs []string, print bool) bool {
	cfg := loadConfig()
	m, ok := cfg.Modules[target]
	if !ok {
		if print {
			fmt.Println("[OK] No boundary rules for target:", target)
		}
		return true
	}
	raw, _ := m["forbidden"].([]interface{})
	var violations []string
	for _, x := range raw {
		pat := fmt.Sprint(x)
		prefix := strings.TrimSuffix(pat, "/**")
		for _, f := range fs {
			if strings.HasPrefix(filepath.ToSlash(f), prefix) {
				violations = append(violations, f)
			}
		}
	}
	if len(violations) > 0 {
		if print {
			fmt.Println("[FAIL] Boundary violations detected:")
			for _, v := range violations {
				fmt.Println("  -", v)
			}
		}
		return false
	}
	if print {
		fmt.Println("[OK] Boundary check passed.")
	}
	return true
}

// secretScan checks changed files for hardcoded secrets using a conservative regex.
func secretScan(fs []string) bool {
	re := regexp.MustCompile(
		`(?i)(password\s*=\s*["'][^"']{4,}|jwt[_:]key\s*=|connectionstrings\s*=|api[_-]?key\s*=\s*["'][^"']{4,}|-----BEGIN (RSA |EC )?PRIVATE KEY|secret\s*=\s*["'][^"']{4,})`,
	)
	for _, f := range fs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if re.Match(b) {
			fmt.Println("[FAIL] Potential hardcoded secret detected in:", f)
			return false
		}
	}
	return true
}

func runValidation(target string, rep *WorkflowReport) bool {
	p := detectProject()
	fs := changedFiles()

	boundary := boundaryCheck(target, fs, true)
	rep.Validation["boundary"] = boundary

	sec := secretScan(fs)
	rep.Validation["secretScan"] = sec

	build := runCommandStep("Build", p.Build)
	rep.Validation["build"] = build

	test := false
	if build {
		test = runCommandStep("Tests", p.Test)
	}
	rep.Validation["tests"] = test

	front := true
	if touched(fs, "HConnect.Web") && p.FrontendBuild != "" {
		front = runCommandStep("Frontend Build", p.FrontendBuild)
	}
	rep.Validation["frontendBuild"] = front

	return boundary && sec && build && test && front
}

func validateOnly() error {
	p := detectProject()
	ok := runCommandStep("Build", p.Build)
	ok = runCommandStep("Tests", p.Test) && ok
	if p.FrontendBuild != "" {
		ok = runCommandStep("Frontend Build", p.FrontendBuild) && ok
	}
	if !ok {
		return errors.New("validation failed — fix errors before committing")
	}
	fmt.Println("[OK] All validation steps passed.")
	return nil
}

func runCommandStep(title, command string) bool {
	if strings.TrimSpace(command) == "" {
		fmt.Printf("[MERA] %s skipped (not configured)\n", title)
		return true
	}
	fmt.Printf("[MERA] Running %s: %s\n", title, command)
	ctx := context.Background()
	var err error
	if runtime.GOOS == "windows" {
		err = runInteractive(ctx, "cmd", []string{"/c", command}, 0, 0, 0)
	} else {
		err = runInteractive(ctx, "sh", []string{"-lc", command}, 0, 0, 0)
	}
	if err != nil {
		fmt.Printf("[FAIL] %s failed: %v\n", title, err)
		return false
	}
	fmt.Printf("[OK] %s passed.\n", title)
	return true
}

func touched(fs []string, prefix string) bool {
	for _, f := range fs {
		if strings.HasPrefix(filepath.ToSlash(f), prefix) {
			return true
		}
	}
	return false
}
