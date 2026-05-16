package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	DefaultModel     string                    `json:"defaultModel"`
	MapTokensFast    int                       `json:"mapTokensFast"`
	MapTokensNormal  int                       `json:"mapTokensNormal"`
	MapTokensDeep    int                       `json:"mapTokensDeep"`
	HeartbeatSeconds int                       `json:"heartbeatSeconds"`
	TimeoutSeconds   int                       `json:"timeoutSeconds"`
	MaxSessions      int                       `json:"maxSessions"`
	AutoCommit       bool                      `json:"autoCommit"`
	AutoPush         bool                      `json:"autoPush"`
	AutoDeploy       bool                      `json:"autoDeploy"`
	Stack            map[string]any            `json:"stack"`
	Modules          map[string]map[string]any `json:"modules"`
}

func defaultConfig() Config {
	return Config{
		SchemaVersion:    ConfigSchemaVersion,
		DefaultModel:     "qwen2.5-coder:7b",
		MapTokensFast:    256,
		MapTokensNormal:  512,
		MapTokensDeep:    1024,
		HeartbeatSeconds: 15,
		TimeoutSeconds:   600,
		MaxSessions:      50,
		AutoCommit:       false,
		AutoPush:         false,
		AutoDeploy:       false,
		Stack: map[string]any{
			"frontend": map[string]any{
				"framework":          "auto-detect",
				"styling":            "existing-project-patterns",
				"forbiddenLibraries": []string{"antd", "@mui/material", "bootstrap", "chakra-ui"},
				"rules": []string{
					"Use existing components",
					"No new deps without approval",
					"Minimal scoped changes",
				},
			},
			"backend": map[string]any{
				"framework": "auto-detect",
				"rules": []string{
					"Thin controllers",
					"No secrets in code",
					"Do not change routes unless requested",
					"Add tests for new logic",
				},
			},
		},
		Modules: map[string]map[string]any{
			"Identity": {"forbidden": []string{"Gateway/**", "PaymentGateway/**"}},
			"Gateway":  {"forbidden": []string{"Identity/**", "PaymentGateway/**"}},
		},
	}
}

func loadConfig() Config {
	cfg := defaultConfig()
	path := filepath.Join(meraDir(), "config.json")
	bakPath := path + ".bak"

	b, readErr := os.ReadFile(path)
	if readErr != nil {
		return cfg
	}

	if json.Unmarshal(b, &cfg) != nil {
		fmt.Fprintf(os.Stderr, "[WARN] config.json is corrupt — attempting recovery from backup\n")
		appendMeraLog("WARN", "config.json corrupt, trying backup")
		if bak, bakErr := os.ReadFile(bakPath); bakErr == nil && json.Unmarshal(bak, &cfg) == nil {
			fmt.Fprintf(os.Stderr, "[WARN] Recovered config from config.json.bak\n")
			appendMeraLog("WARN", "recovered config from .bak")
		} else {
			fmt.Fprintf(os.Stderr, "[WARN] Backup unavailable or also corrupt — using defaults\n")
			appendMeraLog("WARN", "config recovery failed, using defaults")
			cfg = defaultConfig()
		}
		return cfg
	}

	if cfg.SchemaVersion < ConfigSchemaVersion {
		// Backup before migrating so a bad migration is recoverable.
		_ = os.WriteFile(bakPath, b, 0644)
		cfg.SchemaVersion = ConfigSchemaVersion
		if b2, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = writeNoBOM(path, b2)
		}
	}
	return cfg
}

func initProject() error {
	for _, d := range []string{
		meraDir(), reportsDir(), snapshotsDir(), sessionsDir(),
		filepath.Join(meraDir(), "agents"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(meraDir(), "config.json")
	if !exists(cfgPath) {
		b, _ := json.MarshalIndent(defaultConfig(), "", "  ")
		if err := writeNoBOM(cfgPath, b); err != nil {
			return err
		}
	}

	modelsPath := filepath.Join(meraDir(), "models.json")
	if !exists(modelsPath) {
		b, _ := json.MarshalIndent(defaultModelConfig(), "", "  ")
		if err := writeNoBOM(modelsPath, b); err != nil {
			return err
		}
	}

	_ = writeNoBOM(filepath.Join(root(), ".aider.conf.yml"),
		[]byte("auto-commits: false\ndirty-commits: false\nshow-diffs: true\npretty: true\n"))
	_ = writeNoBOM(filepath.Join(root(), ".aiderignore"),
		[]byte(".git\nbin\nobj\nnode_modules\n.next\ndist\nbuild\ncoverage\n.vs\n.idea\n*.user\n*.suo\n*.cache\n.mera/logs\n.mera/reports\n"))

	if !exists(filepath.Join(meraDir(), "history.md")) {
		_ = writeNoBOM(filepath.Join(meraDir(), "history.md"), []byte("# MERA History\n"))
	}

	fmt.Println("[OK] .mera structure ready.")
	return nil
}

func auth() error {
	key := os.Getenv("MERA_AUTH_KEY")
	if key == "" {
		return errors.New("MERA_AUTH_KEY is not set")
	}
	if promptLine("Enter MERA Access Key") != key {
		return errors.New("invalid MERA access key")
	}
	fmt.Println("[OK] Authentication successful.")
	return nil
}

// smokeTestAuth allows -SmokeTest without MERA_AUTH_KEY when no key is configured.
// If a key IS configured it still requires it — prevents accidental bypass.
func smokeTestAuth() error {
	key := os.Getenv("MERA_AUTH_KEY")
	if key == "" {
		return nil // no auth configured — diagnostic mode permitted
	}
	return auth()
}
