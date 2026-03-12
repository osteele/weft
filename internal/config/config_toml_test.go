package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPrefersTOMLConfig(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(tomlPath, []byte("[sync]\nexclude_dirs = [\"lab-notebook\"]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yamlPath, []byte("sync:\n  exclude_dirs:\n    - should-not-win\n"), 0644); err != nil {
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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sync.ExcludeDirs; len(got) == 0 || got[0] != "lab-notebook" {
		t.Fatalf("exclude_dirs = %v, want TOML value", got)
	}
}

func TestLoadFallsBackToLegacyYAMLConfig(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	yamlPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(yamlPath, []byte("sync:\n  exclude_dirs:\n    - legacy-dir\n"), 0644); err != nil {
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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sync.ExcludeDirs; len(got) == 0 || got[0] != "legacy-dir" {
		t.Fatalf("exclude_dirs = %v, want legacy YAML value", got)
	}
}

func TestLoadTOMLDecodesNestedVastaiR2Config(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `[predictor]
project_path = "/tmp/job-estimator"

[vastai]
enabled = true
default_image = "nvidia/cuda:12.4.1-runtime-ubuntu22.04"

[vastai.r2]
account_id = "acct"
access_key_id = "access"
secret_access_key = "secret"
bucket = "bucket"
`
	if err := os.WriteFile(tomlPath, []byte(content), 0644); err != nil {
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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Predictor.ProjectPath != "/tmp/job-estimator" {
		t.Fatalf("predictor.project_path = %q", cfg.Predictor.ProjectPath)
	}
	if !cfg.Vastai.Enabled {
		t.Fatal("vastai.enabled was not decoded from TOML")
	}
	if cfg.Vastai.DefaultImage != "nvidia/cuda:12.4.1-runtime-ubuntu22.04" {
		t.Fatalf("vastai.default_image = %q", cfg.Vastai.DefaultImage)
	}
	if cfg.Vastai.R2.AccountID != "acct" || cfg.Vastai.R2.AccessKeyID != "access" || cfg.Vastai.R2.SecretAccessKey != "secret" || cfg.Vastai.R2.Bucket != "bucket" {
		t.Fatalf("vastai.r2 decoded incorrectly: %+v", cfg.Vastai.R2)
	}
}
