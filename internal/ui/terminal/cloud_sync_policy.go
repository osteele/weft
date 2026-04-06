package terminal

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
)

const (
	cloudSyncLeaseScope = "sync:cloud:reconcile"
	cloudSyncLeaseTTL   = 180 * time.Second
)

var (
	cloudSyncLeaseOwnerOnce sync.Once
	cloudSyncLeaseOwner     string
)

// syncCloudStateForTUI runs one cloud sync phase using the shared TUI timeout
// policy and returns user-facing warnings when the sync times out.
func syncCloudStateForTUI(database *sql.DB, full bool) []string {
	timeout := FastCloudSyncTimeout
	if full {
		timeout = NormalCloudSyncTimeout
	}

	ok, err := db.AcquireAutoLease(database, cloudSyncLeaseScope, cloudSyncOwner(), cloudSyncLeaseTTL)
	if err != nil {
		warnings := []string{fmt.Sprintf("cloud sync lease error: %v", err)}
		warnings = append(warnings, syncCloudDatabaseOnlyForTUI(database, timeout)...)
		return compactWarnings(warnings)
	}
	if !ok {
		return syncCloudDatabaseOnlyForTUI(database, timeout)
	}
	defer func() {
		_ = db.ReleaseAutoLease(database, cloudSyncLeaseScope, cloudSyncOwner())
	}()

	cfg, _ := config.Load()
	if _, completed := syncCloudStateWithTimeout(cfg, database, campaign.NewReconciler(), timeout, false); !completed {
		return []string{degraded.CloudSyncTimedOutWaitingForDB(timeout.String())}
	}
	return nil
}

// syncCloudStateTwoPhaseForTUI runs fast sync first and full sync second.
func syncCloudStateTwoPhaseForTUI(database *sql.DB) []string {
	warnings := syncCloudStateForTUI(database, false)
	warnings = append(warnings, syncCloudStateForTUI(database, true)...)
	return compactWarnings(warnings)
}

func compactWarnings(warnings []string) []string {
	if len(warnings) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(warnings))
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		warning = strings.TrimSpace(warning)
		if warning == "" {
			continue
		}
		if _, ok := seen[warning]; ok {
			continue
		}
		seen[warning] = struct{}{}
		out = append(out, warning)
	}
	return out
}

func cloudSyncOwner() string {
	cloudSyncLeaseOwnerOnce.Do(func() {
		host, _ := os.Hostname()
		if host == "" {
			host = "unknown-host"
		}
		cloudSyncLeaseOwner = fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
	})
	return cloudSyncLeaseOwner
}

// syncCloudDatabaseOnlyForTUI applies DB-local reconciliation updates without
// contacting cloud providers. This path is used when another process owns the
// cloud reconcile lease.
func syncCloudDatabaseOnlyForTUI(database *sql.DB, timeout time.Duration) []string {
	done := make(chan struct{}, 1)
	go func() {
		_ = syncCloudStateWithClients(nil, database, campaign.NewReconciler(), nil, nil, false)
		done <- struct{}{}
	}()

	if timeout <= 0 {
		<-done
		return nil
	}

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return []string{degraded.CloudSyncTimedOutWaitingForDB(timeout.String())}
	}
}
