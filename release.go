package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReleaseManifest describes a packaged MERA release.
// Shipped as manifest.json inside the release zip.
type ReleaseManifest struct {
	ManifestSchemaVersion int               `json:"manifestSchemaVersion"`
	Version               string            `json:"version"`
	ReleaseDate           string            `json:"releaseDate"`
	Files                 []string          `json:"files"`
	Checksums             map[string]string `json:"checksums"`
	RequiredConfigSchema  int               `json:"requiredConfigSchema"`
	RequiredModelsSchema  int               `json:"requiredModelsSchema"`
	MinInstallerVersion   string            `json:"minInstallerVersion"`
	Changelog             string            `json:"changelog,omitempty"`
}

// manifestSearchPaths returns candidate locations for manifest.json.
func manifestSearchPaths() []string {
	exe, _ := os.Executable()
	return []string{
		filepath.Join(filepath.Dir(exe), "manifest.json"),
		filepath.Join(".", "manifest.json"),
		filepath.Join("outputs", "manifest.json"),
	}
}

// loadReleaseManifest finds and reads manifest.json from standard locations.
func loadReleaseManifest() (ReleaseManifest, error) {
	var m ReleaseManifest
	for _, p := range manifestSearchPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return m, fmt.Errorf("invalid manifest at %s: %w", p, err)
		}
		return m, nil
	}
	return m, fmt.Errorf("manifest.json not found")
}

// sha256File computes the hex-encoded SHA-256 checksum of a file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyExeChecksum checks the running binary against a manifest.
// Returns (match, expected, actual, error).
func verifyExeChecksum(m ReleaseManifest) (bool, string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, "", "", err
	}
	name := filepath.Base(exe)
	expected, ok := m.Checksums[name]
	if !ok {
		expected = m.Checksums["mera.exe"] // fallback key
	}
	if expected == "" {
		return false, "", "", fmt.Errorf("mera.exe checksum not found in manifest — integrity cannot be verified")
	}
	actual, err := sha256File(exe)
	if err != nil {
		return false, expected, "", err
	}
	return actual == expected, expected, actual, nil
}

// generateManifest creates a ReleaseManifest for the current build.
// Called by build.ps1 via: mera -GenManifest (developer-only).
func generateManifest(outputDir string) error {
	files := []string{"mera.exe", "setup.ps1", "CHANGELOG.md", "README.md", "VERSION.txt"}
	checksums := map[string]string{}

	for _, name := range files {
		p := filepath.Join(outputDir, name)
		if !exists(p) {
			continue
		}
		sum, err := sha256File(p)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", name, err)
		}
		checksums[name] = sum
	}

	m := ReleaseManifest{
		ManifestSchemaVersion: 1,
		Version:               BuildVersion,
		ReleaseDate:           BuildDate,
		Files:                 files,
		Checksums:             checksums,
		RequiredConfigSchema:  ConfigSchemaVersion,
		RequiredModelsSchema:  ModelsSchemaVersion,
		MinInstallerVersion:   InstallerVersion,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(outputDir, "manifest.json")
	if err := writeNoBOM(path, b); err != nil {
		return err
	}
	fmt.Println("[OK] manifest.json:", path)
	return nil
}
