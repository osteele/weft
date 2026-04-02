package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoadTOMLDecodesSharedHostOverrides(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `
[hosts.cool30]
shared = true

[hosts.cool100]
backend = "queue-runner"
`
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := SetConfigPathsForTesting(tomlPath, filepath.Join(dir, "config.yaml"))
	defer restore()

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HostShared("cool30") {
		t.Fatal("cool30 should be marked shared")
	}
	if cfg.HostShared("cool100") {
		t.Fatal("cool100 should not be marked shared")
	}
	if cfg.HostShared("missing") {
		t.Fatal("missing host should not be marked shared")
	}
}

func TestLoadTOMLDecodesCampaignRetryLimits(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `
[campaign]
retry_first_time_limit = "50m"
retry_first_cost_limit = 1.75
retry_next_time_limit = "20m"
retry_next_cost_limit = 0.40
`
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := SetConfigPathsForTesting(tomlPath, filepath.Join(dir, "config.yaml"))
	defer restore()

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RetryFirstTimeLimit(); got != 50*time.Minute {
		t.Fatalf("RetryFirstTimeLimit() = %v, want 50m", got)
	}
	if got := cfg.RetryFirstCostLimitCents(); got != 175 {
		t.Fatalf("RetryFirstCostLimitCents() = %d, want 175", got)
	}
	if got := cfg.RetryNextTimeLimit(); got != 20*time.Minute {
		t.Fatalf("RetryNextTimeLimit() = %v, want 20m", got)
	}
	if got := cfg.RetryNextCostLimitCents(); got != 40 {
		t.Fatalf("RetryNextCostLimitCents() = %d, want 40", got)
	}
}

func TestCampaignRetryLimitsDefaultsAndFallbacks(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.RetryFirstTimeLimit(); got != 45*time.Minute {
		t.Fatalf("RetryFirstTimeLimit() = %v, want 45m", got)
	}
	if got := cfg.RetryNextTimeLimit(); got != 45*time.Minute {
		t.Fatalf("RetryNextTimeLimit() = %v, want 45m", got)
	}
	if got := cfg.RetryFirstCostLimitCents(); got != 100 {
		t.Fatalf("RetryFirstCostLimitCents() = %d, want 100", got)
	}
	if got := cfg.RetryNextCostLimitCents(); got != 25 {
		t.Fatalf("RetryNextCostLimitCents() = %d, want 25", got)
	}

	cfg.Campaign.RetryFirstTimeLimit = "invalid"
	cfg.Campaign.RetryNextTimeLimit = "-10m"
	cfg.Campaign.RetryFirstCostLimit = -1
	cfg.Campaign.RetryNextCostLimit = -0.1

	if got := cfg.RetryFirstTimeLimit(); got != 45*time.Minute {
		t.Fatalf("invalid RetryFirstTimeLimit() fallback = %v, want 45m", got)
	}
	if got := cfg.RetryNextTimeLimit(); got != 45*time.Minute {
		t.Fatalf("invalid RetryNextTimeLimit() fallback = %v, want 45m", got)
	}
	if got := cfg.RetryFirstCostLimitCents(); got != 100 {
		t.Fatalf("invalid RetryFirstCostLimitCents() fallback = %d, want 100", got)
	}
	if got := cfg.RetryNextCostLimitCents(); got != 25 {
		t.Fatalf("invalid RetryNextCostLimitCents() fallback = %d, want 25", got)
	}
}
