package web

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

const hostRecentSyncWindow = 48 * time.Hour

func filterJobsByView(jobs []*db.Job, view string) []*db.Job {
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if jobMatchesView(job, view) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func jobMatchesView(job *db.Job, view string) bool {
	switch view {
	case "recent":
		if job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusQueued || job.Status == db.StatusDraft {
			return true
		}
		return isRecentHistory(job)
	case "active":
		return job.Status == db.StatusRunning || job.Status == db.StatusStarting || job.Status == db.StatusQueued || job.Status == db.StatusDraft
	case "succeeded":
		return job.Status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0
	case "failed":
		if job.Status == db.StatusFailed || job.Status == db.StatusDead || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
			return true
		}
		return job.Status == db.StatusCompleted && (job.ExitCode == nil || *job.ExitCode != 0)
	default:
		return true
	}
}

func filterJobsByHost(jobs []*db.Job, hostFilter string, hostSyncTimes map[string]time.Time) []*db.Job {
	switch hostFilter {
	case "all":
		return jobs
	case "recent":
		if len(hostSyncTimes) == 0 || !anyHostRecentlySynced(hostSyncTimes) {
			return jobs
		}
		filtered := make([]*db.Job, 0, len(jobs))
		for _, job := range jobs {
			if job == nil {
				continue
			}
			if !job.HasInventoryHost() || isHostRecentlySynced(job.Host, hostSyncTimes) {
				filtered = append(filtered, job)
			}
		}
		return filtered
	default:
		filtered := make([]*db.Job, 0, len(jobs))
		for _, job := range jobs {
			if job == nil {
				continue
			}
			if job.HasInventoryHost() && job.Host == hostFilter {
				filtered = append(filtered, job)
			}
		}
		return filtered
	}
}

func isHostRecentlySynced(host string, hostSyncTimes map[string]time.Time) bool {
	last, ok := hostSyncTimes[host]
	if !ok || last.IsZero() {
		return false
	}
	return time.Since(last) <= hostRecentSyncWindow
}

func anyHostRecentlySynced(hostSyncTimes map[string]time.Time) bool {
	for _, last := range hostSyncTimes {
		if !last.IsZero() && time.Since(last) <= hostRecentSyncWindow {
			return true
		}
	}
	return false
}

func sortJobsForView(jobs []*db.Job, view string) {
	if view == "recent" {
		sortRecentJobs(jobs)
		return
	}
	sortJobsByNewest(jobs)
}

func sortJobsByNewest(jobs []*db.Job) {
	sort.Slice(jobs, func(i, j int) bool {
		return getJobSortTime(jobs[i]) > getJobSortTime(jobs[j])
	})
}

func sortRecentJobs(jobs []*db.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		pi, pj := recentStatusPriority(jobs[i]), recentStatusPriority(jobs[j])
		if pi != pj {
			return pi < pj
		}
		return getJobSortTime(jobs[i]) > getJobSortTime(jobs[j])
	})
}

func recentStatusPriority(job *db.Job) int {
	switch job.Status {
	case db.StatusRunning, db.StatusStarting:
		return 0
	case db.StatusQueued, db.StatusDraft:
		return 1
	default:
		return 2
	}
}

func getJobSortTime(job *db.Job) int64 {
	if job.StartTime > 0 {
		return job.StartTime
	}
	if job.CreatedAt > 0 {
		return job.CreatedAt
	}
	return job.ID
}

func isRecentHistory(job *db.Job) bool {
	const window = 24 * time.Hour
	windowSeconds := int64(window / time.Second)
	now := time.Now()
	if job.EndTime != nil {
		return now.Unix()-*job.EndTime < windowSeconds
	}
	if job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
		timestamp := job.StartTime
		if timestamp == 0 {
			timestamp = job.CreatedAt
		}
		if timestamp == 0 {
			return false
		}
		return now.Sub(time.Unix(timestamp, 0)) < window
	}
	return false
}

func formatJobStatus(job *db.Job) string {
	if job.PendingStatus != nil {
		return formatPendingStatus(*job.PendingStatus)
	}
	return formatActualStatus(job)
}

func formatActualStatus(job *db.Job) string {
	switch job.Status {
	case db.StatusCompleted:
		if job.ExitCode != nil {
			if *job.ExitCode == 0 {
				return "✓ done"
			}
			return fmt.Sprintf("✗ exit %d", *job.ExitCode)
		}
		return "completed"
	case db.StatusRunning:
		return "● running"
	case db.StatusStarting:
		return "○ starting"
	case db.StatusQueued:
		return "◌ queued"
	case db.StatusPaused:
		return "⏸ paused"
	case db.StatusDead:
		return "✗ start failed"
	case db.StatusFailed:
		return "✗ crashed"
	case db.StatusKilled:
		return "✗ killed"
	case db.StatusCanceled:
		return "✗ canceled"
	case db.StatusDraft:
		return "✎ draft"
	default:
		return job.Status
	}
}

func formatPendingStatus(status string) string {
	switch status {
	case db.StatusDead, db.StatusKilled:
		return "⧗ killing…"
	case db.StatusCanceled:
		return "⧗ canceling…"
	case db.StatusRunning:
		return "⧗ starting…"
	case db.StatusQueued:
		return "⧗ queuing…"
	case db.StatusPaused:
		return "⧗ pausing…"
	case db.StatusDraft:
		return "⧗ drafting…"
	default:
		return "⧗ " + status + "…"
	}
}

func formatJobTime(job *db.Job) string {
	if job.EndTime != nil && job.Status != db.StatusRunning && job.Status != db.StatusStarting && job.Status != db.StatusPaused {
		return formatStartTime(*job.EndTime)
	}
	if job.StartTime != 0 {
		return formatStartTime(job.StartTime)
	}
	if job.CreatedAt != 0 {
		return "Q:" + formatQueueTime(job.CreatedAt)
	}
	return "—"
}

func formatStartTime(ts int64) string {
	t := time.Unix(ts, 0)
	elapsed := time.Since(t)
	if elapsed < time.Minute {
		return "<1m ago"
	}
	if elapsed < time.Hour {
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	}
	if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	}
	return t.Format("01/02 15:04")
}

func formatQueueTime(createdAt int64) string {
	t := time.Unix(createdAt, 0)
	elapsed := time.Since(t)

	if elapsed < time.Minute {
		return "<1m"
	} else if elapsed < time.Hour {
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	} else if elapsed < 24*time.Hour {
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
	days := int(elapsed.Hours() / 24)
	return fmt.Sprintf("%dd", days)
}

func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	if max <= 1 {
		return value[:max]
	}
	return value[:max-1] + "…"
}

func shortenHost(value string, max int) string {
	value = strings.TrimSpace(value)
	return truncate(value, max)
}
