package config

import (
	"os"
	"path/filepath"
	"testing"

	toml "github.com/pelletier/go-toml"
)

func TestUpdateGlobalTOMLPreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[sync]
exclude_dirs = ["lab-notebook"]

[runpod]
enabled = false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := configPath
	origLegacy := legacyConfigPath
	configPath = tomlPath
	legacyConfigPath = filepath.Join(dir, "config.yaml")
	defer func() {
		configPath = origPath
		legacyConfigPath = origLegacy
	}()

	if err := UpdateGlobalTOML(func(tree *toml.Tree) error {
		tree.SetPath([]string{"runpod", "enabled"}, true)
		tree.SetPath([]string{"runpod", "bootstrap_template_id"}, "tpl-bootstrap")
		return nil
	}); err != nil {
		t.Fatalf("UpdateGlobalTOML: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Runpod.Enabled {
		t.Fatal("runpod.enabled was not updated")
	}
	if cfg.Runpod.BootstrapTemplateID != "tpl-bootstrap" {
		t.Fatalf("bootstrap_template_id = %q", cfg.Runpod.BootstrapTemplateID)
	}
	if len(cfg.Sync.ExcludeDirs) != 1 || cfg.Sync.ExcludeDirs[0] != "lab-notebook" {
		t.Fatalf("sync.exclude_dirs = %v", cfg.Sync.ExcludeDirs)
	}
}

func TestUpdateGlobalTOMLMigratesLegacyYAML(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
sync:
  exclude_dirs:
    - legacy-dir
runpod:
  enabled: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := configPath
	origLegacy := legacyConfigPath
	configPath = tomlPath
	legacyConfigPath = yamlPath
	defer func() {
		configPath = origPath
		legacyConfigPath = origLegacy
	}()

	if err := UpdateGlobalTOML(func(tree *toml.Tree) error {
		tree.SetPath([]string{"runpod", "enabled"}, true)
		tree.SetPath([]string{"runpod", "bootstrap_template_id"}, "tpl-123")
		return nil
	}); err != nil {
		t.Fatalf("UpdateGlobalTOML: %v", err)
	}

	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatalf("expected TOML config to be written: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Runpod.Enabled {
		t.Fatal("runpod.enabled was not migrated")
	}
	if cfg.Runpod.BootstrapTemplateID != "tpl-123" {
		t.Fatalf("bootstrap_template_id = %q", cfg.Runpod.BootstrapTemplateID)
	}
	if len(cfg.Sync.ExcludeDirs) != 1 || cfg.Sync.ExcludeDirs[0] != "legacy-dir" {
		t.Fatalf("sync.exclude_dirs = %v", cfg.Sync.ExcludeDirs)
	}
}
