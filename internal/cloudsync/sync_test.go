package cloudsync

import (
	"context"
	"sync"
	"sync/atomic"
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

	syncResults := func(context.Context) int {
		resultsStart = time.Now()
		time.Sleep(delay)
		resultsEnd = time.Now()
		return 0
	}

	reconciler := campaign.NewReconciler()
	start := time.Now()
	SyncState(context.Background(), database, reconciler, []cloud.Client{mockClient}, nil, syncResults)
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

// TestSyncState_PropagatesContextCancellation verifies that the ctx passed to
// SyncState is forwarded to the syncResults callback, so the inner work bails
// out when the caller's deadline expires. Regression guard: prior to ctx
// plumbing, SyncCloudJobResults built its own context.Background() and ran for
// up to ~120s regardless of caller cancellation, causing repeated wrapper-side
// timeouts to pile up overlapping syncs.
func TestSyncState_PropagatesContextCancellation(t *testing.T) {
	database := db.SetupTestDB(t)
	defer database.Close()

	var observedCancel atomic.Bool
	syncResults := func(ctx context.Context) int {
		select {
		case <-ctx.Done():
			observedCancel.Store(true)
		case <-time.After(2 * time.Second):
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	SyncState(ctx, database, nil, nil, nil, syncResults)

	if !observedCancel.Load() {
		t.Fatal("inner work did not observe ctx cancellation")
	}
}
