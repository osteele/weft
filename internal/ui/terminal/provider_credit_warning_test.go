package terminal

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
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

	status, _, burn := fetchSharedTUIStatusWithCount(database)
	if !strings.Contains(status, "4 instances (2 up, 2 starting)") {
		t.Fatalf("status = %q, want split instance counts", status)
	}
	if burn != 100 {
		t.Fatalf("burnCentsPerHour = %d, want 100", burn)
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

	status, _, _ := fetchSharedTUIStatusWithCount(database)
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

// TestSharedTUIStatusLineHasSystemPrefix pins a regression: the shared
// status footer line is labelled "System: " so it reads as a sibling of the
// per-job "Job:" and per-host "Host:" lines in the list TUI footer.
func TestSharedTUIStatusLineHasSystemPrefix(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 10}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	// Reset cache so the renderer re-reads the fresh DB rather than a cached
	// value from a prior test, then seed it synchronously (renderer is
	// async-refresh in production).
	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.expires = time.Time{}
	sharedTUIStatusCache.initialized = false
	sharedTUIStatusCache.mu.Unlock()
	refreshSharedTUIStatus(database)

	line := renderSharedTUIStatusLine(database, 0, 0)
	plain := stripANSI(line)
	if !strings.HasPrefix(plain, "System: ") {
		t.Fatalf("expected System: prefix, got: %q", plain)
	}
}

func TestSharedTUIStatusLineWithVisibleRunning_GlobalMismatchPrefix(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 10}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	// Create one running job in the DB — "global" count is 1.
	jobID, err := db.RecordQueuedWithGPU(database, "cool30", "/tmp", "echo", "desc", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.expires = time.Time{}
	sharedTUIStatusCache.initialized = false
	sharedTUIStatusCache.mu.Unlock()
	refreshSharedTUIStatus(database)

	// Pass visibleRunning=0 so globalRunning(1) != visibleRunning(0); the
	// prefix should switch to "System (global): ".
	lines := renderSharedTUIStatusLinesWithVisibleRunning(database, 0, 0, 0)
	if len(lines) == 0 {
		t.Fatal("expected at least one status line")
	}
	plain := stripANSI(lines[0])
	if !strings.HasPrefix(plain, "System (global): ") {
		t.Fatalf("expected 'System (global): ' prefix, got: %q", plain)
	}
}

func TestRenderPausedLaunchesBannerReportsProviderStatus(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusPaused, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.RecordProviderStatus(database, launchID, time.Unix(100, 0), "running", "offline"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}

	plain := stripANSI(renderPausedLaunchesBanner(database, 0))
	if !strings.Contains(plain, "PAUSED: 1 instance") {
		t.Fatalf("banner = %q, want paused count", plain)
	}
	if !strings.Contains(plain, "provider reported offline") {
		t.Fatalf("banner = %q, want raw provider status", plain)
	}
	if strings.Contains(plain, "preempted or account credit") {
		t.Fatalf("banner = %q, should not guess the cause", plain)
	}
}

func TestRenderPausedLaunchesBannerPossibleCreditHoldOnlyForStoppedFleet(t *testing.T) {
	database := db.SetupTestDB(t)
	for i := 0; i < 3; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusPaused, Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch %d: %v", i, err)
		}
		if err := db.RecordProviderStatus(database, launchID, time.Unix(int64(100+i), 0), "running", "stopped"); err != nil {
			t.Fatalf("RecordProviderStatus %d: %v", i, err)
		}
	}

	plain := stripANSI(renderPausedLaunchesBanner(database, 0))
	if !strings.Contains(plain, "provider reported stopped") {
		t.Fatalf("banner = %q, want stopped status", plain)
	}
	if !strings.Contains(plain, "possible account/credit hold") {
		t.Fatalf("banner = %q, want fleet stopped hint", plain)
	}
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

func TestFetchProviderCreditWarningIncludesRunpod(t *testing.T) {
	prevLoad := providerCreditWarningLoad
	prevRunpodUser := runpodCreditWarningUser
	prevVastUser := vastaiCreditWarningUser
	providerCreditWarningLoad = func() (*config.Config, error) {
		cfg := config.DefaultConfig()
		cfg.Runpod.Enabled = true
		cfg.Runpod.SpendingLimit = 20
		cfg.Vastai.Enabled = false
		return cfg, nil
	}
	runpodCreditWarningUser = func() (float64, error) { return 5, nil }
	vastaiCreditWarningUser = func() (float64, error) {
		t.Fatal("vastai user fetch should not run when disabled")
		return 0, nil
	}
	t.Cleanup(func() {
		providerCreditWarningLoad = prevLoad
		runpodCreditWarningUser = prevRunpodUser
		vastaiCreditWarningUser = prevVastUser
	})

	got := fetchProviderCreditWarning()
	if !strings.Contains(got, "RunPod credits low ($5.00 < $20.00)") {
		t.Fatalf("fetchProviderCreditWarning() = %q", got)
	}
}

func TestFetchProviderCreditWarningIncludesVastaiCreditCheckFailure(t *testing.T) {
	prevLoad := providerCreditWarningLoad
	prevRunpodUser := runpodCreditWarningUser
	prevVastUser := vastaiCreditWarningUser
	providerCreditWarningLoad = func() (*config.Config, error) {
		cfg := config.DefaultConfig()
		cfg.Vastai.Enabled = true
		cfg.Runpod.Enabled = false
		return cfg, nil
	}
	vastaiCreditWarningUser = func() (float64, error) {
		return 0, errors.New("show user: owner: Extra inputs are not permitted")
	}
	runpodCreditWarningUser = func() (float64, error) {
		t.Fatal("runpod user fetch should not run when disabled")
		return 0, nil
	}
	t.Cleanup(func() {
		providerCreditWarningLoad = prevLoad
		runpodCreditWarningUser = prevRunpodUser
		vastaiCreditWarningUser = prevVastUser
	})

	got := fetchProviderCreditWarning()
	if !strings.Contains(got, "Vast.ai credit check failed") || !strings.Contains(got, "owner: Extra inputs are not permitted") {
		t.Fatalf("fetchProviderCreditWarning() = %q", got)
	}
}

func TestFetchProviderCreditWarningSuppressesTransientVastaiCreditCheckFailure(t *testing.T) {
	prevLoad := providerCreditWarningLoad
	prevRunpodUser := runpodCreditWarningUser
	prevVastUser := vastaiCreditWarningUser
	providerCreditWarningLoad = func() (*config.Config, error) {
		cfg := config.DefaultConfig()
		cfg.Vastai.Enabled = true
		cfg.Runpod.Enabled = false
		return cfg, nil
	}
	vastaiCreditWarningUser = func() (float64, error) {
		return 0, errors.New(`show user: request failed: Get "https://console.vast.ai/api/v0/users/current/": context deadline exceeded`)
	}
	runpodCreditWarningUser = func() (float64, error) {
		t.Fatal("runpod user fetch should not run when disabled")
		return 0, nil
	}
	t.Cleanup(func() {
		providerCreditWarningLoad = prevLoad
		runpodCreditWarningUser = prevRunpodUser
		vastaiCreditWarningUser = prevVastUser
	})

	got := fetchProviderCreditWarning()
	if got != "" {
		t.Fatalf("fetchProviderCreditWarning() = %q, want no warning for transient request failure", got)
	}
}

func TestFormatCostRateSegment(t *testing.T) {
	cases := []struct {
		burn   int
		target int
		want   string
	}{
		{0, 0, ""},
		{123, 0, "$1.23/hr"},
		{0, 500, "target $5.00/hr"},
		{123, 500, "$1.23/hr of $5.00/hr target"},
	}
	for _, tc := range cases {
		got := formatCostRateSegment(tc.burn, tc.target)
		if got != tc.want {
			t.Errorf("formatCostRateSegment(%d, %d) = %q, want %q", tc.burn, tc.target, got, tc.want)
		}
	}
}

func TestPluralize(t *testing.T) {
	cases := []struct {
		n        int
		singular string
		plural   string
		want     string
	}{
		{0, "job", "jobs", "0 jobs"},
		{1, "job", "jobs", "1 job"},
		{2, "job", "jobs", "2 jobs"},
		{1, "instance", "instances", "1 instance"},
	}
	for _, tc := range cases {
		if got := pluralize(tc.n, tc.singular, tc.plural); got != tc.want {
			t.Errorf("pluralize(%d, %q, %q) = %q, want %q", tc.n, tc.singular, tc.plural, got, tc.want)
		}
	}
}

// TestSharedTUIStatusSingularises pins the user-visible "1 job running · 1 instance"
// form when counts are exactly 1.
func TestSharedTUIStatusSingularises(t *testing.T) {
	database := db.SetupTestDB(t)
	i1, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, CostPerHourCents: 14})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo one", "one", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, i1); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	status, _, _ := fetchSharedTUIStatusWithCount(database)
	if !strings.Contains(status, "1 job running") {
		t.Errorf("expected '1 job running', got: %s", status)
	}
	if !strings.Contains(status, "1 instance") {
		t.Errorf("expected '1 instance' (singular), got: %s", status)
	}
	if strings.Contains(status, "1 jobs") || strings.Contains(status, "1 instances") {
		t.Errorf("plural form at count=1, got: %s", status)
	}
}

func TestFormatSharedJobStatusGrammar(t *testing.T) {
	cases := []struct {
		name                 string
		running              int
		queued               int
		unprocessedCompleted int
		unprocessedFailed    int
		want                 string
	}{
		{"running and queued", 2, 30, 0, 0, "2 jobs running | 30 queued"},
		{"single running", 1, 0, 0, 0, "1 job running"},
		{"queued first", 0, 30, 0, 0, "30 jobs queued"},
		{"completed first", 0, 0, 3, 0, "3 jobs completed"},
		{"single failed first", 0, 0, 0, 1, "1 job failed"},
		{"completed and failed", 0, 0, 2, 1, "2 jobs completed | 1 failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSharedJobStatus(tc.running, tc.queued, tc.unprocessedCompleted, tc.unprocessedFailed)
			if got != tc.want {
				t.Fatalf("formatSharedJobStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}
