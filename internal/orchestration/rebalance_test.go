package orchestration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

func withDefaultRebalanceDurations(t *testing.T) {
	t.Helper()
	orig := estimateRebalanceDurationsDetailed
	estimateRebalanceDurationsDetailed = func(_ *predictor.Config, batchJobs []predictor.BatchJob) map[int64]estimate.DurationPrediction {
		out := make(map[int64]estimate.DurationPrediction, len(batchJobs))
		for _, job := range batchJobs {
			out[job.ID] = estimate.DurationPrediction{Estimate: estimate.DefaultJobDuration}
		}
		return out
	}
	t.Cleanup(func() {
		estimateRebalanceDurationsDetailed = orig
	})
}

func TestRebalanceQueuedJobsAcrossInstances_MovesWhenSourceBlocked(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	queuedJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	dstRun := createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, dstRun); err != nil {
		t.Fatalf("MarkQueuedJobRunning(dst): %v", err)
	}

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply: false,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 1 {
		t.Fatalf("moves len = %d, want 1", len(result.Moves))
	}
	move := result.Moves[0]
	if move.JobID != queuedJob {
		t.Fatalf("move.JobID = %d, want %d", move.JobID, queuedJob)
	}
	if move.FromInstanceID != srcID || move.ToInstanceID != dstID {
		t.Fatalf("move src/dst = %d->%d, want %d->%d", move.FromInstanceID, move.ToInstanceID, srcID, dstID)
	}
	if move.ProfileID != "fast" {
		t.Fatalf("default profile = %q, want fast", move.ProfileID)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_MovesWhenScoreImprovesWithSourceIdleSlot(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 90, 2, "")

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply: false,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 1 {
		t.Fatalf("moves len = %d, want 1", len(result.Moves))
	}
}

func TestRebalanceQueuedJobsAcrossInstances_RespectsCostCeiling(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 120, 2, "")

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply: false,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 0 {
		t.Fatalf("moves len = %d, want 0 for default ceiling", len(result.Moves))
	}

	override, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:               false,
		CostCeilingOverride: 1.25,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances override: %v", err)
	}
	if len(override.Moves) != 1 {
		t.Fatalf("override moves len = %d, want 1", len(override.Moves))
	}
	if override.Moves[0].ToInstanceID != dstID {
		t.Fatalf("override dst = %d, want %d", override.Moves[0].ToInstanceID, dstID)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_TieBreaksByRemainingRental(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstShort := createRebalanceLaunch(t, database, "A100", 80, 90, 2, "")
	dstLong := createRebalanceLaunch(t, database, "A100", 80, 90, 2, "")

	if _, err := database.Exec(`UPDATE launches SET provider_running_at = ?, max_time_seconds = ? WHERE id = ?`, now-100, 300, dstShort); err != nil {
		t.Fatalf("set short remaining: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET provider_running_at = ?, max_time_seconds = ? WHERE id = ?`, now-100, 3600, dstLong); err != nil {
		t.Fatalf("set long remaining: %v", err)
	}

	queuedJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, dstShort, "A100", t.TempDir())
	_ = createQueuedLaunchJob(t, database, dstLong, "A100", t.TempDir())

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply: false,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) < 1 {
		t.Fatalf("moves len = %d, want at least 1", len(result.Moves))
	}
	if result.Moves[0].JobID != queuedJob {
		t.Fatalf("move job = %d, want %d", result.Moves[0].JobID, queuedJob)
	}
	if result.Moves[0].ToInstanceID != dstLong {
		t.Fatalf("move dst = %d, want %d (longer remaining)", result.Moves[0].ToInstanceID, dstLong)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_BalancesSevenVsOneUnderStrategies(t *testing.T) {
	cases := []struct {
		strategy string
		profile  string
	}{
		{strategy: "cheap", profile: "cheap"},
		{strategy: "balanced", profile: "tradeoff-1"},
		{strategy: "fast", profile: "fast"},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			withDefaultRebalanceDurations(t)
			database := db.SetupTestDB(t)
			srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
			dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")

			srcRun := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
			if err := db.MarkQueuedJobRunning(database, srcRun); err != nil {
				t.Fatalf("MarkQueuedJobRunning(src): %v", err)
			}
			dstRun := createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())
			if err := db.MarkQueuedJobRunning(database, dstRun); err != nil {
				t.Fatalf("MarkQueuedJobRunning(dst): %v", err)
			}
			for i := 0; i < 7; i++ {
				_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
			}
			_ = createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())

			result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
				Apply:    false,
				Strategy: tc.strategy,
			})
			if err != nil {
				t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
			}
			if len(result.Moves) < 2 {
				t.Fatalf("moves len = %d, want multiple moves for strategy %s", len(result.Moves), tc.strategy)
			}
			for _, move := range result.Moves {
				if move.FromInstanceID != srcID || move.ToInstanceID != dstID {
					t.Fatalf("move src/dst = %d->%d, want %d->%d", move.FromInstanceID, move.ToInstanceID, srcID, dstID)
				}
				if move.ProfileID != tc.profile {
					t.Fatalf("move profile = %q, want %q", move.ProfileID, tc.profile)
				}
			}
		})
	}
}

func TestRebalanceQueuedJobsAcrossInstances_UsesMultiSlotDrainModel(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")

	for i := 0; i < 2; i++ {
		jobID := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
		if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning(src): %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		jobID := createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())
		if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning(dst): %v", err)
		}
	}
	for i := 0; i < 7; i++ {
		_ = createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	}

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply: false,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) < 1 {
		t.Fatalf("moves len = %d, want multi-slot balancing moves", len(result.Moves))
	}
}

func createRebalanceLaunch(t *testing.T, database *sql.DB, gpuClass string, gpuMemGB int, costCents int, numGPUs int, dockerImage string) int64 {
	t.Helper()
	if dockerImage == "" {
		dockerImage = "nvidia/cuda:12.4.1-runtime-ubuntu22.04"
	}
	id, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUClass:         gpuClass,
		GPUMemGB:         gpuMemGB,
		CostPerHourCents: costCents,
		NumGPUs:          numGPUs,
		DockerImage:      dockerImage,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	return id
}

func createQueuedLaunchJob(t *testing.T, database *sql.DB, launchID int64, gpuClass string, dir string) int64 {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfgPath := filepath.Join(dir, ".weft.toml")
	if err := os.WriteFile(cfgPath, []byte("[cloud]\nimage = \"nvidia/cuda:12.4.1-runtime-ubuntu22.04\"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", dir, "python train.py", "rebalance", gpuClass)
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, gpuClass); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	return jobID
}
