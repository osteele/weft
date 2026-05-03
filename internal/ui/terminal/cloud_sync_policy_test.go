package terminal

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func resetCloudSyncSingletonsForTest() {
	cloudSyncReconcilerOnce = sync.Once{}
	cloudSyncReconciler = nil
	cloudSyncLeaseOwnerOnce = sync.Once{}
	cloudSyncLeaseOwner = ""
}

func TestSyncCloudStateForTUI_ReusesReconciler(t *testing.T) {
	database := db.SetupTestDB(t)
	resetCloudSyncSingletonsForTest()

	origDeps := deps
	t.Cleanup(func() {
		deps = origDeps
		resetCloudSyncSingletonsForTest()
	})

	var seen []*campaign.Reconciler
	var syncResults []bool
	deps.SyncCloudStateWithTimeoutAndResults = func(_ *config.Config, _ *sql.DB, r *campaign.Reconciler, _ time.Duration, _ bool, sr bool) (CloudSyncResult, bool) {
		seen = append(seen, r)
		syncResults = append(syncResults, sr)
		return CloudSyncResult{}, true
	}

	if warnings := syncCloudStateForTUI(database, false); len(warnings) != 0 {
		t.Fatalf("unexpected warnings on first sync: %v", warnings)
	}
	if warnings := syncCloudStateForTUI(database, true); len(warnings) != 0 {
		t.Fatalf("unexpected warnings on second sync: %v", warnings)
	}

	if len(seen) != 2 {
		t.Fatalf("sync calls = %d, want 2", len(seen))
	}
	if seen[0] == nil || seen[1] == nil {
		t.Fatal("reconciler should never be nil")
	}
	if seen[0] != seen[1] {
		t.Fatal("expected syncCloudStateForTUI to reuse reconciler across calls")
	}
	if len(syncResults) != 2 || syncResults[0] || !syncResults[1] {
		t.Fatalf("syncResults flags = %v, want [false true]", syncResults)
	}
}

func TestSyncCloudDatabaseOnlyForTUI_DoesNotRunCloudClients(t *testing.T) {
	database := db.SetupTestDB(t)
	resetCloudSyncSingletonsForTest()

	origDeps := deps
	t.Cleanup(func() {
		deps = origDeps
		resetCloudSyncSingletonsForTest()
	})

	deps.SyncCloudStateWithClients = func(_ *config.Config, _ *sql.DB, _ *campaign.Reconciler, _ []cloud.Client, _ *r2.Client, _ bool) CloudSyncResult {
		t.Fatal("database-only TUI sync should not run cloud/R2 sync")
		return CloudSyncResult{}
	}

	if warnings := syncCloudDatabaseOnlyForTUI(database, 2*time.Second); len(warnings) != 0 {
		t.Fatalf("unexpected warnings on first sync: %v", warnings)
	}
	if warnings := syncCloudDatabaseOnlyForTUI(database, 2*time.Second); len(warnings) != 0 {
		t.Fatalf("unexpected warnings on second sync: %v", warnings)
	}
}
