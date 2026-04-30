package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/vastai"
)

const (
	// R2UnreachableBannerID is the stable banner ID for "R2 storage
	// cannot be reached." The historical surface for this was an ad-hoc
	// slog.Warn from the cloud sync path; the banner makes it visible
	// in any TUI a user has open.
	R2UnreachableBannerID = "r2-unreachable"
	// VastUnreachableBannerID is the stable banner ID for "vastai CLI
	// cannot reach the Vast.ai API." Surfaces auth, network, and CLI
	// availability problems before they cascade into failed launches.
	VastUnreachableBannerID = "vastai-unreachable"

	defaultCloudHealthInterval = 2 * time.Minute
	cloudHealthProbeTimeout    = 10 * time.Second
	// cloudHealthFailureThreshold defines how many consecutive probe
	// failures must accumulate before a banner is shown. A single
	// transient failure (e.g. one packet lost during a poll) shouldn't
	// pop a UI element.
	cloudHealthFailureThreshold = 2
)

// CloudHealthConfig configures WatchCloudHealth.
type CloudHealthConfig struct {
	Bus *banner.Bus
	// R2 is the configured client; pass nil to skip R2 probing.
	R2 *r2.Client
	// Vastai is the configured client; pass nil to skip Vastai probing.
	Vastai *vastai.Client
	// Interval between health probes per provider. Defaults to 2 min.
	Interval time.Duration
}

// WatchCloudHealth periodically probes the configured cloud providers
// (R2 storage and Vast.ai API) and pushes a warning banner when a
// provider has been unreachable for cloudHealthFailureThreshold
// consecutive probes. Banners clear on the next successful probe.
//
// Probes use a short timeout so a single sluggish provider doesn't
// stall the loop. Runs until ctx is cancelled.
func WatchCloudHealth(ctx context.Context, cfg CloudHealthConfig) {
	if cfg.Bus == nil {
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultCloudHealthInterval
	}

	r2Failures := 0
	vastFailures := 0

	tick := func() {
		if cfg.R2 != nil {
			r2Failures = pollR2(ctx, cfg, r2Failures)
		}
		if cfg.Vastai != nil {
			vastFailures = pollVastai(cfg, vastFailures)
		}
	}

	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func pollR2(parent context.Context, cfg CloudHealthConfig, failures int) int {
	ctx, cancel := context.WithTimeout(parent, cloudHealthProbeTimeout)
	defer cancel()
	// HEAD on a sentinel key. The key need not exist — a "not found"
	// response still means the bucket is reachable. Only network /
	// auth / DNS errors count as failure.
	_, err := cfg.R2.ObjectExists(ctx, "_health/probe")
	if err == nil {
		cfg.Bus.Clear(R2UnreachableBannerID)
		return 0
	}
	failures++
	slog.Debug("R2 health probe failed", "component", "banner", "consecutive", failures, "error", err)
	if failures >= cloudHealthFailureThreshold {
		cfg.Bus.Push(banner.Banner{
			ID:       R2UnreachableBannerID,
			Severity: banner.SeverityWarning,
			Text:     "R2 storage unreachable; cloud sync and artifact fetches will fail",
			Hint:     "check network / VPN; cloud-job results may stall until R2 returns",
		})
	}
	return failures
}

func pollVastai(cfg CloudHealthConfig, failures int) int {
	// Client.Available() spawns the vastai CLI and pings the user
	// endpoint. It honours its own internal grace period (recent
	// success window) so transient hiccups don't trip us.
	err := cfg.Vastai.Available()
	if err == nil {
		cfg.Bus.Clear(VastUnreachableBannerID)
		return 0
	}
	failures++
	slog.Debug("vastai health probe failed", "component", "banner", "consecutive", failures, "error", err)
	if failures >= cloudHealthFailureThreshold {
		cfg.Bus.Push(banner.Banner{
			ID:       VastUnreachableBannerID,
			Severity: banner.SeverityWarning,
			Text:     "vast.ai API unreachable; instance launches and status polls will fail",
			Hint:     "check network / VPN, or run `vastai show user` to confirm CLI auth",
		})
	}
	return failures
}
