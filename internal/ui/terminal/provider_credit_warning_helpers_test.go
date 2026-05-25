package terminal

import (
	"testing"
	"time"
)

// SuppressProviderCreditWarningForTesting seeds the provider-credit-warning
// cache with an empty value and stubs the fetcher so View() rendering does
// not depend on the host's real Vast.ai/RunPod account balance or on
// whether a previous test in the same run already populated the cache.
//
// Without this, tests that exercise watchModel.View() inherit whatever the
// async refresh goroutine happened to fetch — a known source of order-
// dependent flakes (a warning line can displace expected content from a
// fixed-height test viewport).
//
// Registered cleanup restores the previous state so subsequent tests that
// intentionally exercise the credit-warning path keep working.
func SuppressProviderCreditWarningForTesting(t *testing.T) {
	t.Helper()

	prevFetch := providerCreditWarningFetch
	providerCreditWarningFetch = func() string { return "" }

	providerCreditWarningCache.mu.Lock()
	prevWarn := providerCreditWarningCache.warning
	prevExp := providerCreditWarningCache.expires
	prevInit := providerCreditWarningCache.initialized
	providerCreditWarningCache.warning = ""
	providerCreditWarningCache.expires = time.Now().Add(time.Hour)
	providerCreditWarningCache.initialized = true
	providerCreditWarningCache.mu.Unlock()

	t.Cleanup(func() {
		providerCreditWarningFetch = prevFetch
		providerCreditWarningCache.mu.Lock()
		providerCreditWarningCache.warning = prevWarn
		providerCreditWarningCache.expires = prevExp
		providerCreditWarningCache.initialized = prevInit
		providerCreditWarningCache.mu.Unlock()
	})
}
