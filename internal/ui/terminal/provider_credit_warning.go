package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	dbpkg "github.com/osteele/weft/internal/db"
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

	providerCreditWarningCache struct {
		mu          sync.Mutex
		expires     time.Time
		warning     string
		refreshing  atomic.Bool
		initialized bool
	}

	sharedTUIStatusFetch = fetchSharedTUIStatusWithCount

	sharedTUIStatusCache struct {
		mu          sync.Mutex
		expires     time.Time
		database    *sql.DB
		status      string
		runningJobs int
	}
)

// renderProviderCreditWarningLine returns a red warning line when any enabled
// provider account appears to be low on credits.
func renderProviderCreditWarningLine(width int) string {
	warning := providerCreditWarningText()
	if warning == "" {
		return ""
	}
	if width > 0 {
		warning = truncateDisplayWidth(warning, width)
	}
	return tuiFailedStyle.Render(warning)
}

// renderSharedTUIStatusLine returns a dim status line shared by TUIs,
// prefixed with "System: " so it sits next to the per-job "Job:" and
// per-host "Host:" footer lines as a sibling.
func renderSharedTUIStatusLine(database *sql.DB, width int) string {
	status := sharedTUIStatusText(database)
	if status == "" {
		return ""
	}
	status = "System: " + status
	if width > 0 {
		status = truncateDisplayWidth(status, width)
	}
	return tuiDimStyle.Render(status)
}

func renderSharedTUIStatusLines(database *sql.DB, width int) []string {
	lines := make([]string, 0, 2)
	if status := renderSharedTUIStatusLine(database, width); status != "" {
		lines = append(lines, status)
	}
	if warning := renderProviderCreditWarningLine(width); warning != "" {
		lines = append(lines, warning)
	}
	return lines
}

// renderSharedTUIStatusLinesWithVisibleRunning returns status lines, prefixing
// "global: " when the global running count differs from visibleRunning.
// Pass visibleRunning < 0 to skip the comparison (same as renderSharedTUIStatusLines).
func renderSharedTUIStatusLinesWithVisibleRunning(database *sql.DB, width int, visibleRunning int) []string {
	lines := make([]string, 0, 2)
	status, globalRunning := sharedTUIStatusTextWithCount(database)
	if status != "" {
		prefix := "System: "
		if visibleRunning >= 0 && globalRunning != visibleRunning {
			prefix = "System (global): "
		}
		status = prefix + status
		if width > 0 {
			status = truncateDisplayWidth(status, width)
		}
		lines = append(lines, tuiDimStyle.Render(status))
	}
	if warning := renderProviderCreditWarningLine(width); warning != "" {
		lines = append(lines, warning)
	}
	return lines
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

func sharedTUIStatusText(database *sql.DB) string {
	s, _ := sharedTUIStatusTextWithCount(database)
	return s
}

func sharedTUIStatusTextWithCount(database *sql.DB) (string, int) {
	if database == nil {
		return "", 0
	}

	now := providerCreditWarningNow()
	sharedTUIStatusCache.mu.Lock()
	if database == sharedTUIStatusCache.database && now.Before(sharedTUIStatusCache.expires) {
		status := sharedTUIStatusCache.status
		count := sharedTUIStatusCache.runningJobs
		sharedTUIStatusCache.mu.Unlock()
		return status, count
	}
	sharedTUIStatusCache.mu.Unlock()

	status, count := sharedTUIStatusFetch(database)
	status = strings.TrimSpace(status)

	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.database = database
	sharedTUIStatusCache.status = status
	sharedTUIStatusCache.runningJobs = count
	sharedTUIStatusCache.expires = now.Add(sharedTUIStatusTTL)
	sharedTUIStatusCache.mu.Unlock()
	return status, count
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

func fetchProviderCreditWarning() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}

	var warnings []string
	if cfg.Vastai.Enabled {
		client := vastai.NewClient()
		user, err := client.ShowUser()
		if err == nil && user != nil {
			threshold := max(defaultLowProviderCreditThreshold, cfg.Vastai.SpendingLimit)
			if user.Credit < threshold {
				warnings = append(warnings, fmt.Sprintf("WARNING: %s credits low ($%.2f < $%.2f)", cloud.ProviderVastai, user.Credit, threshold))
			}
		}
	}

	return strings.Join(warnings, " | ")
}

func fetchSharedTUIStatusWithCount(database *sql.DB) (string, int) {
	runningJobs, err := countRunningJobsInDB(database)
	if err != nil {
		return "", 0
	}
	launches, err := dbpkg.ListRunningLaunches(database)
	if err != nil {
		return "", 0
	}
	totalInstances := len(launches)
	runningInstances := totalInstances
	startingInstances := 0

	launchJobCounts, err := dbpkg.GetLaunchJobCounts(database)
	if err != nil {
		return "", 0
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

	status := fmt.Sprintf("%s running  ·  %s", pluralize(runningJobs, "job", "jobs"), pluralize(totalInstances, "instance", "instances"))
	if startingInstances > 0 {
		status = fmt.Sprintf("%s running  ·  %s (%d up, %d starting)", pluralize(runningJobs, "job", "jobs"), pluralize(totalInstances, "instance", "instances"), runningInstances, startingInstances)
	}
	if burnCentsPerHour > 0 {
		status += fmt.Sprintf("  ·  $%.2f/hr", float64(burnCentsPerHour)/100)
	}
	return status, runningJobs
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
