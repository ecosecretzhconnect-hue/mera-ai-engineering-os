package main

import (
	"fmt"
	"os"
	"strings"
)

const Version = "MERA Go v10.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd := strings.ToLower(os.Args[1])
	switch cmd {
	case "-doctor", "doctor":
		must(initProject())
		runDoctor()
	case "-init", "init":
		must(initProject())
	case "-scan", "scan":
		must(initProject())
		must(auth())
		printProject(detectProject())
	case "-fingerprint", "fingerprint":
		must(initProject())
		printJSON(loadConfig().Stack)
	case "-snapshot", "snapshot":
		must(initProject())
		must(auth())
		must(ensureGitRepo(false))
		must(createSnapshot())
	case "-rollback", "rollback":
		must(initProject())
		must(auth())
		must(rollback())
	case "-blast", "-blastradius", "blast", "blastradius":
		must(initProject())
		fmt.Println("Blast Radius:", blastRadius(changedFiles()))
	case "-boundary", "-boundarycheck", "boundary", "boundarycheck":
		must(initProject())
		target := arg(2, "")
		if !boundaryCheck(target, changedFiles(), true) {
			os.Exit(2)
		}
	case "-plan", "plan":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := strings.Join(safeSlice(os.Args, 3), " ")
		must(writePlan(target, task))
	case "-orchestrate", "orchestrate":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(orchestrate(target, task, "normal", false))
	case "-fast", "fast":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(orchestrate(target, task, "fast", true))
	case "-deep", "deep":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(orchestrate(target, task, "deep", true))
	case "-analyze", "analyze":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(orchestrate(target, task, "analyze", false))
	case "-code", "code":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(orchestrate(target, task, "code", true))
	case "-validate", "validate":
		must(initProject())
		must(auth())
		must(validateOnly())
	case "-explainselection", "explainselection", "-explain", "explain":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(explainSelection(target, task))
	case "-explaindiff", "explaindiff":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(explainDiff(target, task))
	case "-models", "models":
		must(initProject())
		printModels()
	case "-setmodel", "setmodel":
		must(initProject())
		role := arg(2, "")
		model := arg(3, "")
		if role == "" || model == "" {
			fmt.Println("Usage: mera -SetModel <role> <model>")
			fmt.Println("Roles: planner, architect, filescout, code, diffreview, security, sprintadvisor")
			os.Exit(1)
		}
		must(setModelForRole(role, model))
	case "-setprofile", "setprofile":
		must(initProject())
		profile := arg(2, "")
		if profile == "" {
			fmt.Println("Usage: mera -SetProfile FAST|NORMAL|DEEP|STRICT")
			os.Exit(1)
		}
		must(setActiveProfile(profile))
	case "-profilemode", "profilemode":
		must(initProject())
		printProfileMode()
	case "-profile", "profile":
		must(initProject())
		must(auth())
		printProfile()
	case "-dryrun", "dryrun":
		must(initProject())
		must(auth())
		target := arg(2, "")
		task := joinOrPrompt(3, "Describe task")
		must(dryRun(target, task))
	case "-mission", "mission":
		must(initProject())
		must(auth())
		must(missionWizard())
	case "-diag", "diag":
		must(initProject())
		runDiag()
	case "-health", "health":
		must(initProject())
		runHealth()
	case "-logs", "logs":
		must(initProject())
		showLogs()
	case "-version", "version":
		printVersion()
	case "-smoketest", "smoketest":
		must(initProject())
		must(smokeTestAuth())
		must(runSmokeTest())
	case "-replay", "replay":
		must(initProject())
		id := arg(2, "")
		if id == "" {
			listSessions()
		} else {
			must(replaySession(id))
		}
	case "-sessions", "sessions":
		must(initProject())
		listSessions()
	case "-genmanifest", "genmanifest":
		outDir := arg(2, "outputs")
		must(generateManifest(outDir))
	default:
		must(initProject())
		must(auth())
		mode := os.Args[1]
		target := arg(2, "")
		task := promptLine("Paste/describe task")
		must(runAider(mode, target, task, "normal", true, nil, nil))
	}
}

func usage() {
	fmt.Println(Version)
	fmt.Println("Commands: -Doctor -Init -Scan -Fingerprint -Profile -ExplainSelection -ExplainDiff -DryRun -Plan -Analyze -Code -Fast -Deep -Orchestrate -Validate -Snapshot -Rollback -BoundaryCheck -BlastRadius -Mission -Models -SetModel -SetProfile -ProfileMode -Diag -Health -Logs -Version -SmokeTest -Replay -Sessions")
}

func must(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, "[FAIL]", e)
		os.Exit(1)
	}
}

func arg(i int, d string) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return d
}

func safeSlice(s []string, from int) []string {
	if from >= len(s) {
		return nil
	}
	return s[from:]
}

func joinOrPrompt(i int, label string) string {
	if len(os.Args) <= i {
		return promptLine(label)
	}
	s := strings.TrimSpace(strings.Join(os.Args[i:], " "))
	if s == "" {
		return promptLine(label)
	}
	return s
}

// deepPlanRequested returns true when the user passes --deep-plan on the command line.
// This opts out of the BUGFIX_NARROW deterministic planner and forces a full LLM plan.
func deepPlanRequested() bool {
	for _, a := range os.Args {
		if strings.EqualFold(a, "--deep-plan") {
			return true
		}
	}
	return false
}
