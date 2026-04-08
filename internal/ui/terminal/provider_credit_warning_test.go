package terminal

import (
	"strings"
	"testing"

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

	status := fetchSharedTUIStatus(database)
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

	status := fetchSharedTUIStatus(database)
	if !strings.Contains(status, "3 instances") {
		t.Fatalf("status = %q, want compact instance count", status)
	}
	if strings.Contains(status, "starting") {
		t.Fatalf("status = %q, want no starting split when starting=0", status)
	}
}
