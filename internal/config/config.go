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
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		DefaultCommand: "help",
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
