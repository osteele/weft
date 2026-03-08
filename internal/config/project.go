package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectConfigFile is the filename for per-project weft configuration.
const ProjectConfigFile = ".weft.yaml"

// ProjectConfig holds per-project configuration loaded from .weft.yaml.
type ProjectConfig struct {
	Sync    ProjectSyncConfig    `yaml:"sync"`
	Outputs ProjectOutputsConfig `yaml:"outputs"`
	Inputs  []string             `yaml:"inputs"` // e.g. ["hf:gpt2", "hf:meta-llama/Llama-3.1-8B"]
}

// ProjectOutputsConfig holds output collection settings for convention-based output discovery.
type ProjectOutputsConfig struct {
	// Dirs lists relative directory patterns to collect as outputs.
	// Default: ["output/", "outputs/"]
	Dirs []string `yaml:"dirs"`
	// MaxAutoSyncMB is the max total size (MB) to auto-sync back on completion.
	// Outputs larger than this are tracked but only synced on demand.
	// Default: 100
	MaxAutoSyncMB int `yaml:"max_auto_sync_mb"`
}

// DefaultOutputDirs is the default list of output directories to scan.
var DefaultOutputDirs = []string{"output/", "outputs/"}

// DefaultMaxAutoSyncMB is the default max total size (MB) to auto-sync back on completion.
const DefaultMaxAutoSyncMB = 100

// EffectiveDirs returns the configured output dirs or defaults.
func (c *ProjectOutputsConfig) EffectiveDirs() []string {
	if len(c.Dirs) > 0 {
		return c.Dirs
	}
	return DefaultOutputDirs
}

// EffectiveMaxAutoSyncMB returns the configured max auto-sync size or the default.
func (c *ProjectOutputsConfig) EffectiveMaxAutoSyncMB() int {
	if c.MaxAutoSyncMB > 0 {
		return c.MaxAutoSyncMB
	}
	return DefaultMaxAutoSyncMB
}

// ProjectSyncConfig holds sync-related per-project settings.
type ProjectSyncConfig struct {
	// ExtraPaths lists additional local paths to sync to remote hosts before
	// job execution. These are typically out-of-tree data directories.
	ExtraPaths []string `yaml:"extra_paths"`
}

// LoadProjectConfig searches for .weft.yaml starting from dir and walking up
// to the filesystem root. Returns nil (no error) if no config file is found.
func LoadProjectConfig(dir string) (*ProjectConfig, error) {
	if dir == "" {
		return nil, nil
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve directory %s: %w", dir, err)
	}

	for {
		path := filepath.Join(dir, ProjectConfigFile)
		data, err := os.ReadFile(path)
		if err == nil {
			var cfg ProjectConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, err
			}
			return &cfg, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return nil, nil
}

// ProjectExtraPaths returns the extra sync paths from the project config at
// localDir, or nil if no config is found. Errors are silently ignored (the
// project config is optional).
func ProjectExtraPaths(localDir string) []string {
	if localDir == "" {
		return nil
	}
	projCfg, err := LoadProjectConfig(localDir)
	if err != nil || projCfg == nil {
		return nil
	}
	return projCfg.Sync.ExtraPaths
}

// ProjectOutputDirs returns the effective output directories from the project
// config at localDir. Returns the defaults if no config is found.
func ProjectOutputDirs(localDir string) []string {
	return projectOutputsConfig(localDir).EffectiveDirs()
}

// ProjectMaxAutoSyncMB returns the max auto-sync size from the project config
// at localDir. Returns the default if no config is found.
func ProjectMaxAutoSyncMB(localDir string) int {
	return projectOutputsConfig(localDir).EffectiveMaxAutoSyncMB()
}

// ProjectInputs returns the input data assets from the project config at
// localDir, or nil if no config is found. Errors are silently ignored.
func ProjectInputs(localDir string) []string {
	if localDir == "" {
		return nil
	}
	projCfg, err := LoadProjectConfig(localDir)
	if err != nil || projCfg == nil {
		return nil
	}
	return projCfg.Inputs
}

// projectOutputsConfig loads the outputs section from the project config,
// returning a zero value if no config is found.
func projectOutputsConfig(localDir string) *ProjectOutputsConfig {
	if localDir != "" {
		if projCfg, err := LoadProjectConfig(localDir); err == nil && projCfg != nil {
			return &projCfg.Outputs
		}
	}
	return &ProjectOutputsConfig{}
}
