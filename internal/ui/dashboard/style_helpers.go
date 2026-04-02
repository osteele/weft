package dashboard

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

func (m Model) formatStatus(job *db.Job) string {
	// Check if job has a pending (target) status from three-way merge model
	if job.PendingStatus != nil {
		if job.EffectiveStatus() != *job.PendingStatus {
			return m.formatStatusValue(job, job.EffectiveStatus())
		}
		return m.formatPendingStatusDisplay(*job.PendingStatus, job.Status)
	}

	return m.formatStatusValue(job, job.EffectiveStatus())
}

func (m Model) formatStatusValue(job *db.Job, status string) string {
	if display := queueblock.Display(job, nil); display.Blocked && status == db.StatusQueued {
		return "… blocked"
	}
	switch status {
	case db.StatusRunning:
		stale := m.isJobStatusStale(job)
		// Show progress percentage if available
		if prog, ok := m.jobProgress[job.ID]; ok {
			pct := prog.DisplayPercent()
			if pct >= 0 {
				text := fmt.Sprintf("● %3d%%", pct)
				if stale {
					return lipgloss.NewStyle().Italic(true).Render(text)
				}
				return text
			}
		}
		if stale {
			return "● running?"
		}
		return "● running"
	case db.StatusPaused:
		return "⏸ paused"
	case db.StatusStarting:
		return "◐ starting"
	case db.StatusCompleted:
		if job.ExitCode == nil {
			return "✔ completed"
		}
		if *job.ExitCode == 0 {
			return "✔ succeeded"
		}
		return fmt.Sprintf("✖ failed (%d)", *job.ExitCode)
	case db.StatusQueued:
		if job.TargetKind() == db.JobTargetUnplaced {
			return "$ needs rental"
		}
		return "… queued"
	case db.StatusDead:
		return "✖ start failed"
	case db.StatusFailed:
		return "✖ crashed"
	case db.StatusKilled:
		return "✖ killed"
	case db.StatusCanceled:
		return "✖ canceled"
	case db.StatusDraft:
		return "  Draft"
	case db.StatusPendingPlacement:
		return "⧗ placing"
	default:
		return status
	}
}

// formatPendingStatusDisplay shows pending (target) status with current status context.
// Uses ⧗ (hourglass) to indicate an operation is pending.
func (m Model) formatPendingStatusDisplay(pendingStatus, currentStatus string) string {
	switch pendingStatus {
	case db.StatusDead, db.StatusKilled:
		// Show what we're transitioning from
		switch currentStatus {
		case db.StatusRunning:
			return "⧗ killing"
		case db.StatusQueued:
			return "⧗ canceling"
		default:
			return "⧗ killing"
		}
	case db.StatusCanceled:
		return "⧗ canceling"
	case db.StatusRunning:
		if currentStatus == db.StatusPaused {
			return "⧗ resuming"
		}
		return "⧗ starting"
	case db.StatusQueued:
		return "⧗ queuing"
	case db.StatusPaused:
		return "⧗ pausing"
	default:
		return "⧗ " + pendingStatus
	}
}

func (m Model) styleForJob(job *db.Job) lipgloss.Style {
	effectiveStatus := job.EffectiveStatus()

	if effectiveStatus == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode != 0 {
		return failedStyle
	}
	if effectiveStatus == db.StatusRunning || effectiveStatus == db.StatusPaused {
		host := m.findHostByName(job.Host)
		if host != nil {
			if cpuPct, ok := hostCPULoadPercent(host); ok && cpuPct > 100 {
				return failedStyle
			}
		}
	}
	return m.styleForStatus(effectiveStatus)
}

func (m Model) styleForStatus(status string) lipgloss.Style {
	switch status {
	case db.StatusRunning:
		return runningStyle
	case db.StatusPaused:
		return pendingStyle
	case db.StatusCompleted:
		return completedStyle
	case db.StatusDead:
		return deadStyle
	case db.StatusQueued:
		return queuedStyle
	case db.StatusFailed:
		return failedStyle
	case db.StatusKilled:
		return deadStyle
	case db.StatusCanceled:
		return deadStyle
	case db.StatusStarting:
		return pendingStyle
	case db.StatusPendingPlacement:
		return pendingStyle
	default:
		return lipgloss.NewStyle()
	}
}

func (m Model) areJobDependenciesMet(job *db.Job) (bool, []string) {
	return true, nil
}

// isJobStatusStale returns true if the job's host hasn't been checked recently
// This helps identify running jobs whose status might be outdated
func (m Model) isJobStatusStale(job *db.Job) bool {
	// Hosts with running jobs are refreshed every 30s, so 2 minutes gives margin
	const staleThreshold = 2 * time.Minute

	// Find the host for this job
	for _, host := range m.hosts {
		if host.Name == job.Host {
			// If host is offline or last check was more than threshold ago
			if host.Status == HostStatusOffline {
				return true
			}
			if !host.LastCheck.IsZero() && time.Since(host.LastCheck) > staleThreshold {
				return true
			}
			return false
		}
	}
	// Host not found in our list - consider it stale
	return true
}

// isHostDisconnectedLong returns true if the job's host has been disconnected
// for more than 30 minutes. Used to dim jobs on unreachable hosts.
func (m Model) isHostDisconnectedLong(job *db.Job) bool {
	const disconnectedThreshold = 30 * time.Minute

	for _, host := range m.hosts {
		if host.Name == job.Host {
			// If host is online, it's not disconnected
			if host.Status == HostStatusOnline {
				return false
			}
			// For any non-online status (offline, unknown, checking),
			// check if LastCheck is older than threshold
			if !host.LastCheck.IsZero() {
				return time.Since(host.LastCheck) > disconnectedThreshold
			}
			return false
		}
	}
	return false
}
