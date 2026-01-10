package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds application configuration
type Config struct {
	// DefaultCommand is the command to run when no arguments are provided
	// Valid values: "help", "list", "tui"
	DefaultCommand string `yaml:"default_command"`

	// TUI polling intervals (in seconds)
	// SyncInterval is the legacy TUI sync interval (seconds).
	// Deprecated in favor of SyncActiveInterval/SyncIdleInterval.
	SyncInterval int `yaml:"sync_interval"`
	// SyncActiveInterval is how often to sync hosts with running/queued jobs (seconds).
	SyncActiveInterval int `yaml:"sync_active_interval"`
	// SyncIdleInterval is how often to sync idle hosts (seconds).
	SyncIdleInterval int `yaml:"sync_idle_interval"`
	// LogRefreshInterval is how often to refresh logs for selected running jobs
	LogRefreshInterval int `yaml:"log_refresh_interval"`
	// HostRefreshInterval is how often to refresh host info in hosts view
	HostRefreshInterval int `yaml:"host_refresh_interval"`
	// StopQueueRunnerWhenIdle controls whether the TUI stops the queue runner
	// when a host has no queued jobs.
	StopQueueRunnerWhenIdle bool `yaml:"stop_queue_runner_when_idle"`

	// EnableMouse toggles mouse support in the TUI (disables terminal selection when true)
	EnableMouse bool `yaml:"enable_mouse"`

	// LogCacheMaxAge is how long to keep cached log files (in days)
	// Default: 7 days. Set to 0 to disable caching.
	LogCacheMaxAge int `yaml:"log_cache_max_age"`

	// LogCacheMaxSize is the maximum size of log files to cache (in bytes)
	// Default: 51200 (50KB). Logs larger than this are not cached.
	LogCacheMaxSize int `yaml:"log_cache_max_size"`

	// ShowUsageHints toggles whether CLI commands print follow-up suggestions
	ShowUsageHints bool `yaml:"show_usage_hints"`

	// AI/LLM configuration for automatic job description generation
	AI AIConfig `yaml:"ai"`

	// BlockedCommandPatterns lists substrings that should cause an error if found in job commands.
	// Each entry has a pattern (substring to match) and an error message to display.
	BlockedCommandPatterns []BlockedPattern `yaml:"blocked_command_patterns"`

	// OperationLogEnabled controls whether operation logging is enabled
	// Default: true
	OperationLogEnabled *bool `yaml:"operation_log_enabled"`

	// OperationLogMaxSize is the maximum size of the operation log file in bytes
	// When exceeded, the file is rotated (old content moved to .1 backup)
	// Default: 10MB (10485760)
	OperationLogMaxSize int64 `yaml:"operation_log_max_size"`
}

// BlockedPattern defines a substring that should not appear in job commands
type BlockedPattern struct {
	// Pattern is the substring to search for in commands
	Pattern string `yaml:"pattern"`
	// Message is the error message to display when the pattern is found
	Message string `yaml:"message"`
}

// AIConfig holds configuration for AI/LLM features
type AIConfig struct {
	// Enabled controls whether AI description generation is active
	// Default: true (if ollama is available)
	Enabled *bool `yaml:"enabled"`

	// Model specifies the ollama model to use for description generation
	// Default: "llama3.2"
	Model string `yaml:"model"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		DefaultCommand:          "help",
		SyncInterval:            15,
		SyncActiveInterval:      15,
		SyncIdleInterval:        60,
		LogRefreshInterval:      3,
		HostRefreshInterval:     30,
		StopQueueRunnerWhenIdle: false,
		EnableMouse:             false,
		LogCacheMaxAge:          7,
		LogCacheMaxSize:         50 * 1024, // 50KB
		ShowUsageHints:          true,
		AI: AIConfig{
			Enabled: nil, // nil means "auto" - enabled if ollama is available
			Model:   "",  // empty means use default model
		},
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

var configPath string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	configPath = filepath.Join(home, ".config", "remote-jobs", "config.yaml")
}

// ConfigPath returns the path to the config file
func ConfigPath() string {
	return configPath
}

// Load reads the config file, returning defaults if it doesn't exist
func Load() (*Config, error) {
	cfg := DefaultConfig()

	if configPath == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
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
