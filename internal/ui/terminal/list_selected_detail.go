package terminal

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
)

// selectedJobContext bundles the ancillary data the Job and Host footer
// lines need beyond the selected job itself: live-state for rental phase /
// per-job ETA, the Launch record for rental enrichment (GPU/provider/rate/
// uptime/cost), cached inventory host info for staleness, and the full
// sibling list for "waiting for wjN" lookups on on-prem hosts.
type selectedJobContext struct {
	launchLiveByID      map[int64]*db.LaunchLiveState
	launchByID          map[int64]*db.Launch
	hostInfoByName      map[string]*db.CachedHostInfo
	cordonedHostsByName map[string]bool
	siblingJobs         []*db.Job
	// cloudConfigured is true when at least one cloud provider client is
	// available. When true, an unplaced job whose only blocker is "no local
	// host matched ..." is not actionable (autopilot is responsible for
	// finding a rental), so the on-prem rejection reason is hidden from the
	// footer to avoid presenting routine handoff state as a problem.
	cloudConfigured bool
}

// renderSelectedJobDetail returns the "Job:" and "Host:" footer lines for
// the job under the cursor. The Host line is omitted for unplaced jobs —
// their placement context is carried on the Job line. Returns nil when no
// job is selected.
//
// Fields deliberately excluded: project (already shown in the job list row),
// queue name (only one queue is in practical use), and the command / script
// tail (too long for the footer and already visible in the row).
func renderSelectedJobDetail(job *db.Job, ctx selectedJobContext, now time.Time) []string {
	if job == nil {
		return nil
	}
	var lines []string
	if s := renderJobFooterLine(job, ctx, now); s != "" {
		lines = append(lines, s)
	}
	if s := renderHostFooterLine(job, ctx, now); s != "" {
		lines = append(lines, s)
	}
	return lines
}

func renderSelectedLaunchDetail(launch *db.Launch, width int, now time.Time) []string {
	if launch == nil {
		return nil
	}
	status := campaign.DisplayInstanceStatus(launch)
	if reason := campaign.DisplayTerminationReason(launch); reason != "" {
		status += " (" + reason + ")"
	}
	head := "Instance: " + ids.FormatInstanceID(launch.ID)
	if provider := cloud.Provider(launch.Provider).DisplayName(); provider != "" {
		head += " @ " + provider
	}
	parts := []string{head}
	if status != "" {
		parts = append(parts, status)
	}
	if providerID := strings.TrimSpace(launch.EffectiveProviderID()); providerID != "" {
		parts = append(parts, "provider id "+providerID)
	}
	if brief := launch.DisplayGPUBrief(); brief != "" {
		parts = append(parts, brief)
	}
	obs := observeLaunch(launch, nil, now)
	if obs.Rate != nil {
		parts = append(parts, fmt.Sprintf("$%.2f/hr", *obs.Rate))
	}
	if obs.Uptime != nil {
		parts = append(parts, "uptime "+estimate.FormatDurationShort(*obs.Uptime))
	}
	if obs.Cost != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *obs.Cost))
	}

	lines := []string{strings.Join(parts, " · ")}
	if detail := selectedLaunchFailureDetail(launch); detail != "" {
		lines = append(lines, wrapFooterDetailLine("Failure: ", detail, width)...)
	}
	return lines
}

func selectedLaunchFailureDetail(launch *db.Launch) string {
	if launch == nil {
		return ""
	}
	reason := strings.TrimSpace(db.HumanizeTerminationReason(launch.TerminationReason))
	detail := strings.TrimSpace(launch.TerminationDetail)
	switch {
	case reason != "" && detail != "" && reason != detail:
		return reason + ": " + detail
	case detail != "":
		return detail
	case reason != "":
		return reason
	default:
		return strings.TrimSpace(launch.Status)
	}
}

func wrapFooterDetailLine(prefix, detail string, width int) []string {
	if detail == "" {
		return nil
	}
	firstPrefix := prefix
	nextPrefix := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	if width <= lipgloss.Width(firstPrefix)+8 {
		return []string{firstPrefix + detail}
	}
	var lines []string
	currentPrefix := firstPrefix
	current := currentPrefix
	for _, word := range strings.Fields(detail) {
		candidate := current
		if candidate != currentPrefix {
			candidate += " "
		}
		candidate += word
		if lipgloss.Width(candidate) > width && current != currentPrefix {
			lines = append(lines, current)
			currentPrefix = nextPrefix
			current = currentPrefix + word
			continue
		}
		current = candidate
	}
	if current != currentPrefix {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return []string{firstPrefix + detail}
	}
	return lines
}

func renderJobFooterLine(job *db.Job, ctx selectedJobContext, now time.Time) string {
	if job == nil {
		return ""
	}
	parts := []string{"Job: " + ids.FormatJobID(job.ID)}
	if job.Priority > 0 {
		parts = append(parts, fmt.Sprintf("priority %d", job.Priority))
	}
	parts = appendJobStatusParts(parts, job, ctx, now)
	if job.TargetKind() == db.JobTargetUnplaced {
		parts = appendUnplacedParts(parts, job, ctx.cloudConfigured)
	}
	if sibling := waitingForSiblingJobID(job, ctx); sibling > 0 {
		parts = append(parts, "waiting for "+ids.FormatJobID(sibling))
	}
	if remaining, ok := estimateRunningJobRemaining(job, ctx.launchLiveByID, now); ok && remaining.Mean > 0 {
		parts = append(parts, "ETA "+remaining.FormatWithBounds())
	}
	return strings.Join(parts, " · ")
}

// renderHostFooterLine returns the enriched Host line. For rental instances
// with a known Launch record: "Host: wi<id> @ <Provider> · <state> ·
// <GPU brief> · $<rate>/hr · uptime <dur> · $<cost>". For inventory hosts:
// just "Host: <name>", with an optional "· last seen <dur> ago" when cached
// host info is older than db.HostInfoStaleThreshold. Returns "" for
// unplaced or nil jobs.
func renderHostFooterLine(job *db.Job, ctx selectedJobContext, now time.Time) string {
	if job == nil {
		return ""
	}
	switch job.TargetKind() {
	case db.JobTargetInventoryHost:
		host := strings.TrimSpace(job.Host)
		if host == "" {
			return ""
		}
		parts := []string{"Host: " + host}
		if ctx.cordonedHostsByName[host] {
			parts = append(parts, "cordoned")
		}
		if info := ctx.hostInfoByName[host]; info != nil && info.LastUpdated > 0 {
			age := now.Sub(time.Unix(info.LastUpdated, 0))
			if age > db.HostInfoStaleThreshold {
				parts = append(parts, "last seen "+estimate.FormatDurationShort(age)+" ago")
			}
		}
		return strings.Join(parts, " · ")
	case db.JobTargetRentalInstance:
		return renderRentalHostLine(job, ctx, now)
	}
	return ""
}

func renderRentalHostLine(job *db.Job, ctx selectedJobContext, now time.Time) string {
	var launch *db.Launch
	if job.LaunchID != nil {
		launch = ctx.launchByID[*job.LaunchID]
	}
	providerDisplay := ""
	if launch != nil {
		providerDisplay = cloud.Provider(launch.Provider).DisplayName()
	}
	if providerDisplay == "" {
		providerDisplay = cloud.Provider(job.ProviderName()).DisplayName()
	}
	head := "Host: " + job.TargetDisplay()
	if providerDisplay != "" {
		head += " @ " + providerDisplay
	}
	parts := []string{head}
	if launch == nil {
		return strings.Join(parts, " · ")
	}
	if launch.Cordoned {
		parts = append(parts, "cordoned")
	}
	if state := launchStateLabel(launch.Status); state != "" {
		parts = append(parts, state)
	}
	if launch.InstanceType == cloud.InstanceTypeInterruptible {
		parts = append(parts, "interruptible")
	}
	if brief := launch.DisplayGPUBrief(); brief != "" {
		parts = append(parts, brief)
	}
	obs := observeLaunch(launch, nil, now)
	if obs.Rate != nil {
		parts = append(parts, fmt.Sprintf("$%.2f/hr", *obs.Rate))
	}
	if obs.Uptime != nil {
		parts = append(parts, "uptime "+estimate.FormatDurationShort(*obs.Uptime))
	}
	if obs.Cost != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *obs.Cost))
	}
	return strings.Join(parts, " · ")
}

// launchStateLabel returns the Launch.Status verbatim when it matches a
// known constant, and "" otherwise so the Host-line segment is omitted for
// empty or unrecognised values.
func launchStateLabel(status string) string {
	switch status {
	case db.LaunchStatusPlanned,
		db.LaunchStatusLaunching,
		db.LaunchStatusRunning,
		db.LaunchStatusGrace,
		db.LaunchStatusCompleted,
		db.LaunchStatusFailed,
		db.LaunchStatusCancelled:
		return status
	}
	return ""
}

// waitingForSiblingJobID returns the ID of the currently-running job that
// the selected queued/pending job is waiting for on the same target
// (instance or inventory host), or 0 when there is no such sibling or when
// the selected job is not itself queued.
//
// Rental lookup uses LaunchLiveState.JobProgressID — authoritative and
// cheap. Inventory lookup scans the siblings slice; the list TUI's jobs
// are already in-memory so this is bounded by the rendered viewport's
// parent list (typically < 100).
func waitingForSiblingJobID(job *db.Job, ctx selectedJobContext) int64 {
	if job == nil {
		return 0
	}
	if s := job.EffectiveStatus(); s != db.StatusQueued && s != db.StatusPendingPlacement {
		return 0
	}
	switch job.TargetKind() {
	case db.JobTargetRentalInstance:
		if job.LaunchID == nil {
			return 0
		}
		live := ctx.launchLiveByID[*job.LaunchID]
		if live == nil || live.JobProgressID == 0 || live.JobProgressID == job.ID {
			return 0
		}
		return live.JobProgressID
	case db.JobTargetInventoryHost:
		host := strings.TrimSpace(job.Host)
		if host == "" {
			return 0
		}
		for _, other := range ctx.siblingJobs {
			if other == nil || other.ID == job.ID {
				continue
			}
			if strings.TrimSpace(other.Host) != host {
				continue
			}
			switch other.EffectiveStatus() {
			case db.StatusRunning, db.StatusStarting:
				return other.ID
			}
		}
	}
	return 0
}

func appendJobStatusParts(parts []string, job *db.Job, ctx selectedJobContext, now time.Time) []string {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting:
		if job.StartTime > 0 {
			parts = append(parts, "elapsed "+estimate.FormatDurationShort(now.Sub(time.Unix(job.StartTime, 0))))
		}
		if cost, ok := selectedJobElapsedCost(job, ctx, now); ok {
			parts = append(parts, fmt.Sprintf("cost $%.2f", cost))
		}
	case db.StatusQueued, db.StatusPendingPlacement:
		if t := firstNonZeroTimestamp(job.QueuedAt, job.CreatedAt); t > 0 {
			parts = append(parts, "waiting "+estimate.FormatDurationShort(now.Sub(time.Unix(t, 0))))
		}
	case db.StatusCompleted, db.StatusFailed:
		parts = appendTerminalJobParts(parts, job, ctx, now)
		if job.ExitCode != nil {
			parts = append(parts, fmt.Sprintf("exit %d", *job.ExitCode))
		}
		if r := firstLine(job.FailureReason); r != "" {
			parts = append(parts, r)
		} else if m := firstLine(job.ErrorMessage); m != "" {
			parts = append(parts, m)
		}
	}
	return parts
}

func appendTerminalJobParts(parts []string, job *db.Job, ctx selectedJobContext, now time.Time) []string {
	if job.StartTime > 0 && job.EndTime != nil && *job.EndTime > job.StartTime {
		runDuration := time.Duration(*job.EndTime-job.StartTime) * time.Second
		parts = append(parts, "ran "+estimate.FormatDurationShort(runDuration))
	}
	if job.EndTime != nil && *job.EndTime > 0 {
		age := now.Sub(time.Unix(*job.EndTime, 0))
		if age >= 0 {
			parts = append(parts, "finished "+estimate.FormatDurationShort(age)+" ago")
		}
	}
	if cost, ok := selectedCompletedJobCost(job, ctx); ok {
		parts = append(parts, fmt.Sprintf("cost $%.2f", cost))
	}
	return parts
}

func selectedCompletedJobCost(job *db.Job, ctx selectedJobContext) (float64, bool) {
	if job == nil {
		return 0, false
	}
	if job.Cost != nil {
		return *job.Cost, true
	}
	if job.StartTime <= 0 || job.EndTime == nil || *job.EndTime <= job.StartTime || job.LaunchID == nil {
		return 0, false
	}
	launch := ctx.launchByID[*job.LaunchID]
	if launch == nil || launch.CostPerHourCents <= 0 {
		return 0, false
	}
	elapsed := time.Duration(*job.EndTime-job.StartTime) * time.Second
	return elapsed.Hours() * float64(launch.CostPerHourCents) / 100.0, true
}

func selectedJobElapsedCost(job *db.Job, ctx selectedJobContext, now time.Time) (float64, bool) {
	if job == nil || job.StartTime <= 0 || job.LaunchID == nil {
		return 0, false
	}
	launch := ctx.launchByID[*job.LaunchID]
	if launch == nil {
		return 0, false
	}
	obs := observeLaunch(launch, nil, now)
	if obs.Rate == nil || *obs.Rate <= 0 {
		return 0, false
	}
	elapsed := now.Sub(time.Unix(job.StartTime, 0))
	if elapsed <= 0 {
		return 0, false
	}
	return elapsed.Hours() * *obs.Rate, true
}

func appendUnplacedParts(parts []string, job *db.Job, cloudConfigured bool) []string {
	parts = append(parts, "unplaced")
	if req := formatResourceRequest(job); req != "" {
		parts = append(parts, "wants "+req)
	}
	reasons := blockreason.Reasons(job, blockreason.Options{CloudConfigured: cloudConfigured})
	if len(reasons) > 0 {
		parts = append(parts, "blocked: "+strings.Join(reasons, "; "))
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
