package terminal

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
)

// renderSelectedJobDetail returns a single footer line summarising the job
// under the cursor — placement (host+GPU or instance+phase or unplaced+wants)
// joined with status-specific context (elapsed, waiting, or exit+reason).
// Returns nil when job is nil or no useful detail is available.
//
// Fields deliberately excluded: project (already shown in the job list row),
// queue name (only one queue is in practical use), and the command / script
// tail (too long for the footer and already visible in the job list row).
func renderSelectedJobDetail(job *db.Job, live *db.LaunchLiveState, now time.Time) []string {
	if job == nil {
		return nil
	}
	parts := []string{fmt.Sprintf("#%d", job.ID)}
	parts = appendPlacementParts(parts, job, live)
	parts = appendStatusParts(parts, job, now)
	if len(parts) <= 1 {
		return nil
	}
	return []string{parts[0] + "  " + strings.Join(parts[1:], " · ")}
}

func appendPlacementParts(parts []string, job *db.Job, live *db.LaunchLiveState) []string {
	switch job.TargetKind() {
	case db.JobTargetInventoryHost:
		parts = append(parts, "host "+job.Host)
		if gpu := strings.TrimSpace(job.GPU); gpu != "" {
			parts = append(parts, "GPU "+gpu)
		}
	case db.JobTargetRentalInstance:
		if job.LaunchID != nil {
			parts = append(parts, "instance "+ids.FormatInstanceID(*job.LaunchID))
		} else {
			parts = append(parts, "rental")
		}
		if live != nil {
			if phase := strings.TrimSpace(live.InstancePhase); phase != "" {
				parts = append(parts, "phase "+phase)
			}
		}
	case db.JobTargetUnplaced:
		parts = append(parts, "unplaced")
		if req := formatResourceRequest(job); req != "" {
			parts = append(parts, "wants "+req)
		}
		if len(job.PlacementReasons) > 0 {
			parts = append(parts, "blocked: "+strings.Join(job.PlacementReasons, "; "))
		}
	}
	return parts
}

func appendStatusParts(parts []string, job *db.Job, now time.Time) []string {
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
