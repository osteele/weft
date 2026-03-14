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

func normalizeWatchInstanceUpdate(update campaign.InstanceUpdate, ci *db.CloudInstance) campaign.InstanceUpdate {
	if update.CloudInstance == nil {
		update.CloudInstance = ci
	}
	return update
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
		lines = append(lines, fmt.Sprintf("  Phase: %s", formatObservedPhase(update, opts.now)))
	}
	if label := campaign.TerminationIntentLabel(update.TerminationIntent); label != "" {
		lines = append(lines, fmt.Sprintf("  Termination: %s", label))
		if detail := campaign.TerminationIntentDetail(update.TerminationIntent); detail != "" {
			lines = append(lines, fmt.Sprintf("  Detail: %s", detail))
		}
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

	jobGroups := groupCloudInstanceJobs(ci.ID, update.Jobs)
	for _, job := range jobGroups.current {
		i := findInstanceJobIndex(update.Jobs, job)
		if i < 0 {
			continue
		}
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
			campaign.JobProjectLabel(job),
			desc,
		))
		if campaign.IsJobTerminal(displayStatuses[i]) {
			if summary := formatUploadSummary(update.JobPhaseTimings[job.ID]); summary != "" {
				lines = append(lines, fmt.Sprintf("          uploads: %s", summary))
			}
		}
	}

	if len(jobGroups.historical) > 0 {
		lines = append(lines, historicalCloudInstanceJobsHeader)
	}
	for _, job := range jobGroups.historical {
		i := findInstanceJobIndex(update.Jobs, job)
		if i < 0 {
			continue
		}
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
			campaign.JobProjectLabel(job),
			desc,
		))
		if campaign.IsJobTerminal(displayStatuses[i]) {
			if summary := formatUploadSummary(update.JobPhaseTimings[job.ID]); summary != "" {
				lines = append(lines, fmt.Sprintf("          uploads: %s", summary))
			}
		}
	}

	return lines
}

func findInstanceJobIndex(jobs []*db.Job, target *db.Job) int {
	for i, job := range jobs {
		if job == target {
			return i
		}
		if job != nil && target != nil && job.ID == target.ID {
			return i
		}
	}
	return -1
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
	if job == nil || job.EffectiveStatus() != db.StatusRunning {
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
	if ci == nil {
		return ""
	}
	if ci.ActualSpendCents > 0 {
		return fmt.Sprintf("  Cost: $%.2f", float64(ci.ActualSpendCents)/100.0)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if ci.LaunchedAt == nil {
		return ""
	}

	launchedAt := time.Unix(*ci.LaunchedAt, 0)
	end := now
	if ci.EndedAt != nil {
		end = time.Unix(*ci.EndedAt, 0)
	}
	if end.Before(launchedAt) {
		end = launchedAt
	}

	uptime := end.Sub(launchedAt).Truncate(time.Second)
	if inst != nil && inst.CostPerHour > 0 {
		cost := uptime.Hours() * inst.CostPerHour
		return fmt.Sprintf("  Cost: $%.2f (uptime: %s)", cost, uptime)
	}
	if ci.CostPerHourCents > 0 {
		cost := uptime.Hours() * float64(ci.CostPerHourCents) / 100.0
		return fmt.Sprintf("  Cost: $%.2f (uptime: %s)", cost, uptime)
	}
	return ""
}

func updateWatchJobProgressHWM(hwm map[int64]int, prev, curr campaign.InstanceUpdate) {
	if hwm == nil {
		return
	}
	if curr.JobProgress >= 0 && curr.JobProgressID > 0 && curr.JobProgress > hwm[curr.JobProgressID] {
		hwm[curr.JobProgressID] = curr.JobProgress
	}

	runningJobs := make(map[int64]struct{}, len(curr.Jobs))
	for _, j := range curr.Jobs {
		if j.Status == db.StatusRunning {
			runningJobs[j.ID] = struct{}{}
			continue
		}
		delete(hwm, j.ID)
	}
	for _, j := range prev.Jobs {
		if _, ok := runningJobs[j.ID]; !ok {
			delete(hwm, j.ID)
		}
	}
}

func clearWatchJobProgressHWM(hwm map[int64]int, update campaign.InstanceUpdate) {
	if hwm == nil {
		return
	}
	for _, j := range update.Jobs {
		delete(hwm, j.ID)
	}
}
