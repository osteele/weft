package cmd

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
)

func TestPrefilterOnPremWithProgress_ReusesSingleMetricsSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)

	job1, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py --epochs 1", "job 1", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job1): %v", err)
	}
	job2, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py --epochs 2", "job 2", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(job2): %v", err)
	}

	jobs := []*db.Job{
		{ID: job1, GPUClass: "nvidia", Command: "python train.py --epochs 1"},
		{ID: job2, GPUClass: "nvidia", Command: "python train.py --epochs 2"},
	}

	originalLoad := loadInventoryHosts
	originalCollect := collectOnPremMetrics
	originalScore := scoreOnPremHosts
	t.Cleanup(func() {
		loadInventoryHosts = originalLoad
		collectOnPremMetrics = originalCollect
		scoreOnPremHosts = originalScore
	})

	hosts := []inventory.HostSpec{
		{Name: "host-a"},
		{Name: "host-b"},
	}

	loadCalls := 0
	collectCalls := 0
	scoreCalls := 0

	loadInventoryHosts = func() ([]inventory.HostSpec, error) {
		loadCalls++
		return hosts, nil
	}
	collectOnPremMetrics = func(_ *sql.DB, names []string, timeout time.Duration) map[string]*placement.HostMetrics {
		collectCalls++
		if len(names) != 2 {
			t.Fatalf("CollectMetrics got %d hosts, want 2", len(names))
		}
		return map[string]*placement.HostMetrics{
			"host-a": {QueueDepth: 0},
		}
	}
	scoreOnPremHosts = func(_ *sql.DB, scoredHosts []inventory.HostSpec, constraints placement.Constraints, metrics map[string]*placement.HostMetrics, predict placement.JobPredictor) []placement.Score {
		scoreCalls++
		if len(scoredHosts) != 1 || scoredHosts[0].Name != "host-a" {
			t.Fatalf("ScoreHostListWithPredictor hosts = %#v, want reachable host-a only", scoredHosts)
		}
		if metrics["host-a"] == nil {
			t.Fatalf("expected live metrics for host-a")
		}
		return []placement.Score{{Host: "host-a", Eligible: true}}
	}

	remaining := prefilterOnPremWithProgress(database, jobs, nil, nil)
	if len(remaining) != 0 {
		t.Fatalf("remaining jobs = %d, want 0", len(remaining))
	}
	if loadCalls != 1 {
		t.Fatalf("loadInventoryHosts called %d times, want 1", loadCalls)
	}
	if collectCalls != 1 {
		t.Fatalf("collectOnPremMetrics called %d times, want 1", collectCalls)
	}
	if scoreCalls != len(jobs) {
		t.Fatalf("scoreOnPremHosts called %d times, want %d", scoreCalls, len(jobs))
	}

	for _, jobID := range []int64{job1, job2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("GetJobByID(%d): %v", jobID, err)
		}
		if job.Host != "host-a" {
			t.Fatalf("job %d host = %q, want host-a", jobID, job.Host)
		}
	}
}
