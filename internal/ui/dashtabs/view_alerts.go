package dashtabs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

type alertsView struct{}

func newAlertsView() *alertsView                       { return &alertsView{} }
func (v *alertsView) Title() string                    { return "Alerts" }
func (v *alertsView) ShortKey() string                 { return "6" }
func (v *alertsView) Init() tea.Cmd                    { return nil }
func (v *alertsView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

type alert struct {
	severity Attention
	since    time.Time // when the condition started (or was first observed)
	text     string
}

func (v *alertsView) Render(width, height int, snap Snapshot, _ bool) string {
	now := time.Now()
	alerts := collectAlerts(snap, now)
	if len(alerts) == 0 {
		return runningStyle.Render("✓  All clear.")
	}
	// Strict reverse chronological — newest first across all severities.
	// Severity is still encoded by the marker + color on each row.
	sort.SliceStable(alerts, func(i, j int) bool {
		return alerts[i].since.After(alerts[j].since)
	})

	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("Alerts — %d items", len(alerts))))
	b.WriteString("\n\n")
	for _, a := range alerts {
		marker := "•"
		style := dimStyle
		switch a.severity {
		case AttentionWarn:
			marker = "⚠"
			style = failedStyle
		case AttentionInfo:
			marker = "ℹ"
			style = queuedStyle
		}
		ts := alertTimestamp(a.since, now)
		b.WriteString(fmt.Sprintf("  %s  %s  %s\n",
			style.Render(marker),
			dimStyle.Render(ts),
			a.text,
		))
	}
	_ = width
	_ = height
	return b.String()
}

// alertTimestamp formats the "since" time as a short relative string plus an
// absolute clock time for unambiguous reference.
func alertTimestamp(since, now time.Time) string {
	if since.IsZero() {
		return "[—            ]"
	}
	d := now.Sub(since)
	var rel string
	switch {
	case d < time.Minute:
		rel = "just now"
	case d < time.Hour:
		rel = fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		rel = fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		rel = fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
	return fmt.Sprintf("[%-9s %s]", rel, since.Format("Mon 15:04"))
}

func (v *alertsView) Attention(snap Snapshot) Attention {
	for _, a := range collectAlerts(snap, time.Now()) {
		if a.severity == AttentionWarn {
			return AttentionWarn
		}
	}
	return AttentionNone
}

func collectAlerts(snap Snapshot, now time.Time) []alert {
	out := []alert{}

	// Cost above target — undated; use snapshot time.
	if snap.SpendTargetUSD > 0 && snap.SpendUSDPerHour > snap.SpendTargetUSD {
		out = append(out, alert{
			severity: AttentionWarn,
			since:    snap.LoadedAt,
			text:     fmt.Sprintf("Spend $%.2f/hr exceeds target $%.2f/hr", snap.SpendUSDPerHour, snap.SpendTargetUSD),
		})
	}

	// Stuck queued
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusQueued, db.StatusPendingPlacement:
			if j.QueuedAt > 0 && now.Sub(time.Unix(j.QueuedAt, 0)) > 30*time.Minute {
				out = append(out, alert{
					severity: AttentionInfo,
					since:    time.Unix(j.QueuedAt, 0),
					text:     fmt.Sprintf("wj%d queued — no placement", j.ID),
				})
			}
		}
	}

	// Clustered failures
	for _, f := range snap.RecentFailures {
		if f.Count >= 2 {
			out = append(out, alert{
				severity: AttentionWarn,
				since:    f.When,
				text:     fmt.Sprintf("Clustered failures on %s — %d in last 24h", f.Provider, f.Count),
			})
		}
	}

	// Recent failed jobs (last 1h) — bucketed
	cutoff := now.Add(-time.Hour).Unix()
	var lastFail time.Time
	failedRecent := 0
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		if j.EffectiveStatus() != db.StatusFailed {
			continue
		}
		if j.EndTime != nil && *j.EndTime >= cutoff {
			failedRecent++
			when := time.Unix(*j.EndTime, 0)
			if when.After(lastFail) {
				lastFail = when
			}
		}
	}
	if failedRecent > 0 {
		out = append(out, alert{
			severity: AttentionInfo,
			since:    lastFail,
			text:     fmt.Sprintf("%d job failures in the last hour", failedRecent),
		})
	}

	// Stale planned launches (>1 day old, never created on provider)
	if old := stalePlannedLaunches(snap.StaleInstances, now); old > 0 {
		oldest := oldestStalePlannedTime(snap.StaleInstances)
		out = append(out, alert{
			severity: AttentionWarn,
			since:    oldest,
			text:     fmt.Sprintf("%d planned cloud launches stuck (no provider instance, oldest %s) — cleanup needed", old, alertDuration(now.Sub(oldest))),
		})
	}

	// Autopilot paused
	if snap.AutopilotState != nil && snap.AutopilotState.Paused {
		out = append(out, alert{
			severity: AttentionInfo,
			since:    snap.AutopilotState.PausedAt,
			text: fmt.Sprintf("Autopilot paused by %s (%s)",
				orDefault(snap.AutopilotState.PausedBy, "?"),
				orDefault(snap.AutopilotState.PausedReason, "no reason")),
		})
	}

	return out
}

func stalePlannedLaunches(stale []*db.Launch, now time.Time) int {
	cutoff := now.Add(-24 * time.Hour).Unix()
	n := 0
	for _, l := range stale {
		if l == nil {
			continue
		}
		if l.Status != db.LaunchStatusPlanned {
			continue
		}
		if l.ProviderInstanceID != "" {
			continue
		}
		if l.CreatedAt <= cutoff {
			n++
		}
	}
	return n
}

func oldestStalePlannedTime(stale []*db.Launch) time.Time {
	var oldest time.Time
	for _, l := range stale {
		if l == nil || l.Status != db.LaunchStatusPlanned {
			continue
		}
		t := time.Unix(l.CreatedAt, 0)
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

func alertDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
