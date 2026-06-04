package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/cloud"
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
	if !cfg.ProviderExplicitlyEnabled(cloud.ProviderRunpod) {
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
	if !cfg.ProviderExplicitlyEnabled(cloud.ProviderRunpod) {
		t.Fatal("runpod.enabled was not migrated")
	}
	if cfg.Runpod.BootstrapTemplateID != "tpl-123" {
		t.Fatalf("bootstrap_template_id = %q", cfg.Runpod.BootstrapTemplateID)
	}
	if len(cfg.Sync.ExcludeDirs) != 1 || cfg.Sync.ExcludeDirs[0] != "legacy-dir" {
		t.Fatalf("sync.exclude_dirs = %v", cfg.Sync.ExcludeDirs)
	}
}

func TestSetProviderEnabledSetting(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")
	restore := SetConfigPathsForTesting(tomlPath, yamlPath)
	defer restore()

	if err := SetProviderEnabledSetting(cloud.ProviderRunpod, Bool(true)); err != nil {
		t.Fatalf("SetProviderEnabledSetting(true): %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProviderExplicitlyEnabled(cloud.ProviderRunpod) {
		t.Fatal("runpod should be explicitly enabled")
	}

	if err := SetProviderEnabledSetting(cloud.ProviderRunpod, Bool(false)); err != nil {
		t.Fatalf("SetProviderEnabledSetting(false): %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load after disable: %v", err)
	}
	if !cfg.ProviderExplicitlyDisabled(cloud.ProviderRunpod) {
		t.Fatal("runpod should be explicitly disabled")
	}

	if err := SetProviderEnabledSetting(cloud.ProviderRunpod, nil); err != nil {
		t.Fatalf("SetProviderEnabledSetting(nil): %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	setting, err := cfg.ProviderEnabledSetting(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("ProviderEnabledSetting: %v", err)
	}
	if setting != nil {
		t.Fatalf("runpod enabled setting = %v, want nil", *setting)
	}
}

func TestSetAutoRunawaySpendNoProgressLimit(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	origPath := configPath
	origLegacy := legacyConfigPath
	configPath = tomlPath
	legacyConfigPath = yamlPath
	defer func() {
		configPath = origPath
		legacyConfigPath = origLegacy
	}()

	if err := SetAutoRunawaySpendNoProgressLimit(7.5); err != nil {
		t.Fatalf("SetAutoRunawaySpendNoProgressLimit: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Campaign.AutoRunawaySpendNoProgressLimit != 7.5 {
		t.Fatalf("AutoRunawaySpendNoProgressLimit = %v, want 7.5", cfg.Campaign.AutoRunawaySpendNoProgressLimit)
	}
	if cfg.AutoRunawaySpendNoProgressLimitCents() != 750 {
		t.Fatalf("cents = %d, want 750", cfg.AutoRunawaySpendNoProgressLimitCents())
	}

	// Negative clamps to 0; getter then falls back to compiled-in default.
	if err := SetAutoRunawaySpendNoProgressLimit(-1); err != nil {
		t.Fatalf("SetAutoRunawaySpendNoProgressLimit(-1): %v", err)
	}
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Campaign.AutoRunawaySpendNoProgressLimit != 0 {
		t.Fatalf("after negative, value = %v, want 0", cfg2.Campaign.AutoRunawaySpendNoProgressLimit)
	}
}
