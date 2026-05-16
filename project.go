package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Project struct {
	Type          string
	Main          string
	Build         string
	Test          string
	Audit         string
	FrontendBuild string
}

func detectProject() Project {
	p := Project{Type: "Unknown"}
	r := root()

	slnx := firstExt(r, ".slnx")
	sln := firstExt(r, ".sln")

	switch {
	case slnx != "":
		p.Type = ".NET + Web"
		p.Main = slnx
		p.Build = fmt.Sprintf("dotnet build %q", slnx)
		p.Test = fmt.Sprintf("dotnet test %q --no-build", slnx)
		p.Audit = fmt.Sprintf("dotnet list %q package --vulnerable", slnx)
	case sln != "":
		p.Type = ".NET"
		p.Main = sln
		p.Build = fmt.Sprintf("dotnet build %q", sln)
		p.Test = fmt.Sprintf("dotnet test %q --no-build", sln)
		p.Audit = fmt.Sprintf("dotnet list %q package --vulnerable", sln)
	case exists(filepath.Join(r, "package.json")):
		p.Type = "Node"
		p.Main = filepath.Join(r, "package.json")
		p.Build = "npm run build"
		p.Test = "npm test"
		p.Audit = "npm audit --audit-level=moderate"
	case exists(filepath.Join(r, "pyproject.toml")) || exists(filepath.Join(r, "requirements.txt")):
		p.Type = "Python"
		p.Build = "python -m compileall ."
		p.Test = "python -m pytest"
	case exists(filepath.Join(r, "go.mod")):
		p.Type = "Go"
		p.Build = "go build ./..."
		p.Test = "go test ./..."
	}

	// Detect frontend alongside backend
	if exists(filepath.Join(r, "HConnect.Web", "package.json")) {
		p.FrontendBuild = "cd HConnect.Web && npm run build"
	}

	return p
}

func firstExt(r, ext string) string {
	var found string
	_ = filepath.WalkDir(r, func(p string, d os.DirEntry, e error) error {
		if e != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if skipDir(filepath.Base(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ext) {
			found = p
		}
		return nil
	})
	return found
}

func printProject(p Project) {
	fmt.Println("[MERA] Project Type:", p.Type)
	fmt.Println("[MERA] Main:        ", p.Main)
	fmt.Println("[MERA] Build:       ", p.Build)
	fmt.Println("[MERA] Test:        ", p.Test)
	fmt.Println("[MERA] Audit:       ", p.Audit)
	fmt.Println("[MERA] Frontend:    ", p.FrontendBuild)
}
