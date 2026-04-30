package monitor

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/db"
)

const (
	// AutopilotPausedBannerID is the stable banner ID for the
	// autopilot-paused notice. Users frequently pause autopilot for a
	// manual launch and forget to resume; surfacing it across all TUIs
	// makes the state visible until acted on.
	AutopilotPausedBannerID = "autopilot-paused"

	defaultAutopilotPollInterval = 30 * time.Second
)

// AutopilotPausedConfig configures WatchAutopilotPaused.
type AutopilotPausedConfig struct {
	Database *sql.DB
	Bus      *banner.Bus
	// Interval between polls. Defaults to 30s — autopilot pause is a
	// rare-but-sticky state and 30s is fast enough to feel responsive.
	Interval time.Duration
}

// WatchAutopilotPaused polls db.LoadAutopilotState every interval. When
// autopilot is paused, it pushes a warning banner; when it resumes, the
// banner is cleared. Runs until ctx is cancelled.
func WatchAutopilotPaused(ctx context.Context, cfg AutopilotPausedConfig) {
	if cfg.Database == nil || cfg.Bus == nil {
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultAutopilotPollInterval
	}

	tick := func() {
		state, err := db.LoadAutopilotState(cfg.Database)
		if err != nil {
			slog.Debug("autopilot-pause poll failed", "component", "banner", "error", err)
			return
		}
		if state == nil || !state.Paused {
			cfg.Bus.Clear(AutopilotPausedBannerID)
			return
		}
		cfg.Bus.Push(banner.Banner{
			ID:       AutopilotPausedBannerID,
			Severity: banner.SeverityWarning,
			Text:     formatAutopilotPausedText(state),
			Hint:     "weft autopilot resume",
		})
	}

	tick() // initial check at startup so the first frame already reflects state
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

func formatAutopilotPausedText(state *db.AutopilotState) string {
	parts := []string{"autopilot paused"}
	if reason := strings.TrimSpace(state.PausedReason); reason != "" {
		parts = append(parts, "— "+reason)
	}
	if by := strings.TrimSpace(state.PausedBy); by != "" {
		parts = append(parts, "(by "+by+")")
	}
	return strings.Join(parts, " ")
}
