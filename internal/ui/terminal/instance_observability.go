package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
)

type observedActivity struct {
	Bootstrap string
	Phase     string
}

func formatObservedPhase(update campaign.InstanceUpdate, now time.Time) string {
	label := campaign.InstancePhaseLabel(update.InstancePhase)
	if update.PhaseChangedAt == nil {
		return label
	}
	if now.IsZero() {
		now = time.Now()
	}
	return fmt.Sprintf("%s (for %s)", label, now.Sub(*update.PhaseChangedAt).Truncate(time.Second))
}

func formatObservedActivity(update campaign.InstanceUpdate, now time.Time) observedActivity {
	activity := observedActivity{}
	ci := update.Launch
	if update.BootstrapStage != "" {
		activity.Bootstrap = campaign.BootstrapStageLabel(update.BootstrapStage)
	}
	if ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
		reconciledPhase, reconciledVerb := campaign.DisplayPhase(update.Jobs, update.InstancePhase)
		if reconciledPhase != "" {
			reconciled := update
			reconciled.InstancePhase = reconciledPhase
			if reconciledPhase != update.InstancePhase && reconciledVerb == campaign.PhaseRunning {
				if _, jobID, ok := campaign.ParsePhaseJobID(reconciledPhase); ok {
					activity.Phase = fmt.Sprintf("running job %s (observed from DB)", ids.FormatJobID(jobID))
					return activity
				}
			}
			activity.Phase = formatObservedPhase(reconciled, now)
			return activity
		}
	}
	if activity.Bootstrap != "" {
		return activity
	}

	if runningJob := observedRunningJob(update); runningJob != nil {
		activity.Phase = fmt.Sprintf("running job %s (observed from DB)", ids.FormatJobID(runningJob.ID))
		return activity
	}

	if ci == nil || campaign.IsInstanceTerminal(ci.Status) {
		return activity
	}
	if ci.EffectiveProviderID() == "" {
		activity.Bootstrap = "provisioning instance"
		return activity
	}
	if update.Instance != nil && isWatchProviderTerminal(update.Instance.Status) {
		activity.Bootstrap = "waiting for bootstrap activity (provider " + update.Instance.Status + ")"
		return activity
	}
	activity.Bootstrap = formatBootstrapWaiting(update, now)
	return activity
}

func formatBootstrapWaiting(update campaign.InstanceUpdate, now time.Time) string {
	const base = "waiting for bootstrap activity"
	ci := update.Launch
	var start time.Time
	if origin := ci.BootstrapOrigin(); origin != nil && *origin > 0 {
		start = time.Unix(*origin, 0)
	} else if ci.LaunchedAt != nil && *ci.LaunchedAt > 0 {
		start = time.Unix(*ci.LaunchedAt, 0)
	} else if ci.CreatedAt > 0 {
		start = time.Unix(ci.CreatedAt, 0)
	}
	if start.IsZero() {
		return base
	}
	if now.IsZero() {
		now = time.Now() // some TUI callers don't set opts.now
	}

	elapsed := now.Sub(start).Truncate(time.Second)
	if elapsed < time.Second {
		return base
	}
	termAfter := update.BootstrapTerminateAfter
	if termAfter == 0 {
		termAfter = campaign.BootstrapTerminateTimeout
	}
	deadlineRemaining := termAfter - elapsed
	deadlineText := fmt.Sprintf("terminate in %s", deadlineRemaining.Truncate(time.Second))
	if deadlineRemaining <= 0 {
		deadlineText = fmt.Sprintf("termination overdue by %s", (-deadlineRemaining).Truncate(time.Second))
	}

	remaining, ok := update.BootstrapDurations.ConditionalMedian(elapsed)
	if !ok {
		return fmt.Sprintf("%s (%s elapsed, %s)", base, elapsed, deadlineText)
	}
	// Cap estimate at termination deadline.
	if deadlineRemaining > 0 && remaining > deadlineRemaining {
		remaining = deadlineRemaining
	}

	return fmt.Sprintf("%s (%s elapsed, est ~%s remaining, %s)", base, elapsed, remaining.Truncate(time.Second), deadlineText)
}

func observedRunningJob(update campaign.InstanceUpdate) *db.Job {
	jobs := update.Jobs
	if update.Launch != nil {
		jobs = groupLaunchJobs(update.Launch.ID, update.Jobs).current
	}
	for _, job := range jobs {
		if job != nil && job.Status == db.StatusRunning {
			return job
		}
	}
	return nil
}

func formatUploadSummary(timings *db.JobPhaseTimings) string {
	if timings == nil {
		return ""
	}
	var parts []string
	if hasStructuredOutputUploadSummary(timings) {
		parts = append(parts, "outputs "+formatUploadStats(
			valueInt64(timings.UploadWorkspaceBytes),
			valueInt(timings.OutputUploadFiles),
			valueInt64(timings.OutputUploadDuration),
			valueInt(timings.OutputUploadRetries),
		))
	}
	if timings.UploadResultsBytes != nil || timings.ResultsUploadFiles != nil || timings.ResultsUploadDuration != nil {
		parts = append(parts, "logs "+formatUploadStats(
			valueInt64(timings.UploadResultsBytes),
			valueInt(timings.ResultsUploadFiles),
			valueInt64(timings.ResultsUploadDuration),
			valueInt(timings.ResultsUploadRetries),
		))
	}
	return strings.Join(parts, " | ")
}

func hasStructuredOutputUploadSummary(timings *db.JobPhaseTimings) bool {
	if timings == nil {
		return false
	}
	return timings.OutputUploadFiles != nil ||
		timings.OutputUploadRetries != nil ||
		timings.OutputUploadDuration != nil
}

func formatUploadStats(bytes int64, files int, durationMS int64, retries int) string {
	var parts []string
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%d files", files))
	}
	if bytes > 0 {
		parts = append(parts, formatBytesIEC(bytes))
	}
	if durationMS > 0 {
		parts = append(parts, (time.Duration(durationMS) * time.Millisecond).String())
	}
	if retries > 0 {
		parts = append(parts, fmt.Sprintf("retries=%d", retries))
	}
	if len(parts) == 0 {
		return "no data"
	}
	return strings.Join(parts, ", ")
}

func readCachedJobFailureExcerpt(jobID int64) string {
	content, err := logcache.Read(jobID)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	candidates := make([]string, 0, 4)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "===") {
			continue
		}
		if strings.HasPrefix(line, "Downloaded ") || strings.HasPrefix(line, "Preparing ") {
			continue
		}
		candidates = append([]string{line}, candidates...)
		if len(candidates) == 3 {
			break
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return truncate(strings.Join(candidates, " | "), 180)
}

func formatBytesIEC(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/1024)
}

func valueInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func valueInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
