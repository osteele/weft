package config

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestProviderEnabledForDiscoveryLegacyDefault(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai) {
		t.Fatal("Vast.ai should be active when no providers are explicitly configured")
	}
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod) {
		t.Fatal("RunPod should not be active by default")
	}
}

func TestProviderEnabledForDiscoveryExplicitRunpodDisablesVastaiFallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.Enabled = Bool(true)

	if cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai) {
		t.Fatal("Vast.ai fallback should be disabled once providers are explicit")
	}
	if !cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod) {
		t.Fatal("RunPod should be active when explicitly enabled")
	}
}

func TestProviderEnabledForDiscoveryExplicitDisableOnlyDisablesAll(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Vastai.Enabled = Bool(false)

	if cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai) {
		t.Fatal("Vast.ai should be disabled when explicitly false")
	}
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod) {
		t.Fatal("RunPod should remain inactive when unset and providers are explicit")
	}
}
