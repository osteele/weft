package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/progress"
)

type groupedStatusSection struct {
	title string
	key   string
	jobs  []*db.Job
}

func renderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, nil, time.Now())
}

func renderJobListGroupedStatusPlainWithLiveState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState) string {
	return renderJobListGroupedStatusPlainAt(jobs, width, launchLiveByID, time.Now())
}

func renderJobListGroupedStatusPlainAt(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, now time.Time) string {
	running := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
	unplaced := make([]*db.Job, 0)
	completions := make([]*db.Job, 0)
	failures := make([]*db.Job, 0)
	killedCanceled := make([]*db.Job, 0)

	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch groupedStatusBucket(job) {
		case "running":
			running = append(running, job)
		case "queued":
			queued = append(queued, job)
		case "unplaced":
			unplaced = append(unplaced, job)
		case "completions":
			completions = append(completions, job)
		case "failures":
			failures = append(failures, job)
		case "killed_canceled":
			killedCanceled = append(killedCanceled, job)
		}
	}

	sections := []groupedStatusSection{
		{title: "Running", key: "running", jobs: running},
		{title: "Queued", key: "queued", jobs: queued},
		{title: "Unplaced", key: "unplaced", jobs: unplaced},
		{title: "Completions", key: "completions", jobs: completions},
		{title: "Failures", key: "failures", jobs: failures},
		{title: "Killed/Canceled", key: "killed_canceled", jobs: killedCanceled},
	}

	lines := make([]string, 0, len(jobs)+8)
	for _, section := range sections {
		if len(section.jobs) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%d):", section.title, len(section.jobs)))
		for _, job := range section.jobs {
			parts := []string{
				fmt.Sprintf("- %d — %s", job.ID, groupedStatusJobLabel(job)),
			}
			if progressText := groupedStatusProgressSuffix(job, section.key, launchLiveByID); progressText != "" {
				parts = append(parts, progressText)
			}
			if timing := groupedStatusTimingSuffix(job, section.key, now); timing != "" {
				parts = append(parts, timing)
			}
			if suffix := groupedStatusOutcomeSuffix(job, section.title); suffix != "" {
				parts = append(parts, suffix)
			}
			line := strings.Join(parts, " — ")
			lines = append(lines, line)
		}
		lines = append(lines, "")
	}

	if len(lines) == 0 {
		return "None\n"
	}

	// Remove trailing blank line.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if width > 0 {
		for i := range lines {
			lines[i] = truncateDisplayWidth(lines[i], width)
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

func groupedStatusProgressSuffix(job *db.Job, sectionKey string, launchLiveByID map[int64]*db.LaunchLiveState) string {
	if job == nil || sectionKey != "running" {
		return ""
	}
	if job.LaunchID != nil && launchLiveByID != nil {
		if live := launchLiveByID[*job.LaunchID]; live != nil && live.JobProgressID == job.ID {
			if pctText := strings.TrimSpace(progress.FormatPhaseProgress(0, live.JobProgressPct)); pctText != "" {
				return "running " + pctText
			}
		}
	}
	switch job.EffectiveStatus() {
	case db.StatusStarting:
		return "starting"
	case db.StatusRunning:
		return "running"
	default:
		return ""
	}
}

func groupedStatusTimingSuffix(job *db.Job, sectionKey string, now time.Time) string {
	if job == nil {
		return ""
	}
	if sectionKey == "running" && job.StartTime > 0 {
		return "running " + shortRelativeTime(now.Unix()-job.StartTime)
	}
	placedAt := job.QueuedAt
	if placedAt == 0 {
		placedAt = job.CreatedAt
	}
	if placedAt == 0 {
		placedAt = job.StartTime
	}
	if placedAt <= 0 {
		return ""
	}
	label := "placed"
	if sectionKey == "unplaced" {
		label = "queued"
	}
	return label + " " + shortRelativeTime(now.Unix()-placedAt)
}

func groupedStatusBucket(job *db.Job) string {
	status := job.EffectiveStatus()
	switch status {
	case db.StatusRunning, db.StatusStarting:
		return "running"
	case db.StatusQueued, db.StatusPendingPlacement:
		if job.TargetKind() == db.JobTargetUnplaced {
			return "unplaced"
		}
		return "queued"
	case db.StatusKilled, db.StatusCanceled:
		return "killed_canceled"
	case db.StatusFailed, db.StatusDead:
		return "failures"
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return "failures"
		}
		return "completions"
	default:
		return ""
	}
}

func groupedStatusJobLabel(job *db.Job) string {
	project := campaign.JobProjectLabel(job)
	desc := strings.TrimSpace(job.EffectiveDescription())
	label := strings.TrimSpace(project)
	if desc != "" {
		if label != "" {
			label += " "
		}
		label += desc
	}
	if label == "" {
		label = job.EffectiveCommand()
	}
	scope := groupedStatusScopeLabel(job)
	if scope != "" {
		label += fmt.Sprintf(" (%s)", scope)
	}
	return label
}

func groupedStatusScopeLabel(job *db.Job) string {
	switch {
	case job == nil:
		return ""
	case job.UsesRentalPlacement():
		return "rental"
	case job.UsesInventoryPlacement():
		return "inventory"
	default:
		return ""
	}
}

func groupedStatusOutcomeSuffix(job *db.Job, sectionTitle string) string {
	switch sectionTitle {
	case "Completions":
		return "completed ok"
	case "Failures":
		status := job.EffectiveStatus()
		if status == db.StatusCompleted && job.ExitCode != nil {
			return fmt.Sprintf("completed (exit %d)", *job.ExitCode)
		}
		return status
	case "Killed/Canceled":
		return job.EffectiveStatus()
	default:
		return ""
	}
}
