package cloudproviders

import (
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestDiscoveryUnavailableError_SingleProvider(t *testing.T) {
	result := discover([]providerCheck{
		{
			Provider: cloud.ProviderVastai,
			Attempt:  true,
			Load: func() (cloud.Client, error) {
				return nil, fmt.Errorf("DNS lookup failed")
			},
		},
	})

	err := result.UnavailableError()
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	if got := err.Error(); got != "no cloud providers available: vastai: DNS lookup failed" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestDiscoveryUnavailableError_MultipleProvidersStableOrder(t *testing.T) {
	result := discover([]providerCheck{
		{
			Provider: cloud.ProviderVastai,
			Attempt:  true,
			Load: func() (cloud.Client, error) {
				return nil, fmt.Errorf("auth failed")
			},
		},
		{
			Provider: cloud.ProviderRunpod,
			Attempt:  true,
			Load: func() (cloud.Client, error) {
				return nil, fmt.Errorf("HTTP 503")
			},
		},
	})

	err := result.UnavailableError()
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	got := err.Error()
	want := "no cloud providers available: vastai: auth failed; runpod: HTTP 503"
	if got != want {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestDiscoveryUnavailableError_IgnoresFailuresWhenClientAvailable(t *testing.T) {
	result := discover([]providerCheck{
		{
			Provider: cloud.ProviderVastai,
			Attempt:  true,
			Load: func() (cloud.Client, error) {
				return nil, fmt.Errorf("auth failed")
			},
		},
		{
			Provider: cloud.ProviderRunpod,
			Attempt:  true,
			Load: func() (cloud.Client, error) {
				return &cloud.MockClient{ProviderVal: cloud.ProviderRunpod}, nil
			},
		},
	})

	if err := result.UnavailableError(); err != nil {
		t.Fatalf("unexpected unavailable error: %v", err)
	}
	if len(result.Clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(result.Clients))
	}
	if len(result.Unavailable) != 1 {
		t.Fatalf("expected 1 unavailable provider, got %d", len(result.Unavailable))
	}
	if provider := result.Clients[0].Provider(); provider != cloud.ProviderRunpod {
		t.Fatalf("expected runpod client, got %s", provider)
	}
	if !strings.Contains(result.Unavailable[0].Err.Error(), "auth failed") {
		t.Fatalf("unexpected unavailable detail: %v", result.Unavailable[0].Err)
	}
}
