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

	sharedTUIStatusFetch = fetchSharedTUIStatus

	sharedTUIStatusCache struct {
		mu       sync.Mutex
		expires  time.Time
		database *sql.DB
		status   string
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
	if database == nil {
		return ""
	}

	now := providerCreditWarningNow()
	sharedTUIStatusCache.mu.Lock()
	if database == sharedTUIStatusCache.database && now.Before(sharedTUIStatusCache.expires) {
		status := sharedTUIStatusCache.status
		sharedTUIStatusCache.mu.Unlock()
		return status
	}
	sharedTUIStatusCache.mu.Unlock()

	status := strings.TrimSpace(sharedTUIStatusFetch(database))

	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.database = database
	sharedTUIStatusCache.status = status
	sharedTUIStatusCache.expires = now.Add(sharedTUIStatusTTL)
	sharedTUIStatusCache.mu.Unlock()
	return status
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

func fetchSharedTUIStatus(database *sql.DB) string {
	runningJobs, err := countRunningJobsInDB(database)
	if err != nil {
		return ""
	}
	launches, err := dbpkg.ListRunningLaunches(database)
	if err != nil {
		return ""
	}
	totalInstances := len(launches)

	var burnCentsPerHour int
	for _, l := range launches {
		burnCentsPerHour += l.CostPerHourCents
	}

	status := fmt.Sprintf("status: running jobs %d  instances %d", runningJobs, totalInstances)
	if burnCentsPerHour > 0 {
		status += fmt.Sprintf("  burn $%.2f/hr", float64(burnCentsPerHour)/100)
	}
	return status
}

func countRunningJobsInDB(database *sql.DB) (int, error) {
	var running int
	err := database.QueryRow(`
		SELECT COUNT(*)
		FROM job_status
		WHERE tombstoned = 0
		  AND status IN (?, ?, ?)
		  AND effective_target_kind != ?`,
		dbpkg.StatusRunning, dbpkg.StatusStarting, dbpkg.StatusPaused,
		string(dbpkg.JobTargetUnplaced),
	).Scan(&running)
	if err != nil {
		return 0, err
	}
	return running, nil
}
