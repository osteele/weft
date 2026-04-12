package placement

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/predictor"
)

func TestPrefilterOnPrem_ReusesSingleMetricsSnapshot(t *testing.T) {
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
	collectOnPremMetrics = func(_ *sql.DB, names []string, timeout time.Duration) map[string]*HostMetrics {
		collectCalls++
		if len(names) != 2 {
			t.Fatalf("CollectMetrics got %d hosts, want 2", len(names))
		}
		return map[string]*HostMetrics{
			"host-a": {QueueDepth: 0},
		}
	}
	scoreOnPremHosts = func(_ *sql.DB, scoredHosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) []Score {
		scoreCalls++
		if len(scoredHosts) != 1 || scoredHosts[0].Name != "host-a" {
			t.Fatalf("ScoreHostListWithPredictor hosts = %#v, want reachable host-a only", scoredHosts)
		}
		if metrics["host-a"] == nil {
			t.Fatalf("expected live metrics for host-a")
		}
		return []Score{{Host: "host-a", Eligible: true}}
	}

	remaining := PrefilterOnPrem(database, jobs, nil, PrefilterCallbacks{})
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

func TestPrefilterOnPrem_BatchesPredictorCallsAcrossHostsAndJobs(t *testing.T) {
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
	originalResolveBatch := resolvePredictBatchForOnPrem
	originalBuildPredictor := buildJobPredictorFromConfig
	t.Cleanup(func() {
		loadInventoryHosts = originalLoad
		collectOnPremMetrics = originalCollect
		scoreOnPremHosts = originalScore
		resolvePredictBatchForOnPrem = originalResolveBatch
		buildJobPredictorFromConfig = originalBuildPredictor
	})

	hosts := []inventory.HostSpec{
		{Name: "host-a"},
		{Name: "host-b"},
	}

	loadInventoryHosts = func() ([]inventory.HostSpec, error) {
		return hosts, nil
	}
	collectOnPremMetrics = func(_ *sql.DB, names []string, timeout time.Duration) map[string]*HostMetrics {
		return map[string]*HostMetrics{
			"host-a": {QueueDepth: 0},
			"host-b": {QueueDepth: 0},
		}
	}

	batchCalls := 0
	resolvePredictBatchForOnPrem = func(_ predictor.Config, batchJobs []predictor.BatchJob) (map[int64]*predictor.Result, error) {
		batchCalls++
		if len(batchJobs) != len(jobs)*len(hosts) {
			t.Fatalf("ResolvePredictBatch batch size = %d, want %d", len(batchJobs), len(jobs)*len(hosts))
		}
		results := make(map[int64]*predictor.Result, len(batchJobs))
		for _, batchJob := range batchJobs {
			results[batchJob.ID] = &predictor.Result{
				DurationS: &predictor.Prediction{Mean: 60, Lower: 50, Upper: 70},
			}
		}
		return results, nil
	}
	buildJobPredictorFromConfig = func(_ *config.Config, _ Constraints) JobPredictor {
		t.Fatalf("unexpected fallback single-host predictor build")
		return nil
	}

	scoreOnPremHosts = func(_ *sql.DB, scoredHosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) []Score {
		if len(scoredHosts) != len(hosts) {
			t.Fatalf("ScoreHostListWithPredictor hosts = %d, want %d", len(scoredHosts), len(hosts))
		}
		for _, host := range hosts {
			prediction := predict(host.Name)
			if prediction == nil || prediction.DurationS == nil || *prediction.DurationS != 60 {
				t.Fatalf("missing cached prediction for host %s: %+v", host.Name, prediction)
			}
		}
		return []Score{{Host: "host-a", Eligible: true}}
	}

	remaining := PrefilterOnPrem(database, jobs, &config.Config{Predictor: config.PredictorConfig{
		ProjectPath: "/tmp/job-estimator",
		ModelDir:    "/tmp/models",
	}}, PrefilterCallbacks{})
	if len(remaining) != 0 {
		t.Fatalf("remaining jobs = %d, want 0", len(remaining))
	}
	if batchCalls != 1 {
		t.Fatalf("ResolvePredictBatch calls = %d, want 1", batchCalls)
	}
}
