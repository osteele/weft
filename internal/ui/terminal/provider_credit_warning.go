package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
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
		mu      sync.Mutex
		expires time.Time
		warning string
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

// renderSharedTUIStatusLine returns a dim status line shared by TUIs.
func renderSharedTUIStatusLine(database *sql.DB, width int) string {
	status := sharedTUIStatusText(database)
	if status == "" {
		return ""
	}
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
		if visibleRunning >= 0 && globalRunning != visibleRunning {
			status = "global status: " + status
		}
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

func providerCreditWarningText() string {
	now := providerCreditWarningNow()

	providerCreditWarningCache.mu.Lock()
	if now.Before(providerCreditWarningCache.expires) {
		warning := providerCreditWarningCache.warning
		providerCreditWarningCache.mu.Unlock()
		return warning
	}
	providerCreditWarningCache.mu.Unlock()

	warning := strings.TrimSpace(providerCreditWarningFetch())

	providerCreditWarningCache.mu.Lock()
	providerCreditWarningCache.warning = warning
	providerCreditWarningCache.expires = now.Add(providerCreditWarningTTL)
	providerCreditWarningCache.mu.Unlock()
	return warning
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

	status := fmt.Sprintf("%d jobs running  ·  %d instances", runningJobs, totalInstances)
	if startingInstances > 0 {
		status = fmt.Sprintf("%d jobs running  ·  %d instances (%d up, %d starting)", runningJobs, totalInstances, runningInstances, startingInstances)
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
