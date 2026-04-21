package terminal

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

// renderSelectedJobDetail returns the "Job:" and "Host:" footer lines for the
// job under the cursor. The Host line is omitted for unplaced jobs (their
// placement context is carried in the Job line's status segment). Returns
// nil when no job is selected.
//
// Fields deliberately excluded: project (already shown in the job list row),
// queue name (only one queue is in practical use), and the command / script
// tail (too long for the footer and already visible in the row).
func renderSelectedJobDetail(job *db.Job, live *db.LaunchLiveState, now time.Time) []string {
	if job == nil {
		return nil
	}
	var lines []string
	if s := renderJobFooterLine(job, live, now); s != "" {
		lines = append(lines, s)
	}
	if s := renderHostFooterLine(job); s != "" {
		lines = append(lines, s)
	}
	return lines
}

// renderJobFooterLine returns "Job: wj<id> · <status> · ETA …" summarising
// the job under the cursor. Returns "" when job is nil.
func renderJobFooterLine(job *db.Job, live *db.LaunchLiveState, now time.Time) string {
	if job == nil {
		return ""
	}
	parts := []string{"Job: " + formatJobIDDisplay(job.ID)}
	parts = appendJobStatusParts(parts, job, now)
	if job.TargetKind() == db.JobTargetUnplaced {
		parts = appendUnplacedParts(parts, job)
	}
	if remaining, ok := estimateRunningJobRemaining(job, singletonLive(live, job.LaunchID), now); ok && remaining.Mean > 0 {
		parts = append(parts, "ETA "+remaining.FormatWithBounds())
	}
	return strings.Join(parts, " · ")
}

// renderHostFooterLine returns "Host: <target> · provider: <Display>" for
// placed jobs, or "" for unplaced / nil jobs. Inventory jobs render just
// "Host: <hostname>" (no provider suffix — the provider concept applies only
// to rentals).
func renderHostFooterLine(job *db.Job) string {
	if job == nil {
		return ""
	}
	switch job.TargetKind() {
	case db.JobTargetInventoryHost:
		if h := strings.TrimSpace(job.Host); h != "" {
			return "Host: " + h
		}
		return ""
	case db.JobTargetRentalInstance:
		parts := []string{"Host: " + job.TargetDisplay()}
		if display := providerDisplayName(job.ProviderName()); display != "" {
			parts = append(parts, "provider: "+display)
		}
		return strings.Join(parts, " · ")
	}
	return ""
}

// providerDisplayName returns the human-readable provider name for display
// (e.g. "Vast.ai", "RunPod"), or "" for unknown providers.
func providerDisplayName(provider string) string {
	switch provider {
	case string(cloud.ProviderVastai):
		return "Vast.ai"
	case string(cloud.ProviderRunpod):
		return "RunPod"
	default:
		return ""
	}
}

func formatJobIDDisplay(id int64) string {
	return fmt.Sprintf("wj%d", id)
}

func appendJobStatusParts(parts []string, job *db.Job, now time.Time) []string {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting:
		if job.StartTime > 0 {
			parts = append(parts, "elapsed "+estimate.FormatDurationShort(now.Sub(time.Unix(job.StartTime, 0))))
		}
	case db.StatusQueued, db.StatusPendingPlacement:
		if t := firstNonZeroTimestamp(job.QueuedAt, job.CreatedAt); t > 0 {
			parts = append(parts, "waiting "+estimate.FormatDurationShort(now.Sub(time.Unix(t, 0))))
		}
	case db.StatusFailed:
		if job.ExitCode != nil {
			parts = append(parts, fmt.Sprintf("exit %d", *job.ExitCode))
		}
		if r := strings.TrimSpace(job.FailureReason); r != "" {
			parts = append(parts, r)
		} else if m := firstLine(job.ErrorMessage); m != "" {
			parts = append(parts, m)
		}
	}
	return parts
}

func appendUnplacedParts(parts []string, job *db.Job) []string {
	parts = append(parts, "unplaced")
	if req := formatResourceRequest(job); req != "" {
		parts = append(parts, "wants "+req)
	}
	if len(job.PlacementReasons) > 0 {
		parts = append(parts, "blocked: "+strings.Join(job.PlacementReasons, "; "))
	}
	return parts
}

func formatResourceRequest(job *db.Job) string {
	var parts []string
	if job.GPUClass != "" {
		parts = append(parts, job.GPUClass)
	}
	if job.GPUMemGB != nil {
		parts = append(parts, fmt.Sprintf("≥%dGB", *job.GPUMemGB))
	}
	return strings.Join(parts, " ")
}

// singletonLive converts a single (live, launchID) pair into the map form
// expected by estimateRunningJobRemaining. Avoids allocating for the nil
// case.
func singletonLive(live *db.LaunchLiveState, launchID *int64) map[int64]*db.LaunchLiveState {
	if live == nil || launchID == nil {
		return nil
	}
	return map[int64]*db.LaunchLiveState{*launchID: live}
}

func firstNonZeroTimestamp(xs ...int64) int64 {
	for _, x := range xs {
		if x > 0 {
			return x
		}
	}
	return 0
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxRunes = 80
	if utf8.RuneCountInString(s) > maxRunes {
		runes := []rune(s)
		s = string(runes[:maxRunes-1]) + "…"
	}
	return s
}
