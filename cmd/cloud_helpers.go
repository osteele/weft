package cmd

import (
	"fmt"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

// buildCloudClients creates cloud.Client instances for all enabled providers.
// Falls back to creating a vastai client if vastai is enabled (or no providers specified
// but vastai CLI is available).
func buildCloudClients(cfg *config.Config) []cloud.Client {
	var clients []cloud.Client

	if cfg.Vastai.Enabled {
		vc := vastai.NewClient()
		if err := vc.Available(); err == nil {
			clients = append(clients, vastai.NewCloudClient(vc))
		}
	}

	if cfg.Runpod.Enabled {
		rc := runpod.NewCloudClient()
		if err := rc.Available(); err == nil {
			clients = append(clients, rc)
		}
	}

	// Fallback: if no providers explicitly enabled, try vastai
	if len(clients) == 0 && !cfg.Vastai.Enabled && !cfg.Runpod.Enabled {
		vc := vastai.NewClient()
		if err := vc.Available(); err == nil {
			clients = append(clients, vastai.NewCloudClient(vc))
		}
	}

	return clients
}

// cloudClientForProvider finds the client matching a provider from a list.
func cloudClientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
	for _, c := range clients {
		if c.Provider() == provider {
			return c
		}
	}
	// If only one client, use it regardless of provider
	if len(clients) == 1 {
		return clients[0]
	}
	return nil
}

// buildR2Client creates an R2 client from config. Returns nil if R2 is not configured.
func buildR2Client(cfg *config.Config) (*r2.Client, error) {
	r2Cfg := cfg.Vastai.R2
	if r2Cfg.Bucket == "" || r2Cfg.AccessKeyID == "" {
		return nil, nil
	}
	return r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
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
