package terminal

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestCountRunningJobsExcludesUnplaced(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a placed running job (on an inventory host).
	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir, placement_host) VALUES (1, 0, 'echo hi', '/tmp', 'cool30')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttempt(database, 1, "cool30", nil, db.StatusRunning); err != nil {
		t.Fatal(err)
	}

	// Create an unplaced job with running status (no host, no launch).
	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir) VALUES (2, 0, 'echo bye', '/tmp')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttempt(database, 2, "", nil, db.StatusRunning); err != nil {
		t.Fatal(err)
	}

	count, err := countRunningJobsInDB(database)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("countRunningJobsInDB = %d, want 1 (should exclude unplaced running job)", count)
	}
}

func TestFetchSharedTUIStatusSplitsRunningAndStartingInstances(t *testing.T) {
	database := db.SetupTestDB(t)

	i1, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 10})
	if err != nil {
		t.Fatalf("CreateLaunch(i1): %v", err)
	}
	i2, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching, CostPerHourCents: 20})
	if err != nil {
		t.Fatalf("CreateLaunch(i2): %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusGrace, CostPerHourCents: 30}); err != nil {
		t.Fatalf("CreateLaunch(i3): %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 40}); err != nil {
		t.Fatalf("CreateLaunch(i4): %v", err)
	}

	job1, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo one", "one", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job1): %v", err)
	}
	if err := db.SetJobLaunchID(database, job1, i1); err != nil {
		t.Fatalf("SetJobLaunchID(job1): %v", err)
	}
	job2, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo two", "two", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job2): %v", err)
	}
	if err := db.SetJobLaunchID(database, job2, i2); err != nil {
		t.Fatalf("SetJobLaunchID(job2): %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, job1); err != nil {
		t.Fatalf("MarkQueuedJobRunning(job1): %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, job2); err != nil {
		t.Fatalf("MarkQueuedJobRunning(job2): %v", err)
	}

	status, _ := fetchSharedTUIStatusWithCount(database)
	if !strings.Contains(status, "4 instances (2 up, 2 starting)") {
		t.Fatalf("status = %q, want split instance counts", status)
	}
	if !strings.Contains(status, "$1.00/hr") {
		t.Fatalf("status = %q, want cost suffix", status)
	}
}

func TestFetchSharedTUIStatusOmitsStartingWhenZero(t *testing.T) {
	database := db.SetupTestDB(t)

	i1, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 10})
	if err != nil {
		t.Fatalf("CreateLaunch(i1): %v", err)
	}
	i2, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 20})
	if err != nil {
		t.Fatalf("CreateLaunch(i2): %v", err)
	}
	i3, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 30})
	if err != nil {
		t.Fatalf("CreateLaunch(i3): %v", err)
	}

	job1, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo one", "one", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job1): %v", err)
	}
	if err := db.SetJobLaunchID(database, job1, i1); err != nil {
		t.Fatalf("SetJobLaunchID(job1): %v", err)
	}
	job2, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo two", "two", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job2): %v", err)
	}
	if err := db.SetJobLaunchID(database, job2, i2); err != nil {
		t.Fatalf("SetJobLaunchID(job2): %v", err)
	}
	job3, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo three", "three", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job3): %v", err)
	}
	if err := db.SetJobLaunchID(database, job3, i3); err != nil {
		t.Fatalf("SetJobLaunchID(job3): %v", err)
	}

	status, _ := fetchSharedTUIStatusWithCount(database)
	if !strings.Contains(status, "3 instances") {
		t.Fatalf("status = %q, want compact instance count", status)
	}
	if strings.Contains(status, "starting") {
		t.Fatalf("status = %q, want no starting split when starting=0", status)
	}
}

// resetProviderCreditWarningCacheForTest clears the package-level cache and
// the refresh-in-flight flag. Tests that touch providerCreditWarningText()
// must call this both before manipulating the fetch hook and during cleanup;
// otherwise an outstanding refresh goroutine (from an earlier test's real
// fetch) can leave refreshing=true, and a synthetic warning from this test's
// fetch can leak into the cache and poison later tests that render it.
func resetProviderCreditWarningCacheForTest(t *testing.T) {
	t.Helper()
	providerCreditWarningCache.mu.Lock()
	providerCreditWarningCache.warning = ""
	providerCreditWarningCache.expires = time.Time{}
	providerCreditWarningCache.initialized = false
	providerCreditWarningCache.mu.Unlock()
	providerCreditWarningCache.refreshing.Store(false)
}

// TestProviderCreditWarningTextDoesNotBlockOnRefresh is a regression: a stale
// cache must never block the caller on the provider fetch (which spawns the
// vastai CLI / hits the network). The fetch runs asynchronously; stale calls
// return the previously-cached value immediately.
func TestProviderCreditWarningTextDoesNotBlockOnRefresh(t *testing.T) {
	resetProviderCreditWarningCacheForTest(t)

	fetchStarted := make(chan struct{})
	fetchRelease := make(chan struct{})
	var fetchCalls atomic.Int32

	prevFetch := providerCreditWarningFetch
	providerCreditWarningFetch = func() string {
		fetchCalls.Add(1)
		close(fetchStarted)
		<-fetchRelease
		// Return empty so the in-flight goroutine can't leak a synthetic
		// warning into the shared cache.
		return ""
	}
	t.Cleanup(func() {
		providerCreditWarningFetch = prevFetch
		close(fetchRelease)
		resetProviderCreditWarningCacheForTest(t)
	})

	start := time.Now()
	got := providerCreditWarningText()
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("first call must return empty (nothing cached yet), got %q", got)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("first call blocked on fetch for %v; fetch must run async", elapsed)
	}

	select {
	case <-fetchStarted:
	case <-time.After(time.Second):
		t.Fatalf("async fetch was not kicked off")
	}
	if n := fetchCalls.Load(); n != 1 {
		t.Fatalf("fetch called %d times; want exactly 1", n)
	}
}
