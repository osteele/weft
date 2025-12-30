package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds application configuration
type Config struct {
	// DefaultCommand is the command to run when no arguments are provided
	// Valid values: "help", "list", "tui"
	DefaultCommand string `yaml:"default_command"`

	// TUI polling intervals (in seconds)
	// SyncInterval is how often to check if running jobs have completed
	SyncInterval int `yaml:"sync_interval"`
	// LogRefreshInterval is how often to refresh logs for selected running jobs
	LogRefreshInterval int `yaml:"log_refresh_interval"`
	// HostRefreshInterval is how often to refresh host info in hosts view
	HostRefreshInterval int `yaml:"host_refresh_interval"`

	// EnableMouse toggles mouse support in the TUI (disables terminal selection when true)
	EnableMouse bool `yaml:"enable_mouse"`

	// LogCacheMaxAge is how long to keep cached log files (in days)
	// Default: 7 days. Set to 0 to disable caching.
	LogCacheMaxAge int `yaml:"log_cache_max_age"`

	// LogCacheMaxSize is the maximum size of log files to cache (in bytes)
	// Default: 51200 (50KB). Logs larger than this are not cached.
	LogCacheMaxSize int `yaml:"log_cache_max_size"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		DefaultCommand:      "help",
		SyncInterval:        15,
		LogRefreshInterval:  3,
		HostRefreshInterval: 30,
		EnableMouse:         false,
		LogCacheMaxAge:      7,
		LogCacheMaxSize:     50 * 1024, // 50KB
	}
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
