package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml"
	"gopkg.in/yaml.v3"
)

const (
	// ProjectConfigFile is the preferred filename for per-project weft configuration.
	ProjectConfigFile = ".weft.toml"
	// LegacyProjectConfigFile remains supported for backward compatibility.
	LegacyProjectConfigFile = ".weft.yaml"
)

// ProjectConfig holds per-project configuration loaded from .weft.toml.
type ProjectConfig struct {
	Sync    ProjectSyncConfig    `yaml:"sync" toml:"sync"`
	Outputs ProjectOutputsConfig `yaml:"outputs" toml:"outputs"`
	Cloud   ProjectCloudConfig   `yaml:"cloud" toml:"cloud"`
	Inputs  []string             `yaml:"inputs" toml:"inputs"` // e.g. ["hf:gpt2", "hf:meta-llama/Llama-3.1-8B"]
}

// ProjectCloudConfig holds per-project cloud instance settings.
type ProjectCloudConfig struct {
	// Image overrides the default Docker image for cloud instances.
	Image string `yaml:"image" toml:"image"`
}

// ProjectOutputsConfig holds output collection settings for convention-based output discovery.
type ProjectOutputsConfig struct {
	// Dirs lists relative directory patterns to collect as outputs.
	// Default: ["output/", "outputs/"]
	Dirs []string `yaml:"dirs" toml:"dirs"`
	// MaxAutoSyncMB is the max total size (MB) to auto-sync back on completion.
	// Outputs larger than this are tracked but only synced on demand.
	// Default: 100
	MaxAutoSyncMB int `yaml:"max_auto_sync_mb" toml:"max_auto_sync_mb"`
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
	ExtraPaths []string `yaml:"extra_paths" toml:"extra_paths"`
	// ExcludeDirs lists additional path-component patterns excluded from source
	// sync and campaign tarballs for this project only.
	ExcludeDirs []string `yaml:"exclude_dirs" toml:"exclude_dirs"`
}

// LoadProjectConfig searches for .weft.toml (falling back to .weft.yaml)
// starting from dir and walking up to the filesystem root. Returns nil (no
// error) if no config file is found.
func LoadProjectConfig(dir string) (*ProjectConfig, error) {
	if dir == "" {
		return nil, nil
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve directory %s: %w", dir, err)
	}

	for {
		for _, name := range []string{ProjectConfigFile, LegacyProjectConfigFile} {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err == nil {
				var cfg ProjectConfig
				switch filepath.Ext(path) {
				case ".toml":
					if err := toml.Unmarshal(data, &cfg); err != nil {
						return nil, err
					}
				default:
					if err := yaml.Unmarshal(data, &cfg); err != nil {
						return nil, err
					}
				}
				return &cfg, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
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
// localDir, or nil if no config is found.
func ProjectExtraPaths(localDir string) []string {
	if cfg := loadProjectConfigOrWarn(localDir); cfg != nil {
		return cfg.Sync.ExtraPaths
	}
	return nil
}

// ProjectExcludeDirs returns additional source exclude patterns from the
// project config at localDir, or nil if no config is found.
func ProjectExcludeDirs(localDir string) []string {
	if cfg := loadProjectConfigOrWarn(localDir); cfg != nil {
		return cfg.Sync.ExcludeDirs
	}
	return nil
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
// localDir, or nil if no config is found.
func ProjectInputs(localDir string) []string {
	if cfg := loadProjectConfigOrWarn(localDir); cfg != nil {
		return cfg.Inputs
	}
	return nil
}

// ProjectCloudImage returns the cloud image override from the project config at
// localDir, or empty string if no config or image is found.
func ProjectCloudImage(localDir string) string {
	if cfg := loadProjectConfigOrWarn(localDir); cfg != nil {
		return cfg.Cloud.Image
	}
	return ""
}

// loadProjectConfigOrWarn loads the project config, logging a warning on error.
// Returns nil when localDir is empty, no config file exists, or the config fails to parse.
func loadProjectConfigOrWarn(localDir string) *ProjectConfig {
	if localDir == "" {
		return nil
	}
	cfg, err := LoadProjectConfig(localDir)
	if err != nil {
		log.Printf("warning: loading project config from %s: %v", localDir, err)
		return nil
	}
	return cfg
}

// projectOutputsConfig loads the outputs section from the project config,
// returning a zero value if no config is found.
func projectOutputsConfig(localDir string) *ProjectOutputsConfig {
	if cfg := loadProjectConfigOrWarn(localDir); cfg != nil {
		return &cfg.Outputs
	}
	return &ProjectOutputsConfig{}
}
