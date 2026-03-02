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
	Sync ProjectSyncConfig `yaml:"sync"`
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
