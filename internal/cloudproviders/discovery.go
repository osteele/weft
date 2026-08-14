package cloudproviders

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/oplog"
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

	allUnreachable := true
	providerNames := make([]string, 0, len(d.Unavailable))
	for _, unavailable := range d.Unavailable {
		if !isProviderReachabilityError(unavailable.Err) {
			allUnreachable = false
			break
		}
		providerNames = append(providerNames, providerDisplayName(unavailable.Provider))
	}
	if allUnreachable {
		if len(providerNames) == 1 {
			return fmt.Errorf("%s unreachable (network or provider outage); Weft will retry", providerNames[0])
		}
		return fmt.Errorf("cloud providers unreachable (%s; network or provider outage); Weft will retry", strings.Join(providerNames, ", "))
	}

	parts := make([]string, 0, len(d.Unavailable))
	for _, unavailable := range d.Unavailable {
		if isProviderReachabilityError(unavailable.Err) {
			parts = append(parts, fmt.Sprintf("%s unreachable (network or provider outage)", providerDisplayName(unavailable.Provider)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %v", unavailable.Provider, unavailable.Err))
	}
	return fmt.Errorf("no cloud providers available: %s", strings.Join(parts, "; "))
}

func providerDisplayName(provider cloud.Provider) string {
	if name := provider.DisplayName(); name != "" {
		return name
	}
	return string(provider)
}

func isProviderReachabilityError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"requests.exceptions.connectionerror",
		"connectionpool(",
		"max retries exceeded",
		"dns lookup",
		"failed to resolve",
		"name resolution",
		"network is unreachable",
		"no route to host",
		"connection refused",
		"connection reset",
		"timed out",
		"deadline exceeded",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// Discover creates cloud clients for enabled providers, preserving the existing
// fallback behavior of attempting Vast.ai when no providers are explicitly enabled.
func Discover(cfg *config.Config) Discovery {
	return discover([]providerCheck{
		{
			Provider: cloud.ProviderVastai,
			Attempt:  cfg == nil || cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai),
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
			Attempt:  cfg != nil && cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod),
			Load: func() (cloud.Client, error) {
				cloudType, err := cfg.RunpodCloudType()
				if err != nil {
					return nil, err
				}
				rc := runpod.NewCloudClientWithCloudType(cloudType)
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
	type checkResult struct {
		provider cloud.Provider
		client   cloud.Client
		err      error
		elapsed  time.Duration
	}

	results := make([]checkResult, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		if !check.Attempt {
			continue
		}
		wg.Add(1)
		go func(idx int, p providerCheck) {
			defer wg.Done()
			start := time.Now()
			client, err := p.Load()
			results[idx] = checkResult{
				provider: p.Provider,
				client:   client,
				err:      err,
				elapsed:  time.Since(start),
			}
			if err != nil {
				oplog.Log("cloud.provider_discovery",
					oplog.WithDetailf("provider=%s status=unavailable", p.Provider),
					oplog.WithDuration(results[idx].elapsed),
					oplog.WithError(err))
				return
			}
			oplog.Log("cloud.provider_discovery",
				oplog.WithDetailf("provider=%s status=available", p.Provider),
				oplog.WithDuration(results[idx].elapsed))
		}(i, check)
	}
	wg.Wait()

	for i, check := range checks {
		if !check.Attempt {
			continue
		}
		r := results[i]
		if r.err != nil {
			result.Unavailable = append(result.Unavailable, UnavailableProvider{
				Provider: r.provider,
				Err:      r.err,
			})
			continue
		}
		if r.client != nil {
			result.Clients = append(result.Clients, r.client)
		}
	}

	return result
}
