package terminal

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
)

// TestMain pins the ambient host state that View() reads to inert defaults
// for the whole package test binary. This inverts the isolation model: a
// test is hermetic by default and must *opt in* to real or specific host
// state (e.g. withStoppedDaemonStatus, SeedProviderCreditWarningForTesting),
// rather than every View()-rendering test having to remember to suppress each
// probe. A forgotten suppression — or a newly added probe routed through these
// seams — is then inert in tests instead of leaking real host state and
// displacing rows from fixed-height test viewports.
//
// Pinned here:
//   - daemonStatusProbe: report a live, current-binary daemon so
//     renderDaemonStatusLineView emits no "Daemon: …" line. Synchronous, so a
//     once-at-startup pin holds for the whole run.
//   - providerCreditWarningFetch: return no warning, so even the async refresh
//     goroutine repopulates the cache with an empty value.
//   - time.Local: pin to UTC so renderers that format an absolute timestamp
//     (formatJobListTime) emit the same bytes in every zone. Without this a
//     golden frame only matches in whichever zone it was last regenerated in.
//     Assigned before m.Run() so no test goroutine can observe the write.
//
// These are process-lifetime defaults with no cleanup; individual tests
// override locally (with their own t.Cleanup) when they exercise a probe.
func TestMain(m *testing.M) {
	time.Local = time.UTC

	daemonStatusProbe = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{Live: true}, nil
	}

	providerCreditWarningFetch = func() string { return "" }
	providerCreditWarningCache.mu.Lock()
	providerCreditWarningCache.warning = ""
	providerCreditWarningCache.expires = time.Now().Add(24 * time.Hour)
	providerCreditWarningCache.initialized = true
	providerCreditWarningCache.mu.Unlock()

	m.Run()
}
