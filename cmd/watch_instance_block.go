package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

type watchInstanceBlockOptions struct {
	plain                    bool
	spinner                  string
	showSpinnerIfNonTerminal bool
	resolvedJobsOverride     *int
	dimJobStatuses           bool
	now                      time.Time
}

func formatWatchInstanceBlock(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) string {
	return strings.Join(formatWatchInstanceBlockLines(update, jobProgressHWM, opts), "\n")
}

func formatWatchInstanceBlockLines(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) []string {
	ci := update.CloudInstance
	if ci == nil {
		return nil
	}

	lines := []string{formatWatchInstanceHeaderLine(ci, update.Instance, opts)}
	lines = append(lines, formatWatchProviderLine(ci, update.Instance))

	if update.BootstrapStage != "" {
		lines = append(lines, fmt.Sprintf("  Bootstrap: %s", campaign.BootstrapStageLabel(update.BootstrapStage)))
	}
	if update.InstancePhase != "" {
		lines = append(lines, fmt.Sprintf("  Phase: %s", campaign.InstancePhaseLabel(update.InstancePhase)))
	}
	if label := campaign.TerminationIntentLabel(update.TerminationIntent); label != "" {
		lines = append(lines, fmt.Sprintf("  Termination: %s", label))
	}
	if costLine := formatWatchInstanceCostLine(ci, update.Instance, opts.now); costLine != "" {
		lines = append(lines, costLine)
	}

	if len(update.Jobs) == 0 {
		return lines
	}

	displayStatuses := make([]string, len(update.Jobs))
	resolved := 0
	for i, job := range update.Jobs {
		displayStatuses[i] = campaign.JobDisplayStatus(job, update.JobAttemptOutcomes)
		if campaign.IsJobTerminal(displayStatuses[i]) {
			resolved++
		}
	}
	if opts.resolvedJobsOverride != nil {
		resolved = *opts.resolvedJobsOverride
	}
	lines = append(lines, fmt.Sprintf("  Jobs: %d/%d resolved", resolved, len(update.Jobs)))

	for i, job := range update.Jobs {
		desc := job.Description
		if desc == "" {
			desc = campaign.TruncateCommand(job.Command, 50)
		}

		statusText := displayStatuses[i]
		if progress := watchJobProgressPercent(update, job, jobProgressHWM); progress > 0 {
			statusText = fmt.Sprintf("running %3d%%", progress)
		}

		lines = append(lines, fmt.Sprintf("    %4d  %s  %-12s  %s",
			job.ID,
			renderWatchJobStatusText(statusText, displayStatuses[i], opts),
			job.DirectoryTailDisplay(),
			desc,
		))
	}

	return lines
}

func formatWatchInstanceHeaderLine(ci *db.CloudInstance, inst *cloud.Instance, opts watchInstanceBlockOptions) string {
	statusLabel := watchInstanceStatusLabel(ci, inst)
	if label := ci.GraceStatusLabel(); label != "" {
		statusLabel = label
	}
	if ci.TerminationReason != "" && ci.TerminationReason != db.TerminationReasonCompleted {
		statusLabel += " (" + ci.TerminationReason + ")"
	}

	statusText := statusLabel
	if !opts.plain {
		statusText = watchStatusBlockStyle(statusLabel, ci.Status).Render(statusLabel)
	}

	header := fmt.Sprintf("Instance %d — %s — %s", ci.ID, ci.DisplayGPUSpec(), statusText)
	if !opts.plain {
		header = watchTitleStyle.Render(header)
	}
	if opts.showSpinnerIfNonTerminal && !campaign.IsInstanceTerminal(ci.Status) && opts.spinner != "" {
		header += " " + opts.spinner
	}
	return header
}

func watchInstanceStatusLabel(ci *db.CloudInstance, inst *cloud.Instance) string {
	if ci == nil {
		return ""
	}
	statusLabel := ci.Status
	if inst == nil || inst.Status == "" || campaign.IsInstanceTerminal(ci.Status) {
		return statusLabel
	}
	if ci.Status == db.CloudInstanceStatusRunning && inst.Status != "running" {
		return inst.Status
	}
	if ci.Status == db.CloudInstanceStatusLaunching {
		return inst.Status
	}
	return statusLabel
}

func watchStatusBlockStyle(displayStatus, dbStatus string) lipgloss.Style {
	switch displayStatus {
	case "loading", "launching", "provisioning":
		return watchDimStyle
	case db.CloudInstanceStatusCompleted:
		return watchCompletedStyle
	case db.CloudInstanceStatusFailed, db.CloudInstanceStatusCancelled:
		return watchFailedStyle
	}
	switch dbStatus {
	case db.CloudInstanceStatusCompleted:
		return watchCompletedStyle
	case db.CloudInstanceStatusFailed, db.CloudInstanceStatusCancelled:
		return watchFailedStyle
	default:
		return watchStatusStyle
	}
}

func renderWatchJobStatusText(statusText, displayStatus string, opts watchInstanceBlockOptions) string {
	padded := fmt.Sprintf("%-12s", statusText)
	if opts.plain {
		return padded
	}
	if opts.dimJobStatuses {
		return watchDimStyle.Render(padded)
	}

	switch displayStatus {
	case db.StatusRunning:
		return watchRunningStyle.Render(padded)
	case db.StatusCompleted:
		return watchCompletedStyle.Render(padded)
	case db.StatusFailed, db.AttemptOutcomeOrphaned, db.AttemptOutcomeCancelled:
		return watchFailedStyle.Render(padded)
	default:
		return watchDimStyle.Render(padded)
	}
}

func watchJobProgressPercent(update campaign.InstanceUpdate, job *db.Job, jobProgressHWM map[int64]int) int {
	if job == nil || job.Status != db.StatusRunning {
		return -1
	}
	if jobProgressHWM != nil && jobProgressHWM[job.ID] > 0 {
		return jobProgressHWM[job.ID]
	}
	if update.JobProgressID == job.ID && update.JobProgress > 0 {
		return update.JobProgress
	}
	return -1
}

func formatWatchProviderLine(ci *db.CloudInstance, inst *cloud.Instance) string {
	if ci == nil {
		return ""
	}

	line := fmt.Sprintf("  %s:", ci.Provider)
	if providerInstID := ci.EffectiveProviderID(); providerInstID != "" {
		line += " " + providerInstID
		if inst != nil && !campaign.IsInstanceTerminal(ci.Status) && inst.Status != "" {
			line += fmt.Sprintf(" (%s)", inst.Status)
		}
		return line
	}

	return line + " (provisioning...)"
}

func formatWatchInstanceCostLine(ci *db.CloudInstance, inst *cloud.Instance, now time.Time) string {
	if ci == nil || inst == nil || ci.LaunchedAt == nil {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	uptime := now.Sub(time.Unix(*ci.LaunchedAt, 0)).Truncate(time.Second)
	cost := uptime.Hours() * inst.CostPerHour
	return fmt.Sprintf("  Cost: $%.2f (uptime: %s)", cost, uptime)
}
