package cloudsync

import (
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// TestSyncState_ReconcileAndResultsParallel verifies that the two top-level
// phases of cloudsync.SyncState — ReconcileLaunches and syncResults — run
// concurrently. Serial execution would take ~2*delay; parallel ~delay.
//
// Note: ReconcileLaunches calls ListAllInstances twice per pass (batch fetch
// + orphan sweep), so we must capture only the first call's timing.
func TestSyncState_ReconcileAndResultsParallel(t *testing.T) {
	database := db.SetupTestDB(t)
	defer database.Close()

	const delay = 300 * time.Millisecond

	var (
		once           sync.Once
		firstListStart time.Time
		firstListEnd   time.Time
		resultsStart   time.Time
		resultsEnd     time.Time
	)

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			once.Do(func() {
				firstListStart = time.Now()
				time.Sleep(delay)
				firstListEnd = time.Now()
			})
			return []cloud.Instance{}, nil
		},
	}

	syncResults := func() int {
		resultsStart = time.Now()
		time.Sleep(delay)
		resultsEnd = time.Now()
		return 0
	}

	reconciler := campaign.NewReconciler()
	start := time.Now()
	SyncState(database, reconciler, []cloud.Client{mockClient}, nil, syncResults)
	elapsed := time.Since(start)

	t.Logf("elapsed=%v firstList=[%v..%v] results=[%v..%v]",
		elapsed,
		firstListStart.Sub(start), firstListEnd.Sub(start),
		resultsStart.Sub(start), resultsEnd.Sub(start),
	)

	// The first ListAllInstances window and the syncResults window must
	// overlap. If the phases were serial, one would finish before the other
	// started.
	if firstListStart.After(resultsEnd) || resultsStart.After(firstListEnd) {
		t.Fatalf("reconcile and results did not overlap: firstList=[%v..%v] results=[%v..%v]",
			firstListStart.Sub(start), firstListEnd.Sub(start),
			resultsStart.Sub(start), resultsEnd.Sub(start))
	}
}
