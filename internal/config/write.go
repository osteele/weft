package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/cloud"
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

// SetAutoRunRateSoftTarget updates campaign.auto_run_rate_soft_target (USD/hour)
// in the global TOML config. Values <= 0 disable the target.
func SetAutoRunRateSoftTarget(usdPerHour float64) error {
	return setCampaignUSDField("auto_run_rate_soft_target", usdPerHour)
}

// SetAutoRunawaySpendNoProgressLimit updates the daily-window spend cap
// (USD) used by the runaway breaker. Values <= 0 disable the cap by writing
// 0; the breaker code falls back to the compiled-in default in that case.
func SetAutoRunawaySpendNoProgressLimit(usd float64) error {
	return setCampaignUSDField("auto_runaway_spend_no_progress_limit", usd)
}

// SetProviderEnabledSetting updates a provider's global tri-state enabled
// setting. nil removes the key so the provider returns to auto/default policy.
func SetProviderEnabledSetting(provider cloud.Provider, enabled *bool) error {
	section, err := providerConfigSection(provider)
	if err != nil {
		return err
	}
	return UpdateGlobalTOML(func(tree *toml.Tree) error {
		path := []string{section, "enabled"}
		if enabled == nil {
			if err := tree.DeletePath(path); err != nil {
				if err.Error() == "no such key to delete" {
					return nil
				}
				return fmt.Errorf("delete %s.enabled: %w", section, err)
			}
			return nil
		}
		tree.SetPath(path, *enabled)
		return nil
	})
}

func providerConfigSection(provider cloud.Provider) (string, error) {
	switch provider {
	case cloud.ProviderVastai:
		return "vastai", nil
	case cloud.ProviderRunpod:
		return "runpod", nil
	default:
		return "", fmt.Errorf("unknown provider %q", provider)
	}
}

func setCampaignUSDField(key string, usd float64) error {
	if usd < 0 {
		usd = 0
	}
	normalized := math.Round(usd*100) / 100
	return UpdateGlobalTOML(func(tree *toml.Tree) error {
		tree.SetPath([]string{"campaign", key}, normalized)
		return nil
	})
}
