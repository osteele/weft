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
	ok, err := db.AcquireAutoLease(database, cloudSyncLeaseScope, cloudSyncOwner(), cloudSyncLeaseTTL)
	if err != nil {
		return []string{fmt.Sprintf("cloud sync lease error: %v", err)}
	}
	if !ok {
		return nil
	}
	defer func() {
		_ = db.ReleaseAutoLease(database, cloudSyncLeaseScope, cloudSyncOwner())
	}()

	timeout := FastCloudSyncTimeout
	if full {
		timeout = NormalCloudSyncTimeout
	}
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
