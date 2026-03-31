package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/progress"
)

type watchInstanceBlockOptions struct {
	plain                    bool
	spinner                  string
	showSpinnerIfNonTerminal bool
	resolvedJobsOverride     *int
	dimJobStatuses           bool
	now                      time.Time
	predecessors             []*db.Launch // replacement chain, most-recent-first
}

func normalizeWatchInstanceUpdate(update campaign.InstanceUpdate, ci *db.Launch) campaign.InstanceUpdate {
	if update.Launch == nil {
		update.Launch = ci
	}
	return update
}

// watchInstanceLine is a single line in a rendered instance block.
// jobID is non-zero for lines that represent a job row.
type watchInstanceLine struct {
	text  string
	jobID int64
}

func watchInstanceLineTexts(lines []watchInstanceLine) []string {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.text
	}
	return texts
}

func formatWatchInstanceBlock(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) string {
	return strings.Join(watchInstanceLineTexts(formatWatchInstanceBlockStructured(update, jobProgressHWM, opts)), "\n")
}

func formatWatchInstanceBlockStructured(update campaign.InstanceUpdate, jobProgressHWM map[int64]int, opts watchInstanceBlockOptions) []watchInstanceLine {
	ci := update.Launch
	if ci == nil {
		return nil
	}

	lines := []watchInstanceLine{{text: formatWatchInstanceHeaderLine(ci, update.Instance, opts)}}
	addLine := func(text string) { lines = append(lines, watchInstanceLine{text: text}) }
	addJobLine := func(text string, jobID int64) {
		lines = append(lines, watchInstanceLine{text: text, jobID: jobID})
	}

	addLine(formatWatchProviderLine(ci, update.Instance))
	if specLine := formatWatchInstanceSpecLine(update.Instance); specLine != "" {
		addLine(specLine)
	}

	activity := formatObservedActivity(update, opts.now)
	if activity.Bootstrap != "" {
		addLine(fmt.Sprintf("  Bootstrap: %s", activity.Bootstrap))
	}
	if activity.Phase != "" {
		addLine(fmt.Sprintf("  Phase: %s", activity.Phase))
	}
	if label := campaign.TerminationIntentLabel(update.TerminationIntent); label != "" {
		addLine(fmt.Sprintf("  Cleanup: %s", label))
		if detail := campaign.TerminationIntentDetail(update.TerminationIntent); detail != "" {
			addLine(fmt.Sprintf("  Status: %s", detail))
		}
	}
	if costLine := formatWatchInstanceCostLine(ci, update.Instance, opts.now); costLine != "" {
		addLine(costLine)
	}
	if prevLine := formatPreviousInstanceLine(opts.predecessors, opts.now); prevLine != "" {
		addLine(prevLine)
	}

	if len(update.Jobs) == 0 {
		return lines
	}

	displayStatuses := make([]string, len(update.Jobs))
	resolved := 0
	for i, job := range update.Jobs {
		displayStatuses[i] = campaign.AttemptDisplayStatus(job, update.JobAttemptOutcomes)
		if campaign.IsJobTerminal(displayStatuses[i]) {
			resolved++
		}
	}
	if opts.resolvedJobsOverride != nil {
		resolved = *opts.resolvedJobsOverride
	}
	addLine(fmt.Sprintf("  Jobs: %d/%d resolved", resolved, len(update.Jobs)))

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
		if activePhaseJobID != 0 && job.ID == activePhaseJobID && activePhaseStatus != "" && !campaign.IsJobTerminal(displayStatus) {
			displayStatus = activePhaseStatus
			statusText = activePhaseStatus
		}
		if progressText := watchJobProgressText(update, job, displayStatus, jobProgressHWM); progressText != "" {
			statusText = progressText
		}
		projectLabel := campaign.JobProjectLabel(job)

		addJobLine(fmt.Sprintf("    %4d  %s  %-*s  %s",
			job.ID,
			renderWatchJobStatusText(statusText, displayStatus, opts),
			projectWidth, projectLabel,
			desc,
		), job.ID)
		if campaign.IsJobTerminal(displayStatuses[i]) && activePhaseJobID == job.ID &&
			(activePhaseStatus == campaign.PhaseUploading || activePhaseStatus == campaign.PhaseFinalizing) {
			if summary := formatUploadSummary(update.JobPhaseTimings[job.ID]); summary != "" {
				addLine(fmt.Sprintf("          uploads: %s", summary))
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
		statusLabel += " (" + ci.DisplayTerminationReason() + ")"
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
	if inst == nil || inst.Status == "" || campaign.IsInstanceTerminal(ci.Status) {
		return ci.Status
	}
	// When DB says "running" but provider died, annotate so the user sees it
	// before the reconciler catches up.
	if ci.Status == db.LaunchStatusRunning && isWatchProviderTerminal(inst.Status) {
		return ci.Status + " (" + inst.Status + ")"
	}
	if ci.Status == db.LaunchStatusLaunching {
		return inst.Status
	}
	return ci.Status
}

func isWatchProviderTerminal(status string) bool {
	switch status {
	case cloud.ProviderStatusExited, cloud.ProviderStatusStopped,
		cloud.ProviderStatusError, cloud.ProviderStatusDestroyed,
		cloud.ProviderStatusDead:
		return true
	}
	return false
}

func watchStatusBlockStyle(displayStatus, dbStatus string) lipgloss.Style {
	switch displayStatus {
	case cloud.ProviderStatusLoading, "launching", "provisioning":
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

func watchJobProgressText(update campaign.InstanceUpdate, job *db.Job, displayStatus string, jobProgressHWM map[int64]int) string {
	if job == nil || displayStatus != db.StatusRunning {
		return ""
	}
	rawPct := -1
	phase := 0
	if jobProgressHWM != nil && jobProgressHWM[job.ID] > 0 {
		rawPct = jobProgressHWM[job.ID]
	}
	if update.JobProgressID == job.ID && update.JobProgress > 0 {
		rawPct = update.JobProgress
		phase = update.JobProgressPhase
	}
	if pctText := progress.FormatPhaseProgress(phase, rawPct); pctText != "" {
		return "running " + pctText
	}
	return ""
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
	if curr.JobProgress >= 0 && curr.JobProgressID > 0 && curr.JobProgress != hwm[curr.JobProgressID] {
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

// collectReplacementChain walks ReplacedInstanceID links from ci using the
// provided lookup function, returning the predecessor chain (most-recent-first).
func collectReplacementChain(ci *db.Launch, getCI func(int64) *db.Launch) []*db.Launch {
	if ci == nil || ci.ReplacedInstanceID == nil {
		return nil
	}
	var chain []*db.Launch
	seen := map[int64]bool{ci.ID: true}
	replacedID := ci.ReplacedInstanceID
	for replacedID != nil {
		if seen[*replacedID] {
			break
		}
		seen[*replacedID] = true
		predecessor := getCI(*replacedID)
		if predecessor == nil {
			break
		}
		chain = append(chain, predecessor)
		replacedID = predecessor.ReplacedInstanceID
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
