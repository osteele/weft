package config

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	toml "github.com/pelletier/go-toml"
	"gopkg.in/yaml.v3"
)

// UnknownTOMLKeys is set by Load when the config file contains keys that don't
// map to any struct field. Callers (the CLI bootstrap) can read this to surface
// a single warning to the user without coupling config loading to stderr.
var UnknownTOMLKeys []string

// Config holds application configuration
type Config struct {
	// DefaultCommand is the command to run when no arguments are provided
	// Valid values: "help", "watch", "list", "tui", "web"
	DefaultCommand string `yaml:"default_command" toml:"default_command"`
	// Aliases maps shorthand command names to command fragments.
	// Example: aliases.uj = "job list --unprocessed --watch"
	Aliases map[string]string `yaml:"aliases" toml:"aliases"`

	// TUI polling intervals (in seconds)
	// SyncInterval is the legacy TUI sync interval (seconds).
	// Deprecated in favor of SyncActiveInterval/SyncIdleInterval.
	SyncInterval int `yaml:"sync_interval" toml:"sync_interval"`
	// SyncActiveInterval is how often to sync hosts with running/queued jobs (seconds).
	SyncActiveInterval int `yaml:"sync_active_interval" toml:"sync_active_interval"`
	// SyncIdleInterval is how often to sync idle hosts (seconds).
	SyncIdleInterval int `yaml:"sync_idle_interval" toml:"sync_idle_interval"`
	// LogRefreshInterval is how often to refresh logs for selected running jobs
	LogRefreshInterval int `yaml:"log_refresh_interval" toml:"log_refresh_interval"`
	// HostRefreshInterval is how often to refresh host info in hosts view
	HostRefreshInterval int `yaml:"host_refresh_interval" toml:"host_refresh_interval"`

	// EnableMouse toggles mouse support in the TUI (disables terminal selection when true)
	EnableMouse bool `yaml:"enable_mouse" toml:"enable_mouse"`

	// LogCacheMaxAge is how long to keep cached log files (in days)
	// Default: 7 days. Set to 0 to disable caching.
	LogCacheMaxAge int `yaml:"log_cache_max_age" toml:"log_cache_max_age"`

	// LogCacheMaxSize is the maximum size of log files to cache (in bytes)
	// Default: 1048576 (1MB). Logs larger than this are not cached.
	LogCacheMaxSize int `yaml:"log_cache_max_size" toml:"log_cache_max_size"`

	// ShowUsageHints toggles whether CLI commands print follow-up suggestions
	ShowUsageHints bool `yaml:"show_usage_hints" toml:"show_usage_hints"`

	// ShowRentalHints toggles whether unplaced-job warnings about rental GPUs are printed
	ShowRentalHints bool `yaml:"show_rental_hints" toml:"show_rental_hints"`

	// AI/LLM configuration for automatic job description generation
	AI AIConfig `yaml:"ai" toml:"ai"`

	// LLM holds shared provider settings for API-backed model features.
	LLM LLMConfig `yaml:"llm" toml:"llm"`

	// BlockedCommandPatterns lists substrings that should cause an error if found in job commands.
	// Each entry has a pattern (substring to match) and an error message to display.
	BlockedCommandPatterns []BlockedPattern `yaml:"blocked_command_patterns" toml:"blocked_command_patterns"`

	// OperationLogEnabled controls whether operation logging is enabled
	// Default: true
	OperationLogEnabled *bool `yaml:"operation_log_enabled" toml:"operation_log_enabled"`

	// OperationLogMaxSize is the maximum size of the operation log file in bytes
	// When exceeded, the file is rotated (old content moved to .1 backup)
	// Default: 10MB (10485760)
	OperationLogMaxSize int64 `yaml:"operation_log_max_size" toml:"operation_log_max_size"`

	// WebEnabled controls whether the web UI starts with the TUI.
	WebEnabled bool `yaml:"web_enabled" toml:"web_enabled"`

	// WebPort is the localhost port for the web UI.
	WebPort int `yaml:"web_port" toml:"web_port"`

	// SSH connection settings
	SSH SSHConfig `yaml:"ssh" toml:"ssh"`

	// Cloud holds cloud-provider shared configuration.
	Cloud CloudConfig `yaml:"cloud" toml:"cloud"`

	// Hosts holds per-host configuration overrides.
	Hosts map[string]HostConfig `yaml:"hosts" toml:"hosts"`

	// Predictor holds job-estimator configuration for duration/resource estimation
	Predictor PredictorConfig `yaml:"predictor" toml:"predictor"`

	// Vastai holds Vast.ai cloud GPU and R2 result storage configuration
	Vastai VastaiConfig `yaml:"vastai" toml:"vastai"`

	// Runpod holds Runpod cloud GPU configuration
	Runpod RunpodConfig `yaml:"runpod" toml:"runpod"`

	// Registry holds private Docker registry credentials keyed by registry host.
	Registry map[string]RegistryConfig `yaml:"registry" toml:"registry"`

	// CoordinatorHost is the host where the coordinator daemon runs.
	CoordinatorHost string `yaml:"coordinator_host" toml:"coordinator_host"`

	// Campaign holds cloud campaign defaults
	Campaign CampaignConfig `yaml:"campaign" toml:"campaign"`

	// Sync holds source-sync defaults.
	Sync SyncConfig `yaml:"sync" toml:"sync"`

	// Data holds data locality and cache-management defaults.
	Data DataConfig `yaml:"data" toml:"data"`

	// Remediation holds auto-remediation configuration for failed jobs
	Remediation RemediationConfig `yaml:"remediation" toml:"remediation"`

	// Notifications configures a local command run when a job reaches a terminal status.
	Notifications NotificationsConfig `yaml:"notifications" toml:"notifications"`

	// Telemetry controls durable scheduling-analysis logs.
	Telemetry TelemetryConfig `yaml:"telemetry" toml:"telemetry"`

	// AgentBuild holds on-demand remote builder configuration for agent binaries.
	AgentBuild AgentBuildConfig `yaml:"agent_build" toml:"agent_build"`

	// Autopilot holds singleton-autopilot policy knobs (intervention behavior).
	Autopilot AutopilotConfig `yaml:"autopilot" toml:"autopilot"`

	// AutomapDirs lists local prefixes that should reuse the same relative path
	// on remote hosts; defaults to ["~"].
	AutomapDirs []string `yaml:"automap_dirs" toml:"automap_dirs"`
}

// AutopilotConfig gates optional autopilot interventions that change a job's
// placement (host pin, tags) without explicit user action. These are off by
// default because they can convert an on-prem inventory job into a cloud
// rental job, which is rarely what the user wants when the inventory host's
// problem is transient (slow HF download, temporary SSH issue, host reboot).
type AutopilotConfig struct {
	// AutoReplanStuckInventoryDispatch enables the autopilot rule that
	// automatically unplaces a job from its inventory host and adds the
	// `rental` tag after `inventory_dispatch_auto_replan_after` (10 min) of
	// sustained dispatch failures.
	//
	// When false (the default), an inventory job that's blocked on its host
	// stays blocked until the user intervenes (fix the underlying issue,
	// move the job, or restart it). The user-visible "blocked for Nm"
	// explanation is unchanged; only the automatic intervention is gated.
	//
	// When true, the previous behavior is restored: jobs blocked >10 min
	// on an inventory host are auto-unplaced and the `rental` tag is added,
	// making them eligible for cloud relaunch.
	//
	// See specs/campaign-lifecycle.allium § AutoReplanStuckInventoryDispatch.
	AutoReplanStuckInventoryDispatch bool `yaml:"auto_replan_stuck_inventory_dispatch" toml:"auto_replan_stuck_inventory_dispatch"`
}

// AgentBuildConfig configures remote builders used to compile agent binaries.
type AgentBuildConfig struct {
	// LinuxAMD64Builders is an ordered list of builders tried for linux/amd64.
	// Valid builder types are "ssh" and "fly".
	LinuxAMD64Builders []AgentBuilder `yaml:"linux_amd64_builders" toml:"linux_amd64_builders"`
}

// AgentBuilder defines one remote builder candidate.
type AgentBuilder struct {
	// Type selects the builder implementation ("ssh" or "fly").
	Type string `yaml:"type" toml:"type"`

	// Name is an optional label for logs and diagnostics.
	Name string `yaml:"name" toml:"name"`

	// SSH builder settings.
	Host      string `yaml:"host" toml:"host"`
	RemoteDir string `yaml:"remote_dir" toml:"remote_dir"`
	GoBin     string `yaml:"go_bin" toml:"go_bin"`

	// Fly builder settings.
	App        string `yaml:"app" toml:"app"`
	Machine    string `yaml:"machine" toml:"machine"`
	BaseDir    string `yaml:"base_dir" toml:"base_dir"`
	ZigTarget  string `yaml:"zig_target" toml:"zig_target"`
	ZigVersion string `yaml:"zig_version" toml:"zig_version"`
}

// SyncConfig holds source sync and packaging defaults.
type SyncConfig struct {
	// ExcludeDirs are additional path-component patterns excluded from source sync
	// and campaign tarballs for all projects.
	ExcludeDirs []string `yaml:"exclude_dirs" toml:"exclude_dirs"`
}

// DataConfig configures data locality and cache management.
type DataConfig struct {
	// CacheEvictionPolicy selects the ordering used by `weft data evict`.
	// Valid values: "lru" and "reuse_per_gb". Empty defaults to "lru".
	CacheEvictionPolicy string `yaml:"cache_eviction_policy" toml:"cache_eviction_policy"`
	// CacheReuseWindow is the lookback window for reuse_per_gb recent-use counts.
	// Empty defaults to 30d.
	CacheReuseWindow string `yaml:"cache_reuse_window" toml:"cache_reuse_window"`
}

var defaultSourceExcludeDirs = []string{"runs", "wand", "wandb"}

// Bool returns a pointer to v for tri-state config fields.
func Bool(v bool) *bool {
	return &v
}

// NotificationsConfig configures local job-completion notifications.
type NotificationsConfig struct {
	// Command is run via `sh -c` when a job reaches a terminal status
	// (completed or failed), with job context in WEFT_JOB_* environment
	// variables: WEFT_JOB_ID, WEFT_JOB_STATUS, WEFT_JOB_EXIT_CODE,
	// WEFT_JOB_DIR, WEFT_JOB_DESCRIPTION, WEFT_JOB_HOST, WEFT_JOB_SUMMARY.
	// Empty disables notifications.
	Command string `yaml:"command" toml:"command"`
}

// RemediationConfig holds configuration for automatic job failure remediation.
type RemediationConfig struct {
	// CodingAgent is the command to invoke for code fixes (e.g., "claude -p").
	// Empty means no coding agent is used (opt-out by default).
	CodingAgent string `yaml:"coding_agent" toml:"coding_agent"`
	// CodingAgentDir is the working directory for the agent (defaults to job's working dir).
	CodingAgentDir string `yaml:"coding_agent_dir" toml:"coding_agent_dir"`
}

// TelemetryConfig controls sampling and experiment assignment for scheduling
// analysis logs.
type TelemetryConfig struct {
	PlacementAlternativeSampleRate float64 `yaml:"placement_alternative_sample_rate" toml:"placement_alternative_sample_rate"`
	PlacementAlternativeTopK       int     `yaml:"placement_alternative_top_k" toml:"placement_alternative_top_k"`
	DonorABEnabled                 *bool   `yaml:"donor_ab_enabled" toml:"donor_ab_enabled"`
	DonorABSampleRate              float64 `yaml:"donor_ab_sample_rate" toml:"donor_ab_sample_rate"`
}

// CampaignConfig holds cloud campaign defaults.
type CampaignConfig struct {
	// Reliability is the default provider-offer reliability floor (0-1) used
	// when searching rental offers. Set to 0 to disable reliability filtering.
	Reliability *float64 `yaml:"reliability" toml:"reliability"`

	// GracePeriod is the default grace period after job failure (e.g., "5m", "15m").
	// Default: "5m"
	GracePeriod string `yaml:"grace_period" toml:"grace_period"`

	// GPUWarmup enables a lightweight CUDA warmup before the first GPU benchmark
	// job on a cloud instance. Default: false (disabled).
	GPUWarmup bool `yaml:"gpu_warmup" toml:"gpu_warmup"`

	// RetryFirstTimeLimit is the hard wall-clock cap for the first cloud retry
	// attempt (e.g. "45m"). Empty uses the default.
	RetryFirstTimeLimit string `yaml:"retry_first_time_limit" toml:"retry_first_time_limit"`
	// RetryFirstCostLimit is the hard spend cap (USD) for the first cloud retry attempt.
	RetryFirstCostLimit float64 `yaml:"retry_first_cost_limit" toml:"retry_first_cost_limit"`
	// RetryNextTimeLimit is the hard wall-clock cap for second+ cloud retry attempts.
	RetryNextTimeLimit string `yaml:"retry_next_time_limit" toml:"retry_next_time_limit"`
	// RetryNextCostLimit is the hard spend cap (USD) for second+ cloud retry attempts.
	RetryNextCostLimit float64 `yaml:"retry_next_cost_limit" toml:"retry_next_cost_limit"`

	// AutoRunawayEnabled enables the unattended relaunch runaway breaker.
	// nil defaults to true.
	AutoRunawayEnabled *bool `yaml:"auto_runaway_enabled" toml:"auto_runaway_enabled"`
	// AutoRunawayWindow is the lookback horizon for runaway detection.
	AutoRunawayWindow string `yaml:"auto_runaway_window" toml:"auto_runaway_window"`
	// AutoRunawayChainNoProgressLimit is the max trailing orphaned-chain length
	// before tripping the runaway breaker.
	AutoRunawayChainNoProgressLimit int `yaml:"auto_runaway_chain_no_progress_limit" toml:"auto_runaway_chain_no_progress_limit"`
	// AutoRunawayOrphanChurnLimit is the max orphaned attempts in window before trip.
	AutoRunawayOrphanChurnLimit int `yaml:"auto_runaway_orphan_churn_limit" toml:"auto_runaway_orphan_churn_limit"`
	// AutoRunawayInfraFailureLimit is the max infra-side launch failures in window before trip.
	AutoRunawayInfraFailureLimit int `yaml:"auto_runaway_infra_failure_limit" toml:"auto_runaway_infra_failure_limit"`
	// AutoRunawaySpendNoProgressLimit is the max spend (USD) in-window with no completions.
	AutoRunawaySpendNoProgressLimit float64 `yaml:"auto_runaway_spend_no_progress_limit" toml:"auto_runaway_spend_no_progress_limit"`

	// AutoObjective controls unattended auto-planner optimization profile.
	// Valid values: "cost_first", "balanced", "time_first". Empty uses default.
	AutoObjective string `yaml:"auto_objective" toml:"auto_objective"`
	// ObjectiveCostWeight overrides the auto-planner cost weight when > 0.
	ObjectiveCostWeight float64 `yaml:"objective_cost_weight" toml:"objective_cost_weight"`
	// ObjectiveTimeWeight overrides the auto-planner time weight when > 0.
	ObjectiveTimeWeight float64 `yaml:"objective_time_weight" toml:"objective_time_weight"`
	// OpportunityCostWeight scales scarcity penalties for consuming reusable
	// instance capacity in plan scoring. 0 defaults to the built-in value.
	OpportunityCostWeight float64 `yaml:"opportunity_cost_weight" toml:"opportunity_cost_weight"`
	// AutoRunRateSoftTarget is the unattended autopilot launch-rate target in
	// dollars per hour. 0 disables the target.
	AutoRunRateSoftTarget float64 `yaml:"auto_run_rate_soft_target" toml:"auto_run_rate_soft_target"`

	// Hedge configures hedged launches: launching multiple instances on
	// distinct offers in parallel and culling losers once one survives the
	// early-mortality window. See campaign-lifecycle.allium § HedgeCohort.
	Hedge HedgeConfig `yaml:"hedge" toml:"hedge"`
}

// HedgeConfig controls hedged-launch behavior. Hedging trades a small
// extra spend on probe instances for far higher launch survival on
// flaky provider classes.
type HedgeConfig struct {
	// Enabled turns on hedged launches. Default: false.
	Enabled bool `yaml:"enabled" toml:"enabled"`
	// Count is the total number of instances launched per group when
	// hedging applies (primary + probes). Must be >= 2 to have effect.
	Count int `yaml:"count" toml:"count"`
	// MaxCostPerHour gates hedging on offer cost: hedge only when each
	// offer's hourly cost is at or below this dollar value. 0 disables
	// the gate (hedge any offer).
	MaxCostPerHour float64 `yaml:"max_cost_per_hour" toml:"max_cost_per_hour"`
}

// VastaiConfig holds Vast.ai cloud GPU settings.
type VastaiConfig struct {
	// Enabled controls whether Vast.ai cloud GPU options are available.
	// nil means automatic/default provider policy.
	Enabled *bool `yaml:"enabled" toml:"enabled"`
	// SpendingLimit is the maximum cost per job in dollars
	SpendingLimit float64 `yaml:"spending_limit" toml:"spending_limit"`
	// DefaultImage is the Docker image for cloud instances
	DefaultImage string `yaml:"default_image" toml:"default_image"`
	// MaxRuntime is the auto-kill threshold (e.g., "4h")
	MaxRuntime string `yaml:"max_runtime" toml:"max_runtime"`
	// R2 holds Cloudflare R2 result storage configuration
	R2 R2Config `yaml:"r2" toml:"r2"`
	// SyncTimeout is the timeout in seconds for R2 sync checks (default: 5)
	SyncTimeout int `yaml:"sync_timeout" toml:"sync_timeout"`
}

// R2Config holds Cloudflare R2 credentials and bucket settings.
type R2Config struct {
	// AccountID is the Cloudflare account ID
	AccountID string `yaml:"account_id" toml:"account_id"`
	// AccessKeyID is the R2 API access key
	AccessKeyID string `yaml:"access_key_id" toml:"access_key_id"`
	// SecretAccessKey is the R2 API secret key
	SecretAccessKey string `yaml:"secret_access_key" toml:"secret_access_key"`
	// Bucket is the R2 bucket name for storing results
	Bucket string `yaml:"bucket" toml:"bucket"`
}

// ToCloudR2Config converts to the cloud package R2Config type.
func (r R2Config) ToCloudR2Config() cloud.R2Config {
	return cloud.R2Config{
		AccountID:       r.AccountID,
		AccessKeyID:     r.AccessKeyID,
		SecretAccessKey: r.SecretAccessKey,
		Bucket:          r.Bucket,
	}
}

// RunpodConfig holds Runpod cloud GPU settings.
type RunpodConfig struct {
	// Enabled controls whether Runpod cloud GPU options are available.
	// nil means automatic/default provider policy.
	Enabled *bool `yaml:"enabled" toml:"enabled"`
	// SpendingLimit is the maximum cost per job in dollars
	SpendingLimit float64 `yaml:"spending_limit" toml:"spending_limit"`
	// DefaultImage is the Docker image for cloud instances
	DefaultImage string `yaml:"default_image" toml:"default_image"`
	// BootstrapTemplateID is the RunPod template ID whose startup command
	// installs deps and executes cloud.R2BootstrapTemplateStartCmd().
	BootstrapTemplateID string `yaml:"bootstrap_template_id" toml:"bootstrap_template_id"`
	// MaxRuntime is the auto-kill threshold (e.g., "4h")
	MaxRuntime string `yaml:"max_runtime" toml:"max_runtime"`
}

// RegistryConfig configures credentials for a private Docker registry.
type RegistryConfig struct {
	Username        string `yaml:"username" toml:"username"`
	PasswordEnv     string `yaml:"password_env" toml:"password_env"`
	PasswordCommand string `yaml:"password_command" toml:"password_command"`
	RunpodAuthName  string `yaml:"runpod_auth_name" toml:"runpod_auth_name"`
}

// BlockedPattern defines a substring that should not appear in job commands
type BlockedPattern struct {
	// Pattern is the substring to search for in commands
	Pattern string `yaml:"pattern" toml:"pattern"`
	// Message is the error message to display when the pattern is found
	Message string `yaml:"message" toml:"message"`
}

// LLMConfig holds shared API-backed LLM provider configuration.
type LLMConfig struct {
	// Provider selects the LLM API provider: "anthropic" or "openrouter".
	Provider string `yaml:"provider" toml:"provider"`
	// APIKey optionally stores the provider key. Environment variables are preferred.
	APIKey string `yaml:"api_key" toml:"api_key"`
	// Model is the default provider model ID for API-backed LLM features.
	Model string `yaml:"model" toml:"model"`
}

// AIConfig holds configuration for AI/LLM features
type AIConfig struct {
	// Enabled controls whether AI description generation is active
	// Default: true (if ollama is available)
	Enabled *bool `yaml:"enabled" toml:"enabled"`

	// Model specifies the ollama model to use for description generation
	// Default: "llama3.2"
	Model string `yaml:"model" toml:"model"`

	// Narrate configures `weft narrate` activity narration.
	Narrate NarrateConfig `yaml:"narrate" toml:"narrate"`
}

// NarrateConfig configures `weft narrate`.
type NarrateConfig struct {
	// Model overrides the shared LLM model ID for narrate.
	Model string `yaml:"model" toml:"model"`
	// TickSeconds is the polling interval in seconds (default: 30).
	TickSeconds int `yaml:"tick_seconds" toml:"tick_seconds"`
	// QuietSeconds is the DB-change quiet window before narrating (default: 5).
	QuietSeconds int `yaml:"quiet_seconds" toml:"quiet_seconds"`
	// MaxOutputTokens caps the model's reply size per tick (default: 1600).
	MaxOutputTokens int `yaml:"max_output_tokens" toml:"max_output_tokens"`
	// CompactionThresholdTokens is the accumulated-recap token count that triggers compaction (default: 15000).
	CompactionThresholdTokens int `yaml:"compaction_threshold_tokens" toml:"compaction_threshold_tokens"`
	// Slack posts narrate entries to the configured Slack webhook.
	Slack bool `yaml:"slack" toml:"slack"`
	// SlackMinIntervalSeconds rate-limits Slack posts (default: 300).
	SlackMinIntervalSeconds int `yaml:"slack_min_interval_seconds" toml:"slack_min_interval_seconds"`
}

// LLMProvider returns the configured shared LLM provider, or anthropic.
func (c *Config) LLMProvider() string {
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.LLM.Provider)) {
		case "openrouter":
			return "openrouter"
		case "anthropic", "":
			return "anthropic"
		default:
			return strings.ToLower(strings.TrimSpace(c.LLM.Provider))
		}
	}
	return "anthropic"
}

// LLMModel returns the configured shared LLM model ID, or the provider default.
func (c *Config) LLMModel() string {
	return c.LLMModelForProvider(c.LLMProvider())
}

// LLMModelForProvider returns the configured shared LLM model ID, or a provider default.
func (c *Config) LLMModelForProvider(provider string) string {
	if c != nil && strings.TrimSpace(c.LLM.Model) != "" {
		return strings.TrimSpace(c.LLM.Model)
	}
	if strings.EqualFold(strings.TrimSpace(provider), "openrouter") {
		return "anthropic/claude-sonnet-4.6"
	}
	return "claude-sonnet-4-20250514"
}

// NarrateProvider returns the provider used by narrate.
func (c *Config) NarrateProvider() string {
	return c.LLMProvider()
}

// NarrateModel returns the configured model ID, or the provider default.
func (c *Config) NarrateModel() string {
	return c.NarrateModelForProvider(c.NarrateProvider())
}

// NarrateModelForProvider returns the configured model ID, or a provider default.
func (c *Config) NarrateModelForProvider(provider string) string {
	if c != nil && strings.TrimSpace(c.AI.Narrate.Model) != "" {
		return strings.TrimSpace(c.AI.Narrate.Model)
	}
	return c.LLMModelForProvider(provider)
}

// NarrateTickInterval returns the configured tick interval, or 30s.
func (c *Config) NarrateTickInterval() time.Duration {
	if c != nil && c.AI.Narrate.TickSeconds > 0 {
		return time.Duration(c.AI.Narrate.TickSeconds) * time.Second
	}
	return 30 * time.Second
}

// NarrateQuietWindow returns the DB-change quiet window, or 5s.
func (c *Config) NarrateQuietWindow() time.Duration {
	if c != nil && c.AI.Narrate.QuietSeconds > 0 {
		return time.Duration(c.AI.Narrate.QuietSeconds) * time.Second
	}
	return 5 * time.Second
}

// NarrateMaxOutputTokens returns the configured per-tick output cap, or 1600.
func (c *Config) NarrateMaxOutputTokens() int {
	if c != nil && c.AI.Narrate.MaxOutputTokens > 0 {
		return c.AI.Narrate.MaxOutputTokens
	}
	return 1600
}

// NarrateCompactionThreshold returns the recap-token count that triggers
// compaction, or 15000.
func (c *Config) NarrateCompactionThreshold() int {
	if c != nil && c.AI.Narrate.CompactionThresholdTokens > 0 {
		return c.AI.Narrate.CompactionThresholdTokens
	}
	return 15000
}

// NarrateSlackEnabled reports whether narrate should post to Slack.
func (c *Config) NarrateSlackEnabled() bool {
	return c != nil && c.AI.Narrate.Slack
}

// NarrateSlackMinInterval returns the Slack post rate limit, or 5 minutes.
func (c *Config) NarrateSlackMinInterval() time.Duration {
	if c != nil && c.AI.Narrate.SlackMinIntervalSeconds > 0 {
		return time.Duration(c.AI.Narrate.SlackMinIntervalSeconds) * time.Second
	}
	return 5 * time.Minute
}

// SSHConfig holds SSH connection pool settings.
type SSHConfig struct {
	// PoolSize is the number of persistent sessions per host (default: 4).
	PoolSize int `yaml:"pool_size" toml:"pool_size"`
	// MaxParallel is the maximum concurrent SSH operations across all hosts (default: 8).
	MaxParallel int `yaml:"max_parallel" toml:"max_parallel"`
	// ConnectTimeout is the SSH connect timeout in seconds (default: 10).
	ConnectTimeout int `yaml:"connect_timeout" toml:"connect_timeout"`
}

// CloudConfig holds settings shared across cloud providers.
type CloudConfig struct {
	SSH   CloudSSHConfig   `yaml:"ssh" toml:"ssh"`
	Drain CloudDrainConfig `yaml:"drain" toml:"drain"`
}

// CloudDrainConfig tunes the agent's upload-drain gate. All fields are
// optional; zero values fall back to the r2upload package defaults. The
// shape mirrors r2upload.Options so each knob has one meaning.
type CloudDrainConfig struct {
	// StallTimeoutSeconds: once bytes start flowing, kill rclone after this
	// many seconds of no further byte progress. Default: 30.
	StallTimeoutSeconds int `yaml:"stall_timeout_seconds" toml:"stall_timeout_seconds"`
	// InitialStallTimeoutSeconds: before the first byte is observed, allow
	// this much time for rclone to complete pre-transfer setup (HEAD,
	// multipart init, chunk hashing) before declaring a never-started stall.
	// Default: 300 (5 min).
	InitialStallTimeoutSeconds int `yaml:"initial_stall_timeout_seconds" toml:"initial_stall_timeout_seconds"`
	// HeartbeatTimeoutSeconds: if rclone produces no stderr output for this
	// long, declare the process hung independent of byte progress. Default: 60.
	HeartbeatTimeoutSeconds int `yaml:"heartbeat_timeout_seconds" toml:"heartbeat_timeout_seconds"`
	// FloorThroughputBytesPerSec: bytes/sec used to compute the size-derived
	// ceiling and the pace-check threshold. Default: 262144 (256 KiB/s).
	FloorThroughputBytesPerSec int64 `yaml:"floor_throughput_bytes_per_sec" toml:"floor_throughput_bytes_per_sec"`
	// MaxDrainSeconds: absolute ceiling regardless of size. Default: 900 (15 min).
	MaxDrainSeconds int `yaml:"max_drain_seconds" toml:"max_drain_seconds"`
	// BaselineSeconds added to the ceiling for rclone startup + flush.
	// Default: 60.
	BaselineSeconds int `yaml:"baseline_seconds" toml:"baseline_seconds"`
	// MarkerTimeoutSeconds caps how long WriteFailureMarker is allowed to
	// take. Default: 10. Intentionally short so it can't recurse into a
	// long-running drain right before self-destruct.
	MarkerTimeoutSeconds int `yaml:"marker_timeout_seconds" toml:"marker_timeout_seconds"`
	// PaceCheckAfterSeconds: warmup before the slow-pace gate activates.
	// Lets TCP slow-start and rclone chunk pipelining ramp before we judge
	// average throughput. Negative disables. Default: 120 (2 min).
	PaceCheckAfterSeconds int `yaml:"pace_check_after_seconds" toml:"pace_check_after_seconds"`
	// MinThroughputFraction: after the pace-check warmup, the drain must
	// sustain at least this fraction of FloorThroughputBytesPerSec on
	// average (since drain start) or be killed for uneconomic pace.
	// Negative disables. Default: 0.25 (25%, so 64 KiB/s at the 256 KiB/s floor).
	MinThroughputFraction float64 `yaml:"min_throughput_fraction" toml:"min_throughput_fraction"`
}

// CloudSSHConfig holds cloud-rental SSH identity settings.
type CloudSSHConfig struct {
	IdentityFile  string `yaml:"identity_file" toml:"identity_file"`
	PublicKeyFile string `yaml:"public_key_file" toml:"public_key_file"`
}

// ExpandedIdentityFile returns the configured cloud SSH private key path.
func (c *CloudSSHConfig) ExpandedIdentityFile() string {
	if c == nil {
		return ""
	}
	return expandUserPath(c.IdentityFile)
}

// ExpandedPublicKeyFile returns the configured cloud SSH public key path.
func (c *CloudSSHConfig) ExpandedPublicKeyFile() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.PublicKeyFile) != "" {
		return expandUserPath(c.PublicKeyFile)
	}
	identity := c.ExpandedIdentityFile()
	if identity == "" {
		return ""
	}
	return identity + ".pub"
}

// HostConfig holds per-host configuration.
type HostConfig struct {
	// Static inventory fields. A host listed under [hosts.<name>] is part of
	// the inventory even if only some of these fields are populated.
	OS        string          `yaml:"os" toml:"os"`
	Arch      string          `yaml:"arch" toml:"arch"`
	CPUCores  int             `yaml:"cpu_cores" toml:"cpu_cores"`
	Memory    string          `yaml:"memory" toml:"memory"`
	NetworkBW string          `yaml:"network_bw" toml:"network_bw"`
	GPUs      []HostGPUConfig `yaml:"gpus" toml:"gpus"`
	// NVIDIADriverVersion and CUDAVersion are static compatibility fields
	// discovered from nvidia-smi. They may be set manually when discovery
	// cannot parse a host's driver output.
	NVIDIADriverVersion string  `yaml:"nvidia_driver" toml:"nvidia_driver"`
	CUDAVersion         string  `yaml:"cuda_version" toml:"cuda_version"`
	CPUFactor           float64 `yaml:"cpu_factor" toml:"cpu_factor"`
	GPUFactor           float64 `yaml:"gpu_factor" toml:"gpu_factor"`

	// HFCacheDir is the resolved HF hub cache directory on this host (e.g. /mnt/nas/.cache/huggingface/hub).
	// Set by `weft host discover` or manually. Used by prestage to construct correct rsync destination paths.
	HFCacheDir string `yaml:"hf_cache_dir" toml:"hf_cache_dir"`

	// Backend sets the execution backend for this host ("queue-runner" or "slurm").
	Backend string `yaml:"backend" toml:"backend"`
	// Shared marks an inventory host as multi-tenant, so benchmark auto-placement
	// avoids it unless the job is explicitly inventory-tagged.
	Shared bool `yaml:"shared" toml:"shared"`
	// OptInOnly excludes the host from auto-placement; it is only used when
	// explicitly selected via --host.
	OptInOnly bool `yaml:"opt_in_only" toml:"opt_in_only"`

	// SSHUser is the remote user weft connects as for this host. Empty
	// leaves it to ssh's default (typically the local username), which
	// is fine when ~/.ssh/config does the right thing. Set this when
	// you want weft to connect as a service user (e.g., "agent") that
	// is distinct from the user you use for interactive `ssh <host>`.
	SSHUser string `yaml:"ssh_user" toml:"ssh_user"`

	// SSHIdentityFile is the private key weft should offer when connecting
	// to this host. If unset, weft falls back to `cloud.ssh.identity_file`
	// when SSHUser is non-empty (legacy behavior), otherwise lets ssh use
	// its default agent / ~/.ssh/config resolution. Set this when the host
	// requires a key distinct from the cloud key (e.g., studio's `agent`
	// account only accepts ~/.ssh/agent_studio_ed25519).
	SSHIdentityFile string `yaml:"ssh_identity_file" toml:"ssh_identity_file"`

	// Benchmark configures how quiet this host must be before benchmark-tagged
	// jobs are allowed to start.
	Benchmark HostBenchmarkConfig `yaml:"benchmark" toml:"benchmark"`
}

// HostBenchmarkConfig holds per-host benchmark idle gate thresholds.
type HostBenchmarkConfig struct {
	CPUThreshold  int `yaml:"cpu_threshold" toml:"cpu_threshold"`
	RAMThreshold  int `yaml:"ram_threshold" toml:"ram_threshold"`
	GPUThreshold  int `yaml:"gpu_threshold" toml:"gpu_threshold"`
	VRAMThreshold int `yaml:"vram_threshold" toml:"vram_threshold"`
	IdleSamples   int `yaml:"idle_samples" toml:"idle_samples"`
	CheckInterval int `yaml:"check_interval" toml:"check_interval"`
}

// HostGPUConfig describes a homogeneous GPU group for a host.
type HostGPUConfig struct {
	Name    string `yaml:"name" toml:"name"`
	Class   string `yaml:"class" toml:"class"`
	Memory  string `yaml:"memory" toml:"memory"`
	Indices []int  `yaml:"indices" toml:"indices"`
}

// PredictorConfig holds configuration for the job-estimator integration.
type PredictorConfig struct {
	// ProjectPath is the path to the job-estimator Python project checkout.
	ProjectPath string `yaml:"project_path" toml:"project_path"`
	// ModelDir overrides the default model directory (~/.cache/weft/models).
	ModelDir string `yaml:"model_dir" toml:"model_dir"`
	// RetrainInterval is how many new completed jobs trigger a retrain (default: 50).
	RetrainInterval int `yaml:"retrain_interval" toml:"retrain_interval"`
	// DBPaths lists additional job database paths for training.
	DBPaths []string `yaml:"db_paths" toml:"db_paths"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		DefaultCommand:      "help",
		SyncInterval:        15,
		SyncActiveInterval:  15,
		SyncIdleInterval:    60,
		LogRefreshInterval:  3,
		HostRefreshInterval: 30,
		EnableMouse:         false,
		LogCacheMaxAge:      7,
		LogCacheMaxSize:     1024 * 1024, // 1MB
		ShowUsageHints:      true,
		WebEnabled:          true,
		WebPort:             8127,
		AI: AIConfig{
			Enabled: nil, // nil means "auto" - enabled if ollama is available
			Model:   "",  // empty means use default model
		},
		AutomapDirs: defaultAutomapDirs(),
	}
}

func defaultAutomapDirs() []string {
	return []string{"~"}
}

// CloudCreateOpts returns provider-appropriate instance creation defaults.
func (c *Config) CloudCreateOpts(provider cloud.Provider) (cloud.CreateOpts, error) {
	sshIdentity := ""
	sshPublicKey := ""
	if c != nil {
		sshIdentity = c.Cloud.SSH.ExpandedIdentityFile()
		sshPublicKey = c.Cloud.SSH.ExpandedPublicKeyFile()
	}
	switch provider {
	case cloud.ProviderRunpod:
		image := cloud.DefaultRunpodImage
		templateID := ""
		if c != nil {
			configured := strings.TrimSpace(c.Runpod.DefaultImage)
			if strings.HasPrefix(strings.ToLower(configured), "runpod/") {
				image = configured
			}
			templateID = strings.TrimSpace(c.Runpod.BootstrapTemplateID)
		}
		return cloud.CreateOpts{
			Image:            image,
			DiskGB:           50,
			SSHEnabled:       true,
			SSHIdentityFile:  sshIdentity,
			SSHPublicKeyFile: sshPublicKey,
			TemplateID:       templateID,
		}, nil
	case cloud.ProviderVastai:
		image := ""
		if c != nil {
			image = c.Vastai.DefaultImage
		}
		opts := cloud.DefaultCreateOpts(image)
		opts.SSHIdentityFile = sshIdentity
		opts.SSHPublicKeyFile = sshPublicKey
		return opts, nil
	default:
		opts := cloud.DefaultCreateOpts("")
		opts.SSHIdentityFile = sshIdentity
		opts.SSHPublicKeyFile = sshPublicKey
		return opts, nil
	}
}

// ProviderEnabledSetting returns the raw tri-state provider setting.
// nil means unset/auto, true means explicitly enabled, false means explicitly
// disabled.
func (c *Config) ProviderEnabledSetting(provider cloud.Provider) (*bool, error) {
	if c == nil {
		return nil, nil
	}
	switch provider {
	case cloud.ProviderVastai:
		return c.Vastai.Enabled, nil
	case cloud.ProviderRunpod:
		return c.Runpod.Enabled, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

// ProviderExplicitlyEnabled reports whether a provider is configured true.
func (c *Config) ProviderExplicitlyEnabled(provider cloud.Provider) bool {
	setting, err := c.ProviderEnabledSetting(provider)
	return err == nil && setting != nil && *setting
}

// ProviderExplicitlyDisabled reports whether a provider is configured false.
func (c *Config) ProviderExplicitlyDisabled(provider cloud.Provider) bool {
	setting, err := c.ProviderEnabledSetting(provider)
	return err == nil && setting != nil && !*setting
}

// AnyProviderExplicitlyConfigured reports whether any provider has an enabled
// setting. Once a user starts managing providers explicitly, unset providers
// are treated as disabled instead of falling back to legacy Vast.ai auto.
func (c *Config) AnyProviderExplicitlyConfigured() bool {
	if c == nil {
		return false
	}
	return c.Vastai.Enabled != nil || c.Runpod.Enabled != nil
}

// SetProviderEnabled updates the raw tri-state provider setting.
func (c *Config) SetProviderEnabled(provider cloud.Provider, enabled *bool) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	switch provider {
	case cloud.ProviderVastai:
		c.Vastai.Enabled = enabled
	case cloud.ProviderRunpod:
		c.Runpod.Enabled = enabled
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}
	return nil
}

// ProviderEnabledForDiscovery implements the provider discovery policy.
// Explicit true enables a provider, explicit false disables it. When no
// providers have any explicit setting, Vast.ai remains a legacy auto provider.
func (c *Config) ProviderEnabledForDiscovery(provider cloud.Provider) bool {
	if c == nil {
		return provider == cloud.ProviderVastai
	}
	if c.AnyProviderExplicitlyConfigured() {
		return c.ProviderExplicitlyEnabled(provider)
	}
	return provider == cloud.ProviderVastai
}

// RegistryAuthForImage returns private registry credentials for image. secret
// optionally names a specific [registry] key; otherwise the image registry host
// is used. Empty result means no matching registry config.
func (c *Config) RegistryAuthForImage(image, secret string) (*cloud.RegistryAuth, error) {
	if c == nil || len(c.Registry) == 0 {
		return nil, nil
	}
	key := strings.TrimSpace(secret)
	if key == "" {
		key = imageRegistryHost(image)
	}
	if key == "" {
		return nil, nil
	}
	reg, ok := c.Registry[key]
	if !ok {
		if secret != "" {
			return nil, fmt.Errorf("registry %q is not configured", key)
		}
		return nil, nil
	}
	password, err := reg.password()
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", key, err)
	}
	username := strings.TrimSpace(reg.Username)
	if username == "" {
		return nil, fmt.Errorf("registry %q: username is required", key)
	}
	name := strings.TrimSpace(reg.RunpodAuthName)
	if name == "" {
		name = "weft-" + key
	}
	return &cloud.RegistryAuth{
		Host:     key,
		Username: username,
		Password: password,
		Name:     name,
	}, nil
}

func (r RegistryConfig) password() (string, error) {
	envName := strings.TrimSpace(r.PasswordEnv)
	cmdText := strings.TrimSpace(r.PasswordCommand)
	switch {
	case envName != "" && cmdText != "":
		return "", fmt.Errorf("configure only one of password_env or password_command")
	case envName != "":
		password := strings.TrimSpace(os.Getenv(envName))
		if password == "" {
			return "", fmt.Errorf("environment variable %s is empty", envName)
		}
		return password, nil
	case cmdText != "":
		out, err := exec.Command("/bin/sh", "-lc", cmdText).Output()
		if err != nil {
			return "", fmt.Errorf("password_command failed: %w", err)
		}
		password := strings.TrimSpace(string(out))
		if password == "" {
			return "", fmt.Errorf("password_command returned empty output")
		}
		return password, nil
	default:
		return "", fmt.Errorf("password_env or password_command is required")
	}
}

func imageRegistryHost(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	first, _, _ := strings.Cut(image, "/")
	if first == "" || (!strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost") {
		return "docker.io"
	}
	return first
}

// IsAIEnabled returns whether AI description generation is enabled.
// Returns true if explicitly enabled, or if not set and ollama would be available.
func (c *Config) IsAIEnabled() bool {
	if c.AI.Enabled != nil {
		return *c.AI.Enabled
	}
	// Default: enabled (will check ollama availability at runtime)
	return true
}

// AIModel returns the configured AI model, or the default if not set.
func (c *Config) AIModel() string {
	if c.AI.Model != "" {
		return c.AI.Model
	}
	return "llama3.2" // Default model
}

// IsOperationLogEnabled returns whether operation logging is enabled.
// Returns true by default unless explicitly disabled.
func (c *Config) IsOperationLogEnabled() bool {
	if c.OperationLogEnabled != nil {
		return *c.OperationLogEnabled
	}
	return true // Default: enabled
}

// GetOperationLogMaxSize returns the max operation log size.
// Returns the default (10MB) if not set.
func (c *Config) GetOperationLogMaxSize() int64 {
	if c.OperationLogMaxSize > 0 {
		return c.OperationLogMaxSize
	}
	return 10 * 1024 * 1024 // Default: 10MB
}

// HostBackend returns the configured backend for a host, or empty if not set.
func (c *Config) HostBackend(host string) string {
	if c == nil || host == "" {
		return ""
	}
	if cfg, ok := c.Hosts[host]; ok {
		return strings.ToLower(strings.TrimSpace(cfg.Backend))
	}
	return ""
}

// HostShared reports whether the host is marked as shared in the global config.
func (c *Config) HostShared(host string) bool {
	if c == nil || host == "" {
		return false
	}
	cfg, ok := c.Hosts[host]
	return ok && cfg.Shared
}

// HostOptInOnly reports whether the host is excluded from auto-placement and
// must be selected explicitly via --host.
func (c *Config) HostOptInOnly(host string) bool {
	if c == nil || host == "" {
		return false
	}
	cfg, ok := c.Hosts[host]
	return ok && cfg.OptInOnly
}

// AutoReplanStuckInventoryDispatchEnabled reports whether the autopilot
// should automatically unplace inventory jobs that have been blocked on
// dispatch beyond the configured threshold. Default false: the intervention
// changes a job's placement (and adds the rental tag), which is rarely what
// the user wants when the inventory host's problem is transient.
func (c *Config) AutoReplanStuckInventoryDispatchEnabled() bool {
	if c == nil {
		return false
	}
	return c.Autopilot.AutoReplanStuckInventoryDispatch
}

var (
	configPath       string
	legacyConfigPath string
)

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	configPath = filepath.Join(home, ".config", "weft", "config.toml")
	legacyConfigPath = filepath.Join(home, ".config", "weft", "config.yaml")
}

// ConfigPath returns the path to the config file
func ConfigPath() string {
	return configPath
}

// Load reads the config file, returning defaults if it doesn't exist.
// TOML is preferred; YAML remains as a legacy fallback.
func Load() (*Config, error) {
	cfg := DefaultConfig()
	defer func() {
		cloud.SetSSHIdentityFile(cfg.Cloud.SSH.ExpandedIdentityFile())
	}()

	path := configPath
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && legacyConfigPath != "" {
			path = legacyConfigPath
			data, err = os.ReadFile(path)
		}
		if err != nil {
			if os.IsNotExist(err) {
				return cfg, nil
			}
			return cfg, err
		}
	}

	UnknownTOMLKeys = nil
	switch filepath.Ext(path) {
	case ".toml":
		// Decode in strict mode so unknown keys are reported. The decoder still
		// populates known fields before returning the error, so we keep cfg and
		// extract the undecoded key list for the caller to warn about.
		dec := toml.NewDecoder(bytes.NewReader(data)).Strict(true)
		if err := dec.Decode(cfg); err != nil {
			if keys := parseUndecodedKeys(err); len(keys) > 0 {
				hostBenchmarkKeys, applyErr := applyHostBenchmarkTOML(data, cfg)
				if applyErr != nil {
					return cfg, applyErr
				}
				UnknownTOMLKeys = filterKnownTOMLKeys(keys, hostBenchmarkKeys)
			} else {
				return cfg, err
			}
		}
	default:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

func applyHostBenchmarkTOML(data []byte, cfg *Config) (map[string]struct{}, error) {
	tree, err := toml.LoadBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse host benchmark config: %w", err)
	}
	hostsTree, ok := tree.Get("hosts").(*toml.Tree)
	if !ok {
		return nil, nil
	}

	known := make(map[string]struct{})
	for _, host := range hostsTree.Keys() {
		hostTree, ok := hostsTree.Get(host).(*toml.Tree)
		if !ok {
			continue
		}
		benchTree, ok := hostTree.Get("benchmark").(*toml.Tree)
		if !ok {
			continue
		}
		if cfg.Hosts == nil {
			cfg.Hosts = make(map[string]HostConfig)
		}
		hostCfg := cfg.Hosts[host]
		assignInt := func(key string, dst *int) {
			value, ok := tomlInt(benchTree.Get(key))
			if !ok {
				return
			}
			*dst = value
			known[fmt.Sprintf("hosts.%s.benchmark.%s", host, key)] = struct{}{}
		}
		assignInt("cpu_threshold", &hostCfg.Benchmark.CPUThreshold)
		assignInt("ram_threshold", &hostCfg.Benchmark.RAMThreshold)
		assignInt("gpu_threshold", &hostCfg.Benchmark.GPUThreshold)
		assignInt("vram_threshold", &hostCfg.Benchmark.VRAMThreshold)
		assignInt("idle_samples", &hostCfg.Benchmark.IdleSamples)
		assignInt("check_interval", &hostCfg.Benchmark.CheckInterval)
		cfg.Hosts[host] = hostCfg
	}
	return known, nil
}

func tomlInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

func filterKnownTOMLKeys(keys []string, known map[string]struct{}) []string {
	if len(known) == 0 {
		return keys
	}
	filtered := keys[:0]
	for _, key := range keys {
		if _, ok := known[key]; !ok {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

// parseUndecodedKeys extracts the key list from a go-toml strict-mode error of
// the form `undecoded keys: ["foo" "bar.baz"]`. Returns nil for unrelated
// errors so the caller can treat them as real failures.
func parseUndecodedKeys(err error) []string {
	if err == nil {
		return nil
	}
	const prefix = "undecoded keys: "
	msg := err.Error()
	i := strings.Index(msg, prefix)
	if i < 0 {
		return nil
	}
	rest := msg[i+len(prefix):]
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, "[")
	rest = strings.TrimSuffix(rest, "]")
	var keys []string
	for _, part := range strings.Fields(rest) {
		k := strings.Trim(part, "\"")
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// ExpandUserPath is the exported form of expandUserPath, used by other
// packages (e.g. internal/ssh for per-host identity files).
func ExpandUserPath(path string) string { return expandUserPath(path) }

func expandUserPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

// AutomapDirs returns the list of local directory prefixes that should be
// auto-mapped to the same path on remote hosts. When -C is not specified and
// CWD is under one of these prefixes, the working directory is automatically
// set to the corresponding remote path. The list is read from
// ~/.config/weft/config.toml via the automap_dirs key, defaulting to ["~"].
func AutomapDirs() []string {
	cfg, err := Load()
	if err != nil {
		return defaultAutomapDirs()
	}
	if len(cfg.AutomapDirs) == 0 {
		return defaultAutomapDirs()
	}
	return append([]string(nil), cfg.AutomapDirs...)
}

// DefaultGracePeriod returns the configured grace period string, or "5m" if not set.
func (c *Config) DefaultGracePeriod() string {
	if c.Campaign.GracePeriod != "" {
		return c.Campaign.GracePeriod
	}
	return "5m"
}

// CampaignReliability returns the configured provider-offer reliability floor.
// Valid values are in [0, 1], where 0 disables reliability filtering.
// Invalid values fall back to the default.
func (c *Config) CampaignReliability() float64 {
	if c == nil {
		return defaultCampaignReliability
	}
	if c.Campaign.Reliability == nil {
		return defaultCampaignReliability
	}
	value := *c.Campaign.Reliability
	if value < 0 || value > 1 {
		return defaultCampaignReliability
	}
	return value
}

const (
	defaultCampaignReliability   = 0.95
	defaultPlacementAltSample    = 0.01
	defaultPlacementAltTopK      = 10
	defaultDonorABSample         = 0.05
	defaultRetryFirstTimeLimit   = 45 * time.Minute
	defaultRetryNextTimeLimit    = 45 * time.Minute
	defaultRetryFirstCostUSD     = 1.00
	defaultRetryNextCostUSD      = 0.25
	defaultAutoRunawayWindow     = 24 * time.Hour
	defaultAutoRunawayChain      = 3
	defaultAutoRunawayOrphans    = 8
	defaultAutoRunawayInfraFails = 5
	defaultAutoRunawaySpendUSD   = 5.00
	defaultAutoObjective         = "cost_first"
	defaultOpportunityCostWeight = 1.0
)

// PlacementAlternativeSampleRate returns the fraction of decisions whose
// ranked candidates should be persisted.
func (c *Config) PlacementAlternativeSampleRate() float64 {
	if c == nil || c.Telemetry.PlacementAlternativeSampleRate <= 0 {
		return defaultPlacementAltSample
	}
	if c.Telemetry.PlacementAlternativeSampleRate > 1 {
		return 1
	}
	return c.Telemetry.PlacementAlternativeSampleRate
}

// PlacementAlternativeTopK returns the maximum number of ranked candidates to
// persist for sampled placement decisions.
func (c *Config) PlacementAlternativeTopK() int {
	if c == nil || c.Telemetry.PlacementAlternativeTopK <= 0 {
		return defaultPlacementAltTopK
	}
	return c.Telemetry.PlacementAlternativeTopK
}

// DonorABEnabled reports whether donor-vs-hub control assignment is active.
func (c *Config) DonorABEnabled() bool {
	if c == nil || c.Telemetry.DonorABEnabled == nil {
		return true
	}
	return *c.Telemetry.DonorABEnabled
}

// DonorABSampleRate returns the fraction of eligible campaigns assigned to
// hub-direct control.
func (c *Config) DonorABSampleRate() float64 {
	if c == nil || c.Telemetry.DonorABSampleRate <= 0 {
		return defaultDonorABSample
	}
	if c.Telemetry.DonorABSampleRate > 1 {
		return 1
	}
	return c.Telemetry.DonorABSampleRate
}

func parseDurationOrDefault(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	if d < 0 {
		return fallback
	}
	return d
}

func costUSDToCents(usd float64, fallback float64) int {
	value := usd
	if value == 0 {
		value = fallback
	}
	if value < 0 {
		value = fallback
	}
	return int(math.Round(value * 100))
}

func rateUSDToCentsPerHour(usd float64) int {
	if usd <= 0 {
		return 0
	}
	return int(math.Round(usd * 100))
}

// RetryFirstTimeLimit returns the configured first-retry time limit, defaulting to 45m.
func (c *Config) RetryFirstTimeLimit() time.Duration {
	if c == nil {
		return defaultRetryFirstTimeLimit
	}
	return parseDurationOrDefault(c.Campaign.RetryFirstTimeLimit, defaultRetryFirstTimeLimit)
}

// RetryNextTimeLimit returns the configured subsequent-retry time limit, defaulting to 45m.
func (c *Config) RetryNextTimeLimit() time.Duration {
	if c == nil {
		return defaultRetryNextTimeLimit
	}
	return parseDurationOrDefault(c.Campaign.RetryNextTimeLimit, defaultRetryNextTimeLimit)
}

// RetryFirstCostLimitCents returns the configured first-retry cost limit in cents, defaulting to $1.00.
func (c *Config) RetryFirstCostLimitCents() int {
	if c == nil {
		return costUSDToCents(defaultRetryFirstCostUSD, defaultRetryFirstCostUSD)
	}
	return costUSDToCents(c.Campaign.RetryFirstCostLimit, defaultRetryFirstCostUSD)
}

// RetryNextCostLimitCents returns the configured subsequent-retry cost limit in cents, defaulting to $0.25.
func (c *Config) RetryNextCostLimitCents() int {
	if c == nil {
		return costUSDToCents(defaultRetryNextCostUSD, defaultRetryNextCostUSD)
	}
	return costUSDToCents(c.Campaign.RetryNextCostLimit, defaultRetryNextCostUSD)
}

// AutoRunawayEnabled reports whether unattended runaway protection is enabled.
func (c *Config) AutoRunawayEnabled() bool {
	if c == nil || c.Campaign.AutoRunawayEnabled == nil {
		return true
	}
	return *c.Campaign.AutoRunawayEnabled
}

// AutoRunawayWindow returns the lookback window for runaway detection.
func (c *Config) AutoRunawayWindow() time.Duration {
	if c == nil {
		return defaultAutoRunawayWindow
	}
	return parseDurationOrDefault(c.Campaign.AutoRunawayWindow, defaultAutoRunawayWindow)
}

// AutoRunawayChainNoProgressLimit returns the no-progress chain threshold.
func (c *Config) AutoRunawayChainNoProgressLimit() int {
	if c == nil || c.Campaign.AutoRunawayChainNoProgressLimit <= 0 {
		return defaultAutoRunawayChain
	}
	return c.Campaign.AutoRunawayChainNoProgressLimit
}

// AutoRunawayOrphanChurnLimit returns the orphan churn threshold.
func (c *Config) AutoRunawayOrphanChurnLimit() int {
	if c == nil || c.Campaign.AutoRunawayOrphanChurnLimit <= 0 {
		return defaultAutoRunawayOrphans
	}
	return c.Campaign.AutoRunawayOrphanChurnLimit
}

// AutoRunawayInfraFailureLimit returns the infra-side launch failure threshold.
func (c *Config) AutoRunawayInfraFailureLimit() int {
	if c == nil || c.Campaign.AutoRunawayInfraFailureLimit <= 0 {
		return defaultAutoRunawayInfraFails
	}
	return c.Campaign.AutoRunawayInfraFailureLimit
}

// AutoRunawaySpendNoProgressLimitCents returns the spend threshold in cents.
func (c *Config) AutoRunawaySpendNoProgressLimitCents() int {
	if c == nil {
		return costUSDToCents(defaultAutoRunawaySpendUSD, defaultAutoRunawaySpendUSD)
	}
	return costUSDToCents(c.Campaign.AutoRunawaySpendNoProgressLimit, defaultAutoRunawaySpendUSD)
}

// AutoRunawaySpendDailyCapCents returns the TUI daily cap for the unattended
// runaway spend breaker.
func (c *Config) AutoRunawaySpendDailyCapCents() int {
	return c.AutoRunawaySpendNoProgressLimitCents()
}

// AutoObjective returns the unattended auto-planner objective profile id.
func (c *Config) AutoObjective() string {
	if c == nil {
		return defaultAutoObjective
	}
	value := strings.ToLower(strings.TrimSpace(c.Campaign.AutoObjective))
	switch value {
	case "cost_first", "balanced", "time_first":
		return value
	default:
		return defaultAutoObjective
	}
}

// CampaignObjectiveWeights returns optional explicit objective weights.
// Any non-positive value means "use profile defaults".
func (c *Config) CampaignObjectiveWeights() (float64, float64) {
	if c == nil {
		return 0, 0
	}
	return c.Campaign.ObjectiveCostWeight, c.Campaign.ObjectiveTimeWeight
}

// CampaignOpportunityCostWeight returns the scarcity-penalty scaling factor.
func (c *Config) CampaignOpportunityCostWeight() float64 {
	if c == nil || c.Campaign.OpportunityCostWeight <= 0 {
		return defaultOpportunityCostWeight
	}
	return c.Campaign.OpportunityCostWeight
}

// AutoRunRateSoftTargetCentsPerHour returns the unattended autopilot launch-rate
// target in cents per hour. A zero value disables launch-rate gating.
func (c *Config) AutoRunRateSoftTargetCentsPerHour() int {
	if c == nil {
		return 0
	}
	return rateUSDToCentsPerHour(c.Campaign.AutoRunRateSoftTarget)
}

// AutoRunRateSoftTargetCents returns the unattended autopilot launch-rate
// target in cents per hour. A zero value disables launch-rate gating.
func (c *Config) AutoRunRateSoftTargetCents() int {
	return c.AutoRunRateSoftTargetCentsPerHour()
}

// SourceExcludeDirs returns the effective global source exclude patterns.
// User-configured excludes extend the built-in app defaults instead of
// replacing them.
func (c *Config) SourceExcludeDirs() []string {
	excludes := append([]string(nil), defaultSourceExcludeDirs...)
	if c == nil {
		return excludes
	}
	for _, pattern := range c.Sync.ExcludeDirs {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		found := false
		for _, existing := range excludes {
			if existing == pattern {
				found = true
				break
			}
		}
		if !found {
			excludes = append(excludes, pattern)
		}
	}
	return excludes
}

const (
	CacheEvictionPolicyLRU        = "lru"
	CacheEvictionPolicyReusePerGB = "reuse_per_gb"
	defaultCacheReuseWindow       = 30 * 24 * time.Hour
)

func (c *Config) CacheEvictionPolicy() string {
	if c == nil {
		return CacheEvictionPolicyLRU
	}
	switch strings.ToLower(strings.TrimSpace(c.Data.CacheEvictionPolicy)) {
	case "", CacheEvictionPolicyLRU:
		return CacheEvictionPolicyLRU
	case CacheEvictionPolicyReusePerGB:
		return CacheEvictionPolicyReusePerGB
	default:
		return CacheEvictionPolicyLRU
	}
}

func (c *Config) CacheReuseWindow() time.Duration {
	if c == nil || strings.TrimSpace(c.Data.CacheReuseWindow) == "" {
		return defaultCacheReuseWindow
	}
	d, err := parseDurationWithDays(c.Data.CacheReuseWindow)
	if err != nil || d <= 0 {
		return defaultCacheReuseWindow
	}
	return d
}

func parseDurationWithDays(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasSuffix(raw, "d") && len(raw) > 1 {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

// ValidateCommand checks if a command contains any blocked patterns.
// Returns an error if a blocked pattern is found, nil otherwise.
func (c *Config) ValidateCommand(command string) error {
	for _, bp := range c.BlockedCommandPatterns {
		if strings.Contains(command, bp.Pattern) {
			return fmt.Errorf("command contains blocked pattern %q: %s", bp.Pattern, bp.Message)
		}
	}
	return nil
}
