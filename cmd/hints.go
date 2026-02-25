package cmd

import (
	"sync"

	"github.com/osteele/weft/internal/config"
)

var (
	hintsMu         sync.Mutex
	hintsConfigured bool
	hintsEnabled    = true
)

// usageHintsEnabled returns whether optional CLI hints should be shown.
func usageHintsEnabled() bool {
	hintsMu.Lock()
	defer hintsMu.Unlock()
	if hintsConfigured {
		return hintsEnabled
	}
	cfg, err := config.Load()
	if err == nil && cfg != nil {
		hintsEnabled = cfg.ShowUsageHints
	}
	hintsConfigured = true
	return hintsEnabled
}

// setUsageHintsFromConfig updates the cached hint preference using a known config.
func setUsageHintsFromConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	hintsMu.Lock()
	hintsEnabled = cfg.ShowUsageHints
	hintsConfigured = true
	hintsMu.Unlock()
}
