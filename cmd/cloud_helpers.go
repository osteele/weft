package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

// buildCloudClients creates cloud.Client instances for all enabled providers.
func buildCloudClients(cfg *config.Config) ([]cloud.Client, error) {
	return orchestration.BuildCloudClients(cfg)
}

// cloudClientForProvider finds the client matching a provider from a list.
// If provider is empty, returns the first client as a default. Otherwise
// returns the exact match or nil — never fall back to a different provider,
// since routing a runpod instance to the vastai client (or vice versa)
// corrupts instance lookups and destroys.
func cloudClientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
	if provider == "" && len(clients) > 0 {
		return clients[0]
	}
	for _, c := range clients {
		if c.Provider() == provider {
			return c
		}
	}
	return nil
}

// buildR2Client creates an R2 client from config. Returns nil if R2 is not configured.
func buildR2Client(cfg *config.Config) (*r2.Client, error) {
	return orchestration.BuildR2Client(cfg)
}

// cloudClientForDBInstance creates a cloud.Client for the provider stored in a DB record.
func cloudClientForDBInstance(provider string) cloud.Client {
	switch cloud.Provider(provider) {
	case cloud.ProviderVastai:
		return vastai.NewCloudClient(vastai.NewClient())
	case cloud.ProviderRunpod:
		return runpod.NewCloudClient()
	default:
		// Default to vastai for legacy records
		return vastai.NewCloudClient(vastai.NewClient())
	}
}

func createOptsForProvider(cfg *config.Config, provider cloud.Provider) (cloud.CreateOpts, error) {
	if cfg == nil {
		return cloud.CreateOpts{}, fmt.Errorf("cloud config is required")
	}
	return cfg.CloudCreateOpts(provider)
}
