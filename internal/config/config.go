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
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		DefaultCommand:      "help",
		SyncInterval:        15,
		LogRefreshInterval:  3,
		HostRefreshInterval: 30,
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
