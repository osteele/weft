package cloudproviders

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

// UnavailableProvider records a provider that was configured or attempted but
// failed its availability check.
type UnavailableProvider struct {
	Provider cloud.Provider
	Err      error
}

// Discovery holds the discovered cloud clients plus any provider availability
// failures encountered while building them.
type Discovery struct {
	Clients     []cloud.Client
	Unavailable []UnavailableProvider
}

type providerCheck struct {
	Provider cloud.Provider
	Attempt  bool
	Load     func() (cloud.Client, error)
}

// UnavailableError formats provider availability failures when no providers are usable.
func (d Discovery) UnavailableError() error {
	if len(d.Clients) > 0 || len(d.Unavailable) == 0 {
		return nil
	}

	parts := make([]string, 0, len(d.Unavailable))
	for _, unavailable := range d.Unavailable {
		parts = append(parts, fmt.Sprintf("%s: %v", unavailable.Provider, unavailable.Err))
	}
	return fmt.Errorf("no cloud providers available: %s", strings.Join(parts, "; "))
}

// Discover creates cloud clients for enabled providers, preserving the existing
// fallback behavior of attempting Vast.ai when no providers are explicitly enabled.
func Discover(cfg *config.Config) Discovery {
	vastaiEnabled := cfg != nil && cfg.Vastai.Enabled
	runpodEnabled := cfg != nil && cfg.Runpod.Enabled

	return discover([]providerCheck{
		{
			Provider: cloud.ProviderVastai,
			Attempt:  vastaiEnabled || !runpodEnabled,
			Load: func() (cloud.Client, error) {
				vc := vastai.NewClient()
				if err := vc.Available(); err != nil {
					return nil, err
				}
				return vastai.NewCloudClient(vc), nil
			},
		},
		{
			Provider: cloud.ProviderRunpod,
			Attempt:  runpodEnabled,
			Load: func() (cloud.Client, error) {
				rc := runpod.NewCloudClient()
				if err := rc.Available(); err != nil {
					return nil, err
				}
				return rc, nil
			},
		},
	})
}

func discover(checks []providerCheck) Discovery {
	result := Discovery{}
	for _, check := range checks {
		if !check.Attempt {
			continue
		}

		client, err := check.Load()
		if err != nil {
			result.Unavailable = append(result.Unavailable, UnavailableProvider{
				Provider: check.Provider,
				Err:      err,
			})
			continue
		}

		result.Clients = append(result.Clients, client)
	}

	return result
}
