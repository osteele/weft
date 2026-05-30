package orchestration

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
)

func TestCheckProviderCreditHealthSkipsDisabledProviders(t *testing.T) {
	ResetProviderCreditGateCacheForTests()
	t.Cleanup(ResetProviderCreditGateCacheForTests)

	// No providers enabled → nothing to check.
	cfg := &config.Config{}
	if got := CheckProviderCreditHealth(cfg); len(got) != 0 {
		t.Fatalf("CheckProviderCreditHealth = %v, want empty for disabled providers", got)
	}
}

func TestCheckProviderCreditHealthExhaustedVastai(t *testing.T) {
	ResetProviderCreditGateCacheForTests()
	t.Cleanup(ResetProviderCreditGateCacheForTests)

	prev := providerCreditGateProbeVastai
	providerCreditGateProbeVastai = func() (float64, error) { return 0, nil }
	t.Cleanup(func() { providerCreditGateProbeVastai = prev })

	cfg := &config.Config{}
	cfg.Vastai.Enabled = true

	got := CheckProviderCreditHealth(cfg)
	if len(got) != 1 {
		t.Fatalf("len(statuses) = %d, want 1", len(got))
	}
	if !got[0].Exhausted {
		t.Errorf("Vastai status = %+v, want Exhausted=true", got[0])
	}
	if got[0].Provider != cloud.ProviderVastai {
		t.Errorf("Provider = %s, want %s", got[0].Provider, cloud.ProviderVastai)
	}
}

func TestCheckProviderCreditHealthProbeFailureIsNotExhausted(t *testing.T) {
	ResetProviderCreditGateCacheForTests()
	t.Cleanup(ResetProviderCreditGateCacheForTests)

	prev := providerCreditGateProbeVastai
	providerCreditGateProbeVastai = func() (float64, error) {
		return 0, errors.New("show user: network unreachable")
	}
	t.Cleanup(func() { providerCreditGateProbeVastai = prev })

	cfg := &config.Config{}
	cfg.Vastai.Enabled = true

	got := CheckProviderCreditHealth(cfg)
	if len(got) != 1 {
		t.Fatalf("len(statuses) = %d, want 1", len(got))
	}
	if got[0].Exhausted {
		t.Errorf("status = %+v, want Exhausted=false (probe failed, unknown balance)", got[0])
	}
	if got[0].ProbeError == nil {
		t.Errorf("ProbeError = nil, want set so caller can distinguish failed probe from healthy")
	}
}

func TestCheckProviderCreditHealthCachesProbe(t *testing.T) {
	ResetProviderCreditGateCacheForTests()
	t.Cleanup(ResetProviderCreditGateCacheForTests)

	calls := 0
	prev := providerCreditGateProbeVastai
	providerCreditGateProbeVastai = func() (float64, error) {
		calls++
		return 5.0, nil
	}
	t.Cleanup(func() { providerCreditGateProbeVastai = prev })

	cfg := &config.Config{}
	cfg.Vastai.Enabled = true

	_ = CheckProviderCreditHealth(cfg)
	_ = CheckProviderCreditHealth(cfg)
	_ = CheckProviderCreditHealth(cfg)
	if calls != 1 {
		t.Errorf("probe called %d times, want 1 (subsequent calls should hit the cache)", calls)
	}
}

func TestCheckProviderCreditHealthCacheExpiresAfterTTL(t *testing.T) {
	ResetProviderCreditGateCacheForTests()
	t.Cleanup(ResetProviderCreditGateCacheForTests)

	now := time.Unix(1_700_000_000, 0)
	prevNow := providerCreditGateProbeNow
	providerCreditGateProbeNow = func() time.Time { return now }
	t.Cleanup(func() { providerCreditGateProbeNow = prevNow })

	calls := 0
	prev := providerCreditGateProbeVastai
	providerCreditGateProbeVastai = func() (float64, error) {
		calls++
		return 5.0, nil
	}
	t.Cleanup(func() { providerCreditGateProbeVastai = prev })

	cfg := &config.Config{}
	cfg.Vastai.Enabled = true

	_ = CheckProviderCreditHealth(cfg)
	now = now.Add(providerCreditGateTTL + time.Second)
	_ = CheckProviderCreditHealth(cfg)
	if calls != 2 {
		t.Errorf("probe called %d times, want 2 (cache should have expired)", calls)
	}
}

func TestAllEnabledProvidersExhausted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []ProviderCreditStatus
		want bool
	}{
		{"empty", nil, false},
		{
			"all exhausted",
			[]ProviderCreditStatus{
				{Provider: cloud.ProviderVastai, Exhausted: true},
				{Provider: cloud.ProviderRunpod, Exhausted: true},
			},
			true,
		},
		{
			"one healthy",
			[]ProviderCreditStatus{
				{Provider: cloud.ProviderVastai, Exhausted: true},
				{Provider: cloud.ProviderRunpod, Exhausted: false, Balance: 12.50},
			},
			false,
		},
		{
			"probe failure treated as unknown",
			[]ProviderCreditStatus{
				{Provider: cloud.ProviderVastai, Exhausted: false, ProbeError: errors.New("net")},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllEnabledProvidersExhausted(tc.in); got != tc.want {
				t.Errorf("AllEnabledProvidersExhausted(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatCreditExhaustionReasonIncludesBalance(t *testing.T) {
	t.Parallel()
	got := FormatCreditExhaustionReason([]ProviderCreditStatus{
		{Provider: cloud.ProviderVastai, Exhausted: true, Balance: 0.0},
	})
	if !strings.Contains(got, "credit exhausted") || !strings.Contains(got, "$0.00") {
		t.Errorf("FormatCreditExhaustionReason = %q, want credit-exhausted with balance", got)
	}
	if !strings.Contains(got, "top up") {
		t.Errorf("FormatCreditExhaustionReason = %q, want top-up hint", got)
	}
}

func TestFormatCreditExhaustionReasonSkipsHealthyProviders(t *testing.T) {
	t.Parallel()
	got := FormatCreditExhaustionReason([]ProviderCreditStatus{
		{Provider: cloud.ProviderVastai, Exhausted: false, Balance: 25.0},
	})
	if got != "" {
		t.Errorf("FormatCreditExhaustionReason = %q, want empty when no provider is exhausted", got)
	}
}
