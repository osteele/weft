package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml"
)

// UpdateGlobalTOML updates the preferred global TOML config in place.
// If only the legacy YAML config exists, it is loaded and rewritten as TOML.
func UpdateGlobalTOML(update func(tree *toml.Tree) error) error {
	if configPath == "" {
		return fmt.Errorf("config path is not initialized")
	}

	path := configPath
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	tree, err := loadOrCreateTOMLTree()
	if err != nil {
		return err
	}
	if err := update(tree); err != nil {
		return err
	}

	data, err := tree.ToTomlString()
	if err != nil {
		return fmt.Errorf("encode config TOML: %w", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		return fmt.Errorf("write config TOML: %w", err)
	}
	return nil
}

func loadOrCreateTOMLTree() (*toml.Tree, error) {
	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			tree, err := toml.LoadBytes(data)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			return tree, nil
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", configPath, err)
		}
	}

	if legacyConfigPath == "" {
		return toml.TreeFromMap(map[string]interface{}{})
	}
	if _, err := os.Stat(legacyConfigPath); errors.Is(err, os.ErrNotExist) {
		return toml.TreeFromMap(map[string]interface{}{})
	} else if err != nil {
		return nil, fmt.Errorf("read %s: %w", legacyConfigPath, err)
	}

	cfg, err := Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config as TOML: %w", err)
	}
	tree, err := toml.LoadBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse generated TOML: %w", err)
	}
	return tree, nil
}

// SetConfigPathsForTesting overrides the global config paths until the returned
// restore function is called.
func SetConfigPathsForTesting(tomlPath, yamlPath string) func() {
	origPath := configPath
	origLegacy := legacyConfigPath
	configPath = tomlPath
	legacyConfigPath = yamlPath
	return func() {
		configPath = origPath
		legacyConfigPath = origLegacy
	}
}
