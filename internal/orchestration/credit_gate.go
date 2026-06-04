package orchestration

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

// CreditExhaustionThreshold is the dollar balance at or below which a provider
// is considered exhausted for placement purposes. Vast.ai's create endpoint
// returns an empty body when the account is out of credit, so we want to short
// out *before* it gets a chance to mint cryptic ErrProviderRejected rows that
// pollute the failure table and the survival model.
//
// We treat anything <= $0 as exhausted. We deliberately do NOT use the wider
// "credits low" UI threshold ($10) here — that warning catches a different
// problem (runway shrinking faster than expected) and applying it as a hard
// gate would block useful launches when the operator still has spendable
// credit.
const CreditExhaustionThreshold = 0.0

// providerCreditGateTTL caches credit-balance probes between autopilot passes.
// Matches the UI's providerCreditWarningTTL (30s, in
// internal/ui/terminal/provider_credit_warning.go) so the autopilot's gate
// decision and the operator's banner cannot disagree about whether credit is
// available — both layers see the same balance for the same caching window.
const providerCreditGateTTL = 30 * time.Second

// providerCreditGateProbeNow is overridable in tests.
var providerCreditGateProbeNow = time.Now

// providerCreditGateProbeVastai is overridable in tests; in production it
// reads the credit balance from Vast.ai's ShowUser endpoint.
var providerCreditGateProbeVastai = func() (float64, error) {
	user, err := vastai.NewClient().ShowUser()
	if err != nil || user == nil {
		return 0, err
	}
	return user.Credit, nil
}

// providerCreditGateProbeRunpod is overridable in tests; in production it
// reads the credit balance from RunPod's ShowUser endpoint.
var providerCreditGateProbeRunpod = func() (float64, error) {
	user, err := runpod.NewCloudClient().ShowUser()
	if err != nil || user == nil {
		return 0, err
	}
	return user.ClientBalance, nil
}

// providerCreditGateState caches the most recent probe for each provider.
// We refresh per-provider rather than as a single batch so a slow Vast.ai
// response does not stall RunPod's gate (and vice versa). The per-provider
// inflight map prevents concurrent autopilot/launch goroutines from all
// missing the cache on the same provider and stampeding the ShowUser
// endpoint — losers wait on the winner's probe instead of issuing their own.
var providerCreditGateState struct {
	mu       sync.Mutex
	probes   map[cloud.Provider]providerCreditProbe
	inflight map[cloud.Provider]chan struct{}
}

type providerCreditProbe struct {
	balance float64
	err     error
	at      time.Time
}

// ProviderCreditStatus is the result of a single provider's gate check.
// Returned only for providers that were enabled and probed — there is no
// status entry for disabled providers, so callers do not need to filter.
type ProviderCreditStatus struct {
	Provider   cloud.Provider
	Balance    float64
	Exhausted  bool
	ProbeError error
}

// CheckProviderCreditHealth probes the configured cloud providers and reports
// whether each is healthy enough to attempt placement. Results are cached for
// providerCreditGateTTL so repeated autopilot passes don't hammer the user
// endpoint. A failed probe is treated as "not exhausted" — we err toward
// attempting placement so a transient provider API hiccup doesn't block the
// queue.
//
// Probes for the two enabled providers run in parallel. ShowUser is a
// straight HTTPS GET that typically takes 200–500ms per provider, so running
// them sequentially would double the cold-cache latency of the autopilot
// tick. Matches the pattern in cloudproviders/discovery.go.
func CheckProviderCreditHealth(cfg *config.Config) []ProviderCreditStatus {
	if cfg == nil {
		return nil
	}
	type request struct {
		provider cloud.Provider
		probe    func() (float64, error)
	}
	requests := make([]request, 0, 2)
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai) {
		requests = append(requests, request{cloud.ProviderVastai, providerCreditGateProbeVastai})
	}
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod) {
		requests = append(requests, request{cloud.ProviderRunpod, providerCreditGateProbeRunpod})
	}
	if len(requests) == 0 {
		return nil
	}

	out := make([]ProviderCreditStatus, len(requests))
	var wg sync.WaitGroup
	for i, req := range requests {
		wg.Add(1)
		go func(idx int, r request) {
			defer wg.Done()
			out[idx] = runCreditGateProbe(r.provider, r.probe)
		}(i, req)
	}
	wg.Wait()
	return out
}

// runCreditGateProbe returns the cached probe when it is fresh, and
// otherwise serializes concurrent misses on the same provider through a
// per-provider inflight channel so only one goroutine calls the probe and
// the rest reuse its result. Without this guard, a burst of autopilot ticks
// (or a parallel CreateInstance failure burst falling back to the gate via
// some future code path) could stampede ShowUser.
func runCreditGateProbe(provider cloud.Provider, probe func() (float64, error)) ProviderCreditStatus {
	now := providerCreditGateProbeNow()

	providerCreditGateState.mu.Lock()
	if providerCreditGateState.probes == nil {
		providerCreditGateState.probes = map[cloud.Provider]providerCreditProbe{}
	}
	if providerCreditGateState.inflight == nil {
		providerCreditGateState.inflight = map[cloud.Provider]chan struct{}{}
	}
	if cached, ok := providerCreditGateState.probes[provider]; ok && now.Sub(cached.at) < providerCreditGateTTL {
		providerCreditGateState.mu.Unlock()
		return creditStatusFromProbe(provider, cached)
	}
	if done, racing := providerCreditGateState.inflight[provider]; racing {
		providerCreditGateState.mu.Unlock()
		<-done
		providerCreditGateState.mu.Lock()
		cached := providerCreditGateState.probes[provider]
		providerCreditGateState.mu.Unlock()
		return creditStatusFromProbe(provider, cached)
	}
	done := make(chan struct{})
	providerCreditGateState.inflight[provider] = done
	providerCreditGateState.mu.Unlock()

	balance, err := probe()

	providerCreditGateState.mu.Lock()
	cached := providerCreditProbe{balance: balance, err: err, at: now}
	providerCreditGateState.probes[provider] = cached
	delete(providerCreditGateState.inflight, provider)
	providerCreditGateState.mu.Unlock()
	close(done)
	return creditStatusFromProbe(provider, cached)
}

func creditStatusFromProbe(provider cloud.Provider, p providerCreditProbe) ProviderCreditStatus {
	status := ProviderCreditStatus{
		Provider:   provider,
		Balance:    p.balance,
		ProbeError: p.err,
	}
	if p.err == nil && p.balance <= CreditExhaustionThreshold {
		status.Exhausted = true
	}
	return status
}

// ResetProviderCreditGateCacheForTests clears the cached probes. Exposed for
// tests that exercise the gate's caching behavior.
func ResetProviderCreditGateCacheForTests() {
	providerCreditGateState.mu.Lock()
	providerCreditGateState.probes = nil
	providerCreditGateState.mu.Unlock()
}

// AllEnabledProvidersExhausted reports whether every enabled provider in
// statuses is credit-exhausted. Returns false when at least one provider is
// healthy or when a probe failed (we treat probe failure as "unknown, attempt
// anyway"). Returns false when statuses is empty: the caller has no providers
// to gate, so there's nothing to short out.
func AllEnabledProvidersExhausted(statuses []ProviderCreditStatus) bool {
	if len(statuses) == 0 {
		return false
	}
	for _, s := range statuses {
		if !s.Exhausted {
			return false
		}
	}
	return true
}

// FormatCreditExhaustionReason returns a one-line blocked reason describing
// which providers are exhausted and their balances. Suitable for surfacing in
// the autopilot's blocked-reasons map.
func FormatCreditExhaustionReason(statuses []ProviderCreditStatus) string {
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		if !s.Exhausted {
			continue
		}
		name := s.Provider.DisplayName()
		if name == "" {
			name = string(s.Provider)
		}
		parts = append(parts, fmt.Sprintf("%s credit exhausted ($%.2f)", name, s.Balance))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + " — top up before instances can launch"
}

// logProviderCreditGate emits an oplog event summarising the gate result so
// `weft autopilot status` and the operations log explain *why* a pass stopped
// short of calling the provider.
func logProviderCreditGate(statuses []ProviderCreditStatus, rentalScopeSize int) {
	if len(statuses) == 0 {
		return
	}
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		state := "healthy"
		switch {
		case s.ProbeError != nil:
			state = "probe_error"
		case s.Exhausted:
			state = "exhausted"
		}
		parts = append(parts, fmt.Sprintf("%s=%s($%.2f)", s.Provider, state, s.Balance))
	}
	oplog.Log("auto_pilot.credit_gate",
		oplog.WithDetailf("rental_scope=%d %s", rentalScopeSize, strings.Join(parts, " ")))
}
