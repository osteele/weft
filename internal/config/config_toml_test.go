package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
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

func TestBugTrackerDefaultAndOverride(t *testing.T) {
	dir := t.TempDir()
	restore := SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	defer restore()

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := cfg.BugTracker()
	if err != nil {
		t.Fatal(err)
	}
	if tracker != BugTrackerGitHub {
		t.Fatalf("default bug tracker = %q, want %q", tracker, BugTrackerGitHub)
	}

	if err := SetBugTrackerSetting(BugTrackerLocal); err != nil {
		t.Fatalf("SetBugTrackerSetting: %v", err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	tracker, err = cfg.BugTracker()
	if err != nil {
		t.Fatal(err)
	}
	if tracker != BugTrackerLocal {
		t.Fatalf("configured bug tracker = %q, want %q", tracker, BugTrackerLocal)
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
	if !cfg.ProviderExplicitlyEnabled(cloud.ProviderVastai) {
		t.Fatal("vastai.enabled was not decoded from TOML")
	}
	if cfg.Vastai.DefaultImage != "nvidia/cuda:12.4.1-runtime-ubuntu22.04" {
		t.Fatalf("vastai.default_image = %q", cfg.Vastai.DefaultImage)
	}
	if cfg.Vastai.R2.AccountID != "acct" || cfg.Vastai.R2.AccessKeyID != "access" || cfg.Vastai.R2.SecretAccessKey != "secret" || cfg.Vastai.R2.Bucket != "bucket" {
		t.Fatalf("vastai.r2 decoded incorrectly: %+v", cfg.Vastai.R2)
	}
}

func TestNarrateQuietWindowDefaultAndOverride(t *testing.T) {
	var cfg Config
	if got := cfg.NarrateQuietWindow(); got != 5*time.Second {
		t.Fatalf("default quiet window = %s, want 5s", got)
	}

	cfg.AI.Narrate.QuietSeconds = 12
	if got := cfg.NarrateQuietWindow(); got != 12*time.Second {
		t.Fatalf("configured quiet window = %s, want 12s", got)
	}
}

func TestNarrateProviderAndModelDefaults(t *testing.T) {
	var cfg Config
	if got := cfg.NarrateProvider(); got != "anthropic" {
		t.Fatalf("default provider = %q, want anthropic", got)
	}
	if got := cfg.NarrateModel(); got != "claude-sonnet-4-20250514" {
		t.Fatalf("default model = %q", got)
	}

	cfg.LLM.Provider = "openrouter"
	if got := cfg.NarrateProvider(); got != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", got)
	}
	if got := cfg.NarrateModel(); got != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("openrouter default model = %q", got)
	}
}

func TestNarrateSlackMinIntervalDefaultAndOverride(t *testing.T) {
	var cfg Config
	if got := cfg.NarrateSlackMinInterval(); got != 5*time.Minute {
		t.Fatalf("default slack interval = %s, want 5m", got)
	}
	cfg.AI.Narrate.SlackMinIntervalSeconds = 120
	if got := cfg.NarrateSlackMinInterval(); got != 2*time.Minute {
		t.Fatalf("configured slack interval = %s, want 2m", got)
	}
}

func TestLoadTOMLDecodesNarrateProvider(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `
[llm]
provider = "openrouter"
model = "anthropic/claude-sonnet-4.6"

[ai.narrate]
slack = true
slack_min_interval_seconds = 300
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
	if cfg.NarrateProvider() != "openrouter" {
		t.Fatalf("provider = %q", cfg.NarrateProvider())
	}
	if cfg.NarrateModel() != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("model = %q", cfg.NarrateModel())
	}
	if !cfg.NarrateSlackEnabled() {
		t.Fatal("slack should be enabled")
	}
	if cfg.NarrateSlackMinInterval() != 5*time.Minute {
		t.Fatalf("slack interval = %s", cfg.NarrateSlackMinInterval())
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

[hosts.studio.benchmark]
cpu_threshold = 15
ram_threshold = 35
idle_samples = 2
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
	if cfg.Hosts["studio"].Benchmark.CPUThreshold != 15 {
		t.Fatalf("studio benchmark cpu_threshold = %d, want 15", cfg.Hosts["studio"].Benchmark.CPUThreshold)
	}
	if cfg.Hosts["studio"].Benchmark.RAMThreshold != 35 {
		t.Fatalf("studio benchmark ram_threshold = %d, want 35", cfg.Hosts["studio"].Benchmark.RAMThreshold)
	}
	if cfg.Hosts["studio"].Benchmark.IdleSamples != 2 {
		t.Fatalf("studio benchmark idle_samples = %d, want 2", cfg.Hosts["studio"].Benchmark.IdleSamples)
	}
	if len(UnknownTOMLKeys) != 0 {
		t.Fatalf("UnknownTOMLKeys = %#v, want empty", UnknownTOMLKeys)
	}
}

func TestAutoReplanStuckInventoryDispatchDefaultOffAndOverride(t *testing.T) {
	// Default (no config block, no key): disabled. The intervention changes
	// a job's placement and adds the `rental` tag, so it must be opt-in.
	var cfg Config
	if cfg.AutoReplanStuckInventoryDispatchEnabled() {
		t.Fatal("default should be off; an unconfigured Config{} returned enabled")
	}

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	content := `
[autopilot]
auto_replan_stuck_inventory_dispatch = true
`
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := SetConfigPathsForTesting(tomlPath, filepath.Join(dir, "config.yaml"))
	defer restore()

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.AutoReplanStuckInventoryDispatchEnabled() {
		t.Fatal("[autopilot] auto_replan_stuck_inventory_dispatch = true was not honored")
	}
}

func TestDataCacheEvictionDefaultsAndOverrides(t *testing.T) {
	var cfg Config
	if got := cfg.CacheEvictionPolicy(); got != CacheEvictionPolicyLRU {
		t.Fatalf("default policy = %q, want %q", got, CacheEvictionPolicyLRU)
	}
	if got := cfg.CacheReuseWindow(); got != 30*24*time.Hour {
		t.Fatalf("default reuse window = %s, want 720h", got)
	}

	cfg.Data.CacheEvictionPolicy = "reuse_per_gb"
	cfg.Data.CacheReuseWindow = "14d"
	if got := cfg.CacheEvictionPolicy(); got != CacheEvictionPolicyReusePerGB {
		t.Fatalf("policy = %q, want %q", got, CacheEvictionPolicyReusePerGB)
	}
	if got := cfg.CacheReuseWindow(); got != 14*24*time.Hour {
		t.Fatalf("reuse window = %s, want 336h", got)
	}

	cfg.Data.CacheEvictionPolicy = "unknown"
	cfg.Data.CacheReuseWindow = "bad"
	if got := cfg.CacheEvictionPolicy(); got != CacheEvictionPolicyLRU {
		t.Fatalf("invalid policy = %q, want %q", got, CacheEvictionPolicyLRU)
	}
	if got := cfg.CacheReuseWindow(); got != 30*24*time.Hour {
		t.Fatalf("invalid reuse window = %s, want default", got)
	}
}

func TestLoadTOMLDecodesDataCacheEvictionConfig(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `
[data]
cache_eviction_policy = "reuse_per_gb"
cache_reuse_window = "14d"
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
	if got := cfg.CacheEvictionPolicy(); got != CacheEvictionPolicyReusePerGB {
		t.Fatalf("policy = %q, want %q", got, CacheEvictionPolicyReusePerGB)
	}
	if got := cfg.CacheReuseWindow(); got != 14*24*time.Hour {
		t.Fatalf("reuse window = %s, want 336h", got)
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

func TestLoadTOMLDecodesAliases(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `
[aliases]
uj = "job list --group-by status --unprocessed --watch"
li = "launch instances --yes"
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
	if got := cfg.Aliases["uj"]; got != "job list --group-by status --unprocessed --watch" {
		t.Fatalf("aliases.uj = %q", got)
	}
	if got := cfg.Aliases["li"]; got != "launch instances --yes" {
		t.Fatalf("aliases.li = %q", got)
	}
}

func TestRegistryAuthForImage(t *testing.T) {
	t.Setenv("WEFT_TEST_REGISTRY_PASSWORD", "secret-token")
	cfg := &Config{
		Registry: map[string]RegistryConfig{
			"ghcr.io": {
				Username:       "osteele",
				PasswordEnv:    "WEFT_TEST_REGISTRY_PASSWORD",
				RunpodAuthName: "weft-ghcr",
			},
		},
	}

	auth, err := cfg.RegistryAuthForImage("ghcr.io/org/image:tag", "")
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		t.Fatal("expected registry auth")
	}
	if auth.Host != "ghcr.io" || auth.Username != "osteele" || auth.Password != "secret-token" || auth.Name != "weft-ghcr" {
		t.Fatalf("auth = %+v", auth)
	}
}

func TestRegistryAuthForImage_NamedSecret(t *testing.T) {
	t.Setenv("WEFT_TEST_REGISTRY_PASSWORD", "secret-token")
	cfg := &Config{
		Registry: map[string]RegistryConfig{
			"nvcr": {
				Username:    "$oauthtoken",
				PasswordEnv: "WEFT_TEST_REGISTRY_PASSWORD",
			},
		},
	}

	auth, err := cfg.RegistryAuthForImage("nvcr.io/nvidia/pytorch:24.01-py3", "nvcr")
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil || auth.Host != "nvcr" || auth.Name != "weft-nvcr" {
		t.Fatalf("auth = %+v", auth)
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

func TestCampaignReliability_DefaultAndExplicit(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.CampaignReliability(); got != 0.95 {
		t.Fatalf("CampaignReliability() default = %v, want 0.95", got)
	}

	cfg.Campaign.Reliability = float64Ptr(0.9)
	if got := cfg.CampaignReliability(); got != 0.9 {
		t.Fatalf("CampaignReliability() explicit = %v, want 0.9", got)
	}
}

func TestCampaignReliability_ZeroDisablesFilter(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Campaign.Reliability = float64Ptr(0)
	if got := cfg.CampaignReliability(); got != 0 {
		t.Fatalf("CampaignReliability() = %v, want 0", got)
	}
}

func TestCampaignReliability_InvalidFallsBack(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Campaign.Reliability = float64Ptr(-0.1)
	if got := cfg.CampaignReliability(); got != 0.95 {
		t.Fatalf("CampaignReliability() negative fallback = %v, want 0.95", got)
	}
	cfg.Campaign.Reliability = float64Ptr(1.1)
	if got := cfg.CampaignReliability(); got != 0.95 {
		t.Fatalf("CampaignReliability() >1 fallback = %v, want 0.95", got)
	}
}

func float64Ptr(v float64) *float64 {
	return &v
}

func TestLoadTOMLDecodesCloudDrainConfig(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	content := `[cloud.drain]
stall_timeout_seconds = 45
floor_throughput_bytes_per_sec = 524288
max_drain_seconds = 1200
baseline_seconds = 90
marker_timeout_seconds = 15
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
	d := cfg.Cloud.Drain
	if d.StallTimeoutSeconds != 45 {
		t.Errorf("stall_timeout_seconds = %d, want 45", d.StallTimeoutSeconds)
	}
	if d.FloorThroughputBytesPerSec != 524288 {
		t.Errorf("floor_throughput_bytes_per_sec = %d, want 524288", d.FloorThroughputBytesPerSec)
	}
	if d.MaxDrainSeconds != 1200 {
		t.Errorf("max_drain_seconds = %d, want 1200", d.MaxDrainSeconds)
	}
	if d.BaselineSeconds != 90 {
		t.Errorf("baseline_seconds = %d, want 90", d.BaselineSeconds)
	}
	if d.MarkerTimeoutSeconds != 15 {
		t.Errorf("marker_timeout_seconds = %d, want 15", d.MarkerTimeoutSeconds)
	}
}
