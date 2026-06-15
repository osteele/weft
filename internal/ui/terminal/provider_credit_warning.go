package terminal

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemoncontrol"
	dbpkg "github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
)

const (
	providerCreditWarningTTL          = 30 * time.Second
	sharedTUIStatusTTL                = 10 * time.Second
	defaultLowProviderCreditThreshold = 10.0
)

var (
	providerCreditWarningNow = time.Now

	providerCreditWarningFetch = fetchProviderCreditWarning
	providerCreditWarningLoad  = config.Load
	vastaiCreditWarningUser    = func() (float64, error) {
		user, err := vastai.NewClient().ShowUser()
		if err != nil || user == nil {
			return 0, err
		}
		return user.Credit, nil
	}
	runpodCreditWarningUser = func() (float64, error) {
		user, err := runpod.NewCloudClient().ShowUser()
		if err != nil || user == nil {
			return 0, err
		}
		return user.ClientBalance, nil
	}
	daemonStatusPaths = daemoncontrol.DefaultPaths

	providerCreditWarningCache struct {
		mu          sync.Mutex
		expires     time.Time
		warning     string
		refreshing  atomic.Bool
		initialized bool
	}

	sharedTUIStatusFetch = fetchSharedTUIStatusWithCount

	sharedTUIStatusCache struct {
		mu               sync.Mutex
		expires          time.Time
		database         *sql.DB
		status           string
		runningJobs      int
		burnCentsPerHour int
		pausedLaunches   int
		refreshing       atomic.Bool
		initialized      bool
	}
)

type pausedLaunchBannerSummary struct {
	Count    int
	Statuses map[string]int
}

// renderProviderCreditWarningLine returns a red warning line when any enabled
// provider account appears to be low on credits.
func renderProviderCreditWarningLine(width int) string {
	return renderProviderCreditWarningLineView(width).line
}

// renderSharedTUIStatusLine returns a dim status line shared by TUIs,
// prefixed with "System: " so it sits next to the per-job "Job:" and
// per-host "Host:" footer lines as a sibling. When targetCents > 0 the
// auto-pilot run-rate target is appended next to the current burn rate.
func renderSharedTUIStatusLine(database *sql.DB, width int, targetCents int) string {
	return renderSystemLine(database, width, -1, targetCents)
}

func renderSharedTUIStatusLines(database *sql.DB, width int, targetCents int) []string {
	return renderSharedTUIStatusLinesWithVisibleRunning(database, width, -1, targetCents)
}

// renderSharedTUIStatusLinesWithVisibleRunning returns status lines, prefixing
// "System (global): " when the global running count differs from visibleRunning.
// Pass visibleRunning < 0 to skip the comparison.
func renderSharedTUIStatusLinesWithVisibleRunning(database *sql.DB, width int, visibleRunning int, targetCents int) []string {
	return renderSharedTUIStatusLinesView(database, width, visibleRunning, targetCents, false).lines
}

type sharedTUIStatusLinesView struct {
	lines                       []string
	daemonLineIndex             int
	daemonActionable            bool
	vastCreditWarningLineIndex  int
	vastCreditWarningActionable bool
}

func renderSharedTUIStatusLinesView(database *sql.DB, width int, visibleRunning int, targetCents int, daemonActionHint bool) sharedTUIStatusLinesView {
	lines := make([]string, 0, 3)
	view := sharedTUIStatusLinesView{daemonLineIndex: -1, vastCreditWarningLineIndex: -1}
	if daemon := renderDaemonStatusLineView(width, daemonActionHint); daemon.line != "" {
		view.daemonLineIndex = len(lines)
		view.daemonActionable = daemon.actionable
		lines = append(lines, daemon.line)
	}
	if line := renderSystemLine(database, width, visibleRunning, targetCents); line != "" {
		lines = append(lines, line)
	}
	if warning := renderProviderCreditWarningLineView(width); warning.line != "" {
		view.vastCreditWarningLineIndex = len(lines)
		view.vastCreditWarningActionable = warning.vastActionable
		lines = append(lines, warning.line)
	}
	if banner := renderPausedLaunchesBanner(database, width); banner != "" {
		lines = append(lines, banner)
	}
	view.lines = lines
	return view
}

type providerCreditWarningLineView struct {
	line           string
	vastActionable bool
}

func renderProviderCreditWarningLineView(width int) providerCreditWarningLineView {
	warning := providerCreditWarningText()
	if warning == "" {
		return providerCreditWarningLineView{}
	}
	actionable := strings.Contains(warning, "WARNING: Vast.ai credits low")
	if width > 0 {
		warning = truncateDisplayWidth(warning, width)
	}
	return providerCreditWarningLineView{line: tuiFailedStyle.Render(warning), vastActionable: actionable}
}

func renderDaemonStatusLine(width int) string {
	return renderDaemonStatusLineView(width, false).line
}

// placementDaemonStopped reports whether the placement daemon is not running,
// so the unplaced "placement pending" reason can say so. On a status read
// error it returns false (no annotation rather than a misleading one).
func placementDaemonStopped() bool {
	status, err := daemoncontrol.CurrentStatus(daemonStatusPaths())
	return err == nil && !status.Live
}

type daemonStatusLineView struct {
	line       string
	actionable bool
}

func renderDaemonStatusLineView(width int, actionHint bool) daemonStatusLineView {
	status, err := daemoncontrol.CurrentStatus(daemonStatusPaths())
	var msg string
	actionable := false
	switch {
	case err != nil:
		msg = "Daemon: status unavailable"
	case status.ActiveBinaryStale:
		actionable = true
		if actionHint {
			msg = "Daemon: stale binary; click to restart"
		} else {
			msg = "Daemon: stale binary; run `weft daemon restart`"
		}
	case status.Live:
		return daemonStatusLineView{}
	case status.Stale:
		actionable = true
		if actionHint {
			msg = "Daemon: stale; click to restart"
		} else {
			msg = "Daemon: stale"
		}
	default:
		actionable = true
		if actionHint {
			msg = "Daemon: stopped; click to start"
		} else {
			msg = "Daemon: stopped"
		}
	}
	if width > 0 {
		msg = truncateDisplayWidth(msg, width)
	}
	return daemonStatusLineView{line: tuiFailedStyle.Render(msg), actionable: actionable}
}

// renderPausedLaunchesBanner shows a warning when one or more launches are in
// the paused state. Provider-paused instances still incur storage charges; the
// provider status detail is shown when available so the banner does not guess
// whether the cause was preemption, account credit, or provider maintenance.
func renderPausedLaunchesBanner(database *sql.DB, width int) string {
	if database == nil {
		return ""
	}
	summary, err := pausedLaunchBannerSummaryForDB(database)
	if err != nil || summary.Count == 0 {
		return ""
	}
	msg := formatPausedLaunchesBanner(summary)
	if width > 0 {
		msg = truncateDisplayWidth(msg, width)
	}
	return tuiFailedStyle.Render(msg)
}

func pausedLaunchBannerSummaryForDB(database *sql.DB) (pausedLaunchBannerSummary, error) {
	summary := pausedLaunchBannerSummary{Statuses: map[string]int{}}
	rows, err := database.Query(`
		SELECT COALESCE((
			SELECT pst.new_status
			  FROM provider_status_transitions pst
			 WHERE pst.launch_id = l.id
			 ORDER BY pst.observed_at DESC, pst.id DESC
			 LIMIT 1
		), '') AS provider_status,
		       COUNT(*)
		  FROM launches l
		 WHERE l.status = ?
		 GROUP BY provider_status
		 ORDER BY provider_status`,
		dbpkg.LaunchStatusPaused,
	)
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return summary, err
		}
		if status == "" {
			status = "unknown"
		}
		summary.Count += count
		summary.Statuses[status] += count
	}
	return summary, rows.Err()
}

func formatPausedLaunchesBanner(summary pausedLaunchBannerSummary) string {
	msg := fmt.Sprintf("PAUSED: %s", pluralize(summary.Count, "instance", "instances"))
	if statusText := formatPausedProviderStatuses(summary.Statuses); statusText != "" {
		msg += " — provider reported " + statusText
	} else {
		msg += " — provider paused"
	}
	msg += " — storage may still be billable"
	if summary.Count >= 3 && summary.Statuses[cloud.ProviderStatusStopped] == summary.Count {
		msg += " — possible account/credit hold"
	}
	return msg
}

func formatPausedProviderStatuses(statusCounts map[string]int) string {
	if len(statusCounts) == 0 {
		return ""
	}
	if len(statusCounts) == 1 {
		for status := range statusCounts {
			return status
		}
	}
	statuses := make([]string, 0, len(statusCounts))
	for status, count := range statusCounts {
		statuses = append(statuses, fmt.Sprintf("%s:%d", status, count))
	}
	sort.Strings(statuses)
	return strings.Join(statuses, ", ")
}

func renderSystemLine(database *sql.DB, width int, visibleRunning int, targetCents int) string {
	base, globalRunning, burn := sharedTUIStatusTextWithCount(database)
	if base == "" {
		return ""
	}
	prefix := "System: "
	if visibleRunning >= 0 && globalRunning != visibleRunning {
		prefix = "System (global): "
	}
	status := prefix + base
	if seg := formatCostRateSegment(burn, targetCents); seg != "" {
		status += "  ·  " + seg
	}
	if width > 0 {
		status = truncateDisplayWidth(status, width)
	}
	return tuiDimStyle.Render(status)
}

func formatCostRateSegment(burnCentsPerHour, targetCents int) string {
	switch {
	case burnCentsPerHour <= 0 && targetCents <= 0:
		return ""
	case burnCentsPerHour <= 0:
		return "target " + formatAutoRunRateTarget(targetCents)
	case targetCents <= 0:
		return fmt.Sprintf("$%.2f/hr", float64(burnCentsPerHour)/100)
	default:
		return fmt.Sprintf("$%.2f/hr of %s target", float64(burnCentsPerHour)/100, formatAutoRunRateTarget(targetCents))
	}
}

// providerCreditWarningText returns the cached warning line and fires an
// async refresh when the cache is stale. The fetch hits the Vast.ai CLI and
// network, which easily blocks for a second or more — doing it synchronously
// on the View() path causes visible TUI hangs when the cache expires.
func providerCreditWarningText() string {
	now := providerCreditWarningNow()

	providerCreditWarningCache.mu.Lock()
	cached := providerCreditWarningCache.warning
	stale := !now.Before(providerCreditWarningCache.expires)
	initialized := providerCreditWarningCache.initialized
	providerCreditWarningCache.mu.Unlock()

	if stale && providerCreditWarningCache.refreshing.CompareAndSwap(false, true) {
		go refreshProviderCreditWarning()
	}
	if !initialized {
		return ""
	}
	return cached
}

func refreshProviderCreditWarning() {
	defer providerCreditWarningCache.refreshing.Store(false)
	warning := strings.TrimSpace(providerCreditWarningFetch())
	now := providerCreditWarningNow()
	providerCreditWarningCache.mu.Lock()
	providerCreditWarningCache.warning = warning
	providerCreditWarningCache.expires = now.Add(providerCreditWarningTTL)
	providerCreditWarningCache.initialized = true
	providerCreditWarningCache.mu.Unlock()
}

// sharedTUIStatusTextWithCount returns cached system-line state. View() calls
// it on every render; never block here on DB. On cache miss we serve the
// previous value and kick off an async refresh — same pattern as
// providerCreditWarningText. Until the first refresh lands, returns zero
// values (caller renders an empty system line for one tick).
func sharedTUIStatusTextWithCount(database *sql.DB) (string, int, int) {
	if database == nil {
		return "", 0, 0
	}

	now := providerCreditWarningNow()
	sharedTUIStatusCache.mu.Lock()
	stale := database != sharedTUIStatusCache.database || !now.Before(sharedTUIStatusCache.expires)
	status := sharedTUIStatusCache.status
	count := sharedTUIStatusCache.runningJobs
	burn := sharedTUIStatusCache.burnCentsPerHour
	initialized := sharedTUIStatusCache.initialized
	sharedTUIStatusCache.mu.Unlock()

	if stale && sharedTUIStatusCache.refreshing.CompareAndSwap(false, true) {
		go refreshSharedTUIStatus(database)
	}
	if !initialized {
		return "", 0, 0
	}
	return status, count, burn
}

func refreshSharedTUIStatus(database *sql.DB) {
	defer sharedTUIStatusCache.refreshing.Store(false)
	status, count, burn := sharedTUIStatusFetch(database)
	status = strings.TrimSpace(status)
	paused, _ := dbpkg.CountPausedLaunches(database)
	now := providerCreditWarningNow()

	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.database = database
	sharedTUIStatusCache.status = status
	sharedTUIStatusCache.runningJobs = count
	sharedTUIStatusCache.burnCentsPerHour = burn
	sharedTUIStatusCache.pausedLaunches = paused
	sharedTUIStatusCache.expires = now.Add(sharedTUIStatusTTL)
	sharedTUIStatusCache.initialized = true
	sharedTUIStatusCache.mu.Unlock()
}

// pluralize returns "%d singular" when n == 1, else "%d plural". The "up"
// / "starting" sub-phrase uses raw %d because the counts always appear as
// a pair where both singular and plural forms read naturally.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// lowCreditBurnHours is the runway, in hours of current burn, below which we
// raise a credit-low warning even if the absolute balance is above the
// configured floor. Catches "balance dropping faster than expected" cases.
const lowCreditBurnHours = 2.0

func fetchProviderCreditWarning() string {
	cfg, err := providerCreditWarningLoad()
	if err != nil || cfg == nil {
		return ""
	}

	var warnings []string
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderVastai) {
		credit, err := vastaiCreditWarningUser()
		if err == nil {
			threshold := max(defaultLowProviderCreditThreshold, cfg.Vastai.SpendingLimit)
			warnings = appendProviderCreditWarnings(warnings, cloud.ProviderVastai, credit, threshold)
		} else {
			warnings = appendProviderCreditCheckWarning(warnings, cloud.ProviderVastai, err)
		}
	}
	if cfg.ProviderEnabledForDiscovery(cloud.ProviderRunpod) {
		credit, err := runpodCreditWarningUser()
		if err == nil {
			threshold := max(defaultLowProviderCreditThreshold, cfg.Runpod.SpendingLimit)
			warnings = appendProviderCreditWarnings(warnings, cloud.ProviderRunpod, credit, threshold)
		} else {
			warnings = appendProviderCreditCheckWarning(warnings, cloud.ProviderRunpod, err)
		}
	}

	return strings.Join(warnings, " | ")
}

func appendProviderCreditWarnings(warnings []string, provider cloud.Provider, credit float64, threshold float64) []string {
	burnHours := burnHoursAtCurrentRate(credit)
	name := provider.DisplayName()
	if name == "" {
		name = string(provider)
	}
	switch {
	case credit < threshold:
		return append(warnings, fmt.Sprintf("WARNING: %s credits low ($%.2f < $%.2f)", name, credit, threshold))
	case burnHours > 0 && burnHours < lowCreditBurnHours:
		return append(warnings, fmt.Sprintf("WARNING: %s balance $%.2f covers ~%.1fh at current burn", name, credit, burnHours))
	default:
		return warnings
	}
}

func appendProviderCreditCheckWarning(warnings []string, provider cloud.Provider, err error) []string {
	if err == nil {
		return warnings
	}
	if isTransientProviderCreditCheckError(err) {
		return warnings
	}
	name := provider.DisplayName()
	if name == "" {
		name = string(provider)
	}
	detail := truncate(strings.TrimSpace(err.Error()), 120)
	if detail == "" {
		detail = "unknown error"
	}
	return append(warnings, fmt.Sprintf("WARNING: %s credit check failed (%s)", name, detail))
}

func isTransientProviderCreditCheckError(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "request failed:") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "temporary failure") ||
		strings.Contains(msg, "failed to resolve") ||
		strings.Contains(msg, "name resolution") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "eof")
}

// burnHoursAtCurrentRate returns the cached burn-rate runway for the given
// balance. Uses sharedTUIStatusCache.burnCentsPerHour, which is populated by
// the periodic system-line refresh. Returns 0 if no burn data is available.
func burnHoursAtCurrentRate(balanceDollars float64) float64 {
	sharedTUIStatusCache.mu.Lock()
	burnCents := sharedTUIStatusCache.burnCentsPerHour
	sharedTUIStatusCache.mu.Unlock()
	if burnCents <= 0 {
		return 0
	}
	return balanceDollars / (float64(burnCents) / 100.0)
}

func fetchSharedTUIStatusWithCount(database *sql.DB) (string, int, int) {
	runningJobs, err := countRunningJobsInDB(database)
	if err != nil {
		return "", 0, 0
	}
	queuedJobs, err := countQueuedJobsInDB(database)
	if err != nil {
		return "", 0, 0
	}
	unprocessedCompleted, unprocessedFailed, err := countUnprocessedTerminalJobs(database)
	if err != nil {
		return "", 0, 0
	}
	launches, err := dbpkg.ListRunningLaunches(database)
	if err != nil {
		return "", 0, 0
	}
	totalInstances := len(launches)
	runningInstances := totalInstances
	startingInstances := 0

	launchJobCounts, err := dbpkg.GetLaunchJobCounts(database)
	if err != nil {
		return "", 0, 0
	}
	for _, l := range launches {
		if l == nil {
			continue
		}
		if launchJobCounts[l.ID] == 0 {
			startingInstances++
		}
	}
	runningInstances = max(0, totalInstances-startingInstances)

	var burnCentsPerHour int
	for _, l := range launches {
		burnCentsPerHour += l.CostPerHourCents
	}

	jobStatus := formatSharedJobStatus(runningJobs, queuedJobs, unprocessedCompleted, unprocessedFailed)
	status := fmt.Sprintf("%s  ·  %s", jobStatus, pluralize(totalInstances, "instance", "instances"))
	if startingInstances > 0 {
		status = fmt.Sprintf("%s  ·  %s (%d up, %d starting)", jobStatus, pluralize(totalInstances, "instance", "instances"), runningInstances, startingInstances)
	}
	return status, runningJobs, burnCentsPerHour
}

func formatSharedJobStatus(running, queued, unprocessedCompleted, unprocessedFailed int) string {
	parts := []string{}
	hasJobNoun := false
	if running > 0 {
		parts, hasJobNoun = appendSharedJobSegment(parts, hasJobNoun, running, "running")
	}
	if queued > 0 {
		parts, hasJobNoun = appendSharedJobSegment(parts, hasJobNoun, queued, "queued")
	}
	if running == 0 && queued == 0 {
		if unprocessedCompleted > 0 {
			parts, hasJobNoun = appendSharedJobSegment(parts, hasJobNoun, unprocessedCompleted, "completed")
		}
		if unprocessedFailed > 0 {
			parts, hasJobNoun = appendSharedJobSegment(parts, hasJobNoun, unprocessedFailed, "failed")
		}
	}
	if len(parts) == 0 {
		return "0 jobs running"
	}
	return strings.Join(parts, " | ")
}

func appendSharedJobSegment(parts []string, hasJobNoun bool, count int, state string) ([]string, bool) {
	if !hasJobNoun {
		return append(parts, pluralize(count, "job", "jobs")+" "+state), true
	}
	return append(parts, fmt.Sprintf("%d %s", count, state)), true
}

func countRunningJobsInDB(database *sql.DB) (int, error) {
	var running int
	err := database.QueryRow(`
		SELECT COUNT(*)
		FROM job_status
		WHERE tombstoned = 0
		  AND status IN (?, ?)
		  AND effective_target_kind != ?`,
		dbpkg.StatusRunning, dbpkg.StatusStarting,
		string(dbpkg.JobTargetUnplaced),
	).Scan(&running)
	if err != nil {
		return 0, err
	}
	return running, nil
}

func countQueuedJobsInDB(database *sql.DB) (int, error) {
	var queued int
	err := database.QueryRow(`
		SELECT COUNT(*)
		FROM job_status
		WHERE tombstoned = 0
		  AND status IN (?, ?)`,
		dbpkg.StatusQueued, dbpkg.StatusPendingPlacement,
	).Scan(&queued)
	if err != nil {
		return 0, err
	}
	return queued, nil
}

func countUnprocessedTerminalJobs(database *sql.DB) (completed, failed int, err error) {
	jobs, err := dbpkg.ListUnprocessedJobs(database)
	if err != nil {
		return 0, 0, err
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch job.EffectiveStatus() {
		case dbpkg.StatusCompleted:
			completed++
		case dbpkg.StatusFailed, dbpkg.StatusDead, dbpkg.StatusKilled, dbpkg.StatusCanceled:
			failed++
		}
	}
	return completed, failed, nil
}
