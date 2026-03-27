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
	donorInstances           []*db.Launch // predecessor chain, most-recent-first
}

func normalizeWatchInstanceUpdate(update campaign.InstanceUpdate, ci *db.Launch) campaign.InstanceUpdate {
	if update.Launch == nil {
		update.Launch = ci
	}
	return update
}

func formatWatchInstanceBlock(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) string {
	return strings.Join(formatWatchInstanceBlockLines(update, jobProgressHWM, opts), "\n")
}

func formatWatchInstanceBlockLines(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) []string {
	ci := update.Launch
	if ci == nil {
		return nil
	}

	lines := []string{formatWatchInstanceHeaderLine(ci, update.Instance, opts)}
	lines = append(lines, formatWatchProviderLine(ci, update.Instance))
	if specLine := formatWatchInstanceSpecLine(update.Instance); specLine != "" {
		lines = append(lines, specLine)
	}

	activity := formatObservedActivity(update, opts.now)
	if activity.Bootstrap != "" {
		lines = append(lines, fmt.Sprintf("  Bootstrap: %s", activity.Bootstrap))
	}
	if activity.Phase != "" {
		lines = append(lines, fmt.Sprintf("  Phase: %s", activity.Phase))
	}
	if label := campaign.TerminationIntentLabel(update.TerminationIntent); label != "" {
		lines = append(lines, fmt.Sprintf("  Cleanup: %s", label))
		if detail := campaign.TerminationIntentDetail(update.TerminationIntent); detail != "" {
			lines = append(lines, fmt.Sprintf("  Status: %s", detail))
		}
	}
	if costLine := formatWatchInstanceCostLine(ci, update.Instance, opts.now); costLine != "" {
		lines = append(lines, costLine)
	}
	if prevLine := formatPreviousInstanceLine(opts.donorInstances, opts.now); prevLine != "" {
		lines = append(lines, prevLine)
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

	projectWidth := len("PROJECT")
	for _, job := range update.Jobs {
		if job == nil {
			continue
		}
		if w := len(campaign.JobProjectLabel(job)); w > projectWidth {
			projectWidth = w
		}
	}

	activePhaseJobID, activePhaseStatus := watchActivePhaseStatus(update.InstancePhase)
	for i, job := range update.Jobs {
		if job == nil {
			continue
		}
		desc := job.Description
		if desc == "" {
			desc = campaign.TruncateCommand(job.Command, 50)
		}

		displayStatus := displayStatuses[i]
		statusText := displayStatus
		if activePhaseJobID != 0 && job.ID == activePhaseJobID && activePhaseStatus != "" {
			displayStatus = activePhaseStatus
			statusText = activePhaseStatus
		}
		if progress := watchJobProgressPercent(update, job, displayStatus, jobProgressHWM); progress > 0 {
			statusText = fmt.Sprintf("running %3d%%", progress)
		}
		projectLabel := campaign.JobProjectLabel(job)

		lines = append(lines, fmt.Sprintf("    %4d  %s  %-*s  %s",
			job.ID,
			renderWatchJobStatusText(statusText, displayStatus, opts),
			projectWidth, projectLabel,
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

func watchActivePhaseStatus(phase string) (int64, string) {
	verb, jobID, ok := campaign.ParsePhaseJobID(phase)
	if !ok {
		return 0, ""
	}
	switch verb {
	case campaign.PhaseSetup:
		return jobID, campaign.PhaseSetup
	case campaign.PhaseRunning:
		return jobID, db.StatusRunning
	case campaign.PhaseFinalizing:
		return jobID, campaign.PhaseFinalizing
	case campaign.PhaseUploading, campaign.PhaseUploadingResults:
		return jobID, campaign.PhaseUploading
	case campaign.PhaseDiskFull:
		return jobID, db.StatusFailed
	default:
		return 0, ""
	}
}

func formatWatchInstanceHeaderLine(ci *db.Launch, inst *cloud.Instance, opts watchInstanceBlockOptions) string {
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

func watchInstanceStatusLabel(ci *db.Launch, inst *cloud.Instance) string {
	if ci == nil {
		return ""
	}
	statusLabel := ci.Status
	if inst == nil || inst.Status == "" || campaign.IsInstanceTerminal(ci.Status) {
		return statusLabel
	}
	if ci.Status == db.LaunchStatusRunning && inst.Status != "running" {
		return inst.Status
	}
	if ci.Status == db.LaunchStatusLaunching {
		return inst.Status
	}
	return statusLabel
}

func watchStatusBlockStyle(displayStatus, dbStatus string) lipgloss.Style {
	switch displayStatus {
	case "loading", "launching", "provisioning":
		return watchDimStyle
	case db.LaunchStatusCompleted:
		return watchCompletedStyle
	case db.LaunchStatusFailed, db.LaunchStatusCancelled:
		return watchFailedStyle
	}
	switch dbStatus {
	case db.LaunchStatusCompleted:
		return watchCompletedStyle
	case db.LaunchStatusFailed, db.LaunchStatusCancelled:
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

func watchJobProgressPercent(update campaign.InstanceUpdate, job *db.Job, displayStatus string, jobProgressHWM map[int64]int) int {
	if job == nil || displayStatus != db.StatusRunning {
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

func formatWatchProviderLine(ci *db.Launch, inst *cloud.Instance) string {
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

	if campaign.IsInstanceTerminal(ci.Status) {
		return line
	}
	return line + " (provisioning...)"
}

func formatWatchInstanceSpecLine(inst *cloud.Instance) string {
	if inst == nil {
		return ""
	}
	var parts []string
	if inst.CPUName != "" {
		parts = append(parts, inst.CPUName)
	}
	if inst.CPUCores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", inst.CPUCores))
	}
	if inst.RAMGB > 0 {
		parts = append(parts, fmt.Sprintf("%d GB RAM", inst.RAMGB))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("  Specs: %s", strings.Join(parts, ", "))
}

func formatWatchInstanceCostLine(ci *db.Launch, inst *cloud.Instance, now time.Time) string {
	obs := observeLaunch(ci, inst, now)
	if obs.Cost == nil {
		return ""
	}
	if obs.Uptime != nil {
		details := []string{fmt.Sprintf("uptime: %s", *obs.Uptime)}
		if obs.Rate != nil {
			details = append(details, fmt.Sprintf("rate: $%.2f/hr", *obs.Rate))
		}
		return fmt.Sprintf("  Cost: $%.2f (%s)", *obs.Cost, strings.Join(details, ", "))
	}
	return fmt.Sprintf("  Cost: $%.2f", *obs.Cost)
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

// collectDonorChain walks DonorInstanceID links from ci using the provided
// lookup function, returning the predecessor chain (most-recent-first).
func collectDonorChain(ci *db.Launch, getCI func(int64) *db.Launch) []*db.Launch {
	if ci == nil || ci.DonorInstanceID == nil {
		return nil
	}
	var chain []*db.Launch
	seen := map[int64]bool{ci.ID: true}
	donorID := ci.DonorInstanceID
	for donorID != nil {
		if seen[*donorID] {
			break
		}
		seen[*donorID] = true
		donor := getCI(*donorID)
		if donor == nil {
			break
		}
		chain = append(chain, donor)
		donorID = donor.DonorInstanceID
	}
	return chain
}

// formatPreviousInstanceLine renders a compact summary of predecessor instances.
// Single predecessor: "  Previous: Instance 226 — infra_failure, $0.01, 1m3s"
// Chain: "  Previous: Instance 227 (infra_failure) → Instance 226 (infra_failure)"
func formatPreviousInstanceLine(donors []*db.Launch, now time.Time) string {
	if len(donors) == 0 {
		return ""
	}
	if len(donors) == 1 {
		di := donors[0]
		parts := []string{di.DisplayTerminationReason()}
		obs := observeLaunch(di, nil, now)
		if obs.Cost != nil {
			parts = append(parts, fmt.Sprintf("$%.2f", *obs.Cost))
		}
		if obs.Uptime != nil {
			parts = append(parts, obs.Uptime.Truncate(time.Second).String())
		}
		return fmt.Sprintf("  Previous: Instance %d — %s", di.ID, strings.Join(parts, ", "))
	}
	summaries := make([]string, len(donors))
	for i, di := range donors {
		summaries[i] = fmt.Sprintf("Instance %d (%s)", di.ID, di.DisplayTerminationReason())
	}
	return "  Previous: " + strings.Join(summaries, " → ")
}
