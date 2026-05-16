package main

import "fmt"

// These vars are overridden at build time via:
//
//	go build -ldflags "-X main.BuildVersion=10.0.1 -X main.BuildDate=2026-05-14"
var (
	BuildVersion     = "10.0.0"
	BuildDate        = "2026-05-14"
	InstallerVersion = "10.0.0"
)

func printVersion() {
	fmt.Println()
	fmt.Printf("MERA Go v%s\n", BuildVersion)
	fmt.Printf("  Build date       : %s\n", BuildDate)
	fmt.Printf("  Config schema    : v%d\n", ConfigSchemaVersion)
	fmt.Printf("  Models schema    : v%d\n", ModelsSchemaVersion)
	fmt.Printf("  Installer        : v%s\n", InstallerVersion)

	// If a release manifest exists beside the binary, show release info.
	m, err := loadReleaseManifest()
	if err == nil {
		fmt.Printf("  Release date     : %s\n", m.ReleaseDate)
		fmt.Printf("  Min installer    : v%s\n", m.MinInstallerVersion)
	}
	fmt.Println()
}
