package orchestration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
)

func TestRebalanceQueuedRentalJobsToOnPremMovesFasterQueuedJob(t *testing.T) {
	inventory.UseTestHosts(t)
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "RTX 3080", 10, 16, 1, "")

	running := createQueuedLaunchJob(t, database, srcID, "rtx3090", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, running); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	queued := createQueuedLaunchJob(t, database, srcID, "rtx3090", t.TempDir())

	origLoad := autoPilotLoadRentalOnPremHostNames
	origCollect := autoPilotCollectRentalOnPremMetrics
	t.Cleanup(func() {
		autoPilotLoadRentalOnPremHostNames = origLoad
		autoPilotCollectRentalOnPremMetrics = origCollect
	})
	autoPilotLoadRentalOnPremHostNames = func() ([]string, error) {
		return []string{"host-alpha", "host-beta", "host-gamma"}, nil
	}
	autoPilotCollectRentalOnPremMetrics = func(_ *sql.DB, _ []string, _ time.Duration) map[string]*placement.HostMetrics {
		return map[string]*placement.HostMetrics{
			"host-alpha": {CPUPercent: 5, GPUPercent: 5},
			"host-beta":  {CPUPercent: 5, GPUPercent: 5},
			"host-gamma": {CPUPercent: 5, GPUPercent: 5},
		}
	}

	moved, err := rebalanceQueuedRentalJobsToOnPrem(context.Background(), database, &config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("rebalanceQueuedRentalJobsToOnPrem: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	job, err := db.GetJobByID(database, queued)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "host-beta" {
		t.Fatalf("job host = %q, want host-beta", job.Host)
	}
	if job.LaunchID != nil {
		t.Fatalf("launch id = %v, want nil", *job.LaunchID)
	}
}

func TestRebalanceQueuedRentalJobsToOnPremSkipsRentalTaggedJob(t *testing.T) {
	inventory.UseTestHosts(t)
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "RTX 3080", 10, 16, 1, "")

	running := createQueuedLaunchJob(t, database, srcID, "rtx3090", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, running); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	queued := createQueuedLaunchJob(t, database, srcID, "rtx3090", t.TempDir())
	if err := db.SetJobTags(database, queued, []string{db.TagRental}); err != nil {
		t.Fatalf("SetJobTags: %v", err)
	}

	origLoad := autoPilotLoadRentalOnPremHostNames
	origCollect := autoPilotCollectRentalOnPremMetrics
	t.Cleanup(func() {
		autoPilotLoadRentalOnPremHostNames = origLoad
		autoPilotCollectRentalOnPremMetrics = origCollect
	})
	autoPilotLoadRentalOnPremHostNames = func() ([]string, error) {
		return []string{"host-alpha", "host-beta", "host-gamma"}, nil
	}
	autoPilotCollectRentalOnPremMetrics = func(_ *sql.DB, _ []string, _ time.Duration) map[string]*placement.HostMetrics {
		return map[string]*placement.HostMetrics{
			"host-beta": {CPUPercent: 5, GPUPercent: 5},
		}
	}

	moved, err := rebalanceQueuedRentalJobsToOnPrem(context.Background(), database, &config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("rebalanceQueuedRentalJobsToOnPrem: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
	job, err := db.GetJobByID(database, queued)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != srcID {
		t.Fatalf("launch id = %v, want %d", job.LaunchID, srcID)
	}
}
