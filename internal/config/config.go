package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	toml "github.com/pelletier/go-toml"
	"gopkg.in/yaml.v3"
)

// Config holds application configuration
type Config struct {
	// DefaultCommand is the command to run when no arguments are provided
	// Valid values: "help", "watch", "list", "tui", "web"
	DefaultCommand string `yaml:"default_command" toml:"default_command"`

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

	// AI/LLM configuration for automatic job description generation
	AI AIConfig `yaml:"ai" toml:"ai"`

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

	// Hosts holds per-host configuration overrides.
	Hosts map[string]HostConfig `yaml:"hosts" toml:"hosts"`

	// Predictor holds job-estimator configuration for duration/resource estimation
	Predictor PredictorConfig `yaml:"predictor" toml:"predictor"`

	// Vastai holds Vast.ai cloud GPU and R2 result storage configuration
	Vastai VastaiConfig `yaml:"vastai" toml:"vastai"`

	// Runpod holds Runpod cloud GPU configuration
	Runpod RunpodConfig `yaml:"runpod" toml:"runpod"`

	// CoordinatorHost is the host where the coordinator daemon runs.
	CoordinatorHost string `yaml:"coordinator_host" toml:"coordinator_host"`

	// Campaign holds cloud campaign defaults
	Campaign CampaignConfig `yaml:"campaign" toml:"campaign"`

	// Sync holds source-sync defaults.
	Sync SyncConfig `yaml:"sync" toml:"sync"`

	// Remediation holds auto-remediation configuration for failed jobs
	Remediation RemediationConfig `yaml:"remediation" toml:"remediation"`

	// AutomapDirs lists local prefixes that should reuse the same relative path
	// on remote hosts; defaults to ["~"].
	AutomapDirs []string `yaml:"automap_dirs" toml:"automap_dirs"`
}

// SyncConfig holds source sync and packaging defaults.
type SyncConfig struct {
	// ExcludeDirs are additional path-component patterns excluded from source sync
	// and campaign tarballs for all projects.
	ExcludeDirs []string `yaml:"exclude_dirs" toml:"exclude_dirs"`
}

var defaultSourceExcludeDirs = []string{"runs", "wand", "wandb"}

// RemediationConfig holds configuration for automatic job failure remediation.
type RemediationConfig struct {
	// CodingAgent is the command to invoke for code fixes (e.g., "claude -p").
	// Empty means no coding agent is used (opt-out by default).
	CodingAgent string `yaml:"coding_agent" toml:"coding_agent"`
	// CodingAgentDir is the working directory for the agent (defaults to job's working dir).
	CodingAgentDir string `yaml:"coding_agent_dir" toml:"coding_agent_dir"`
}

// CampaignConfig holds cloud campaign defaults.
type CampaignConfig struct {
	// GracePeriod is the default grace period after job failure (e.g., "5m", "15m").
	// Default: "5m"
	GracePeriod string `yaml:"grace_period" toml:"grace_period"`
}

// VastaiConfig holds Vast.ai cloud GPU settings.
type VastaiConfig struct {
	// Enabled controls whether Vast.ai cloud GPU options are available
	Enabled bool `yaml:"enabled" toml:"enabled"`
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
	// Enabled controls whether Runpod cloud GPU options are available
	Enabled bool `yaml:"enabled" toml:"enabled"`
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

// BlockedPattern defines a substring that should not appear in job commands
type BlockedPattern struct {
	// Pattern is the substring to search for in commands
	Pattern string `yaml:"pattern" toml:"pattern"`
	// Message is the error message to display when the pattern is found
	Message string `yaml:"message" toml:"message"`
}

// AIConfig holds configuration for AI/LLM features
type AIConfig struct {
	// Enabled controls whether AI description generation is active
	// Default: true (if ollama is available)
	Enabled *bool `yaml:"enabled" toml:"enabled"`

	// Model specifies the ollama model to use for description generation
	// Default: "llama3.2"
	Model string `yaml:"model" toml:"model"`
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

// HostConfig holds per-host configuration.
type HostConfig struct {
	// Backend sets the execution backend for this host ("queue-runner" or "slurm").
	Backend string `yaml:"backend" toml:"backend"`
	// Shared marks an inventory host as multi-tenant, so benchmark auto-placement
	// avoids it unless the job is explicitly inventory-tagged.
	Shared bool `yaml:"shared" toml:"shared"`
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
	switch provider {
	case cloud.ProviderRunpod:
		if c == nil || c.Runpod.BootstrapTemplateID == "" {
			return cloud.CreateOpts{}, fmt.Errorf("runpod campaigns require runpod.bootstrap_template_id; run `weft runpod setup` or `weft runpod template print-bootstrap`")
		}
		return cloud.CreateOpts{
			DiskGB:     50,
			SSHEnabled: true,
			TemplateID: c.Runpod.BootstrapTemplateID,
		}, nil
	case cloud.ProviderVastai:
		if c == nil {
			return cloud.DefaultCreateOpts(""), nil
		}
		return cloud.DefaultCreateOpts(c.Vastai.DefaultImage), nil
	default:
		return cloud.DefaultCreateOpts(""), nil
	}
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

	switch filepath.Ext(path) {
	case ".toml":
		if err := toml.Unmarshal(data, cfg); err != nil {
			return cfg, err
		}
	default:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
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

// GetCoordinatorHost returns the configured coordinator host.
// Returns "" if not configured.
func (c *Config) GetCoordinatorHost() string {
	return c.CoordinatorHost
}

// DefaultGracePeriod returns the configured grace period string, or "5m" if not set.
func (c *Config) DefaultGracePeriod() string {
	if c.Campaign.GracePeriod != "" {
		return c.Campaign.GracePeriod
	}
	return "5m"
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
