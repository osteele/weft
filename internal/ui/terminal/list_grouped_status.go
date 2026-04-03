package terminal

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

type groupedStatusSection struct {
	title string
	jobs  []*db.Job
}

func renderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	running := make([]*db.Job, 0)
	queued := make([]*db.Job, 0)
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
		case "completions":
			completions = append(completions, job)
		case "failures":
			failures = append(failures, job)
		case "killed_canceled":
			killedCanceled = append(killedCanceled, job)
		}
	}

	sections := []groupedStatusSection{
		{title: "Running", jobs: running},
		{title: "Queued", jobs: queued},
		{title: "Completions", jobs: completions},
		{title: "Failures", jobs: failures},
		{title: "Killed/Canceled", jobs: killedCanceled},
	}

	lines := make([]string, 0, len(jobs)+8)
	for _, section := range sections {
		if len(section.jobs) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%d):", section.title, len(section.jobs)))
		for _, job := range section.jobs {
			line := fmt.Sprintf("- %d — %s", job.ID, groupedStatusJobLabel(job))
			if suffix := groupedStatusOutcomeSuffix(job, section.title); suffix != "" {
				line = line + " — " + suffix
			}
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

func groupedStatusBucket(job *db.Job) string {
	status := job.EffectiveStatus()
	switch status {
	case db.StatusRunning, db.StatusStarting:
		return "running"
	case db.StatusQueued:
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
