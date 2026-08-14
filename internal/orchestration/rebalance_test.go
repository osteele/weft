package orchestration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
)

type capturedRebalanceSubmit struct {
	job    *db.Job
	intent *db.MoveIntent
	move   QueueRebalanceMove
}

// captureRebalanceSubmits redirects the async submission to a recorder so
// tests can assert that a move opened an intent without actually performing
// R2 work. Returns a pointer to a slice the test can inspect.
func captureRebalanceSubmits(t *testing.T) *[]capturedRebalanceSubmit {
	t.Helper()
	var captured []capturedRebalanceSubmit
	orig := submitRebalanceMoveAsyncFn
	submitRebalanceMoveAsyncFn = func(_ *sql.DB, _ *r2.Client, job *db.Job, intent *db.MoveIntent, move QueueRebalanceMove) {
		captured = append(captured, capturedRebalanceSubmit{job: job, intent: intent, move: move})
	}
	t.Cleanup(func() {
		submitRebalanceMoveAsyncFn = orig
	})
	return &captured
}

func withDefaultRebalanceDurations(t *testing.T) {
	t.Helper()
	orig := estimateRebalanceDurationsDetailed
	estimateRebalanceDurationsDetailed = func(_ *predictor.Config, batchJobs []predictor.BatchJob, _ func(string)) map[int64]estimate.DurationPrediction {
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
	queuedJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	dstQueuedJob := createQueuedLaunchJob(t, database, dstID, "A100", t.TempDir())
	runtimeDiskGB := 3
	if err := db.SetJobCLIResourceOverrides(database, dstQueuedJob, &db.CLIResourceOverrides{RuntimeDiskGB: &runtimeDiskGB}); err != nil {
		t.Fatalf("SetJobCLIResourceOverrides(dst): %v", err)
	}

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:    false,
		JobScope: map[int64]struct{}{queuedJob: {}},
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

func TestRebalanceQueuedJobsAcrossInstances_ReportsPriorityThroughputDelta(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	_ = createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning(src): %v", err)
	}
	priorityJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.SetJobPriority(database, priorityJob, 1); err != nil {
		t.Fatalf("SetJobPriority: %v", err)
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
	if result.Moves[0].JobID != priorityJob {
		t.Fatalf("moved job = %d, want priority job %d", result.Moves[0].JobID, priorityJob)
	}
	if result.Moves[0].PriorityDelta >= 0 {
		t.Fatalf("priority delta = %.3f, want negative improvement", result.Moves[0].PriorityDelta)
	}
}

func TestRankedQuickLaunchGroups_PrefersPriorityThenThroughputThenCost(t *testing.T) {
	groups := rankedQuickLaunchGroups(campaign.AutoPlacementPlan{
		LaunchGroups: []campaign.LaunchGroup{
			{JobIDs: []int64{3, 4}, CostPerHourCents: 20, Priority: 0},
			{JobIDs: []int64{2}, CostPerHourCents: 50, Priority: 1},
			{JobIDs: []int64{1}, CostPerHourCents: 10, Priority: 1},
		},
	})
	if len(groups) != 3 {
		t.Fatalf("groups len = %d, want 3", len(groups))
	}
	if groups[0].JobIDs[0] != 1 {
		t.Fatalf("first group = %+v, want cheaper priority group with job 1", groups[0])
	}
	if groups[2].JobIDs[0] != 3 {
		t.Fatalf("last group = %+v, want non-priority throughput group", groups[2])
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

func TestRebalanceQueuedJobsAcrossInstances_ApplyOpensMoveIntent(t *testing.T) {
	withDefaultRebalanceDurations(t)
	captured := captureRebalanceSubmits(t)
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
		Apply:    true,
		R2Client: &r2.Client{},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances apply: %v", err)
	}
	if len(result.Moves) != 1 {
		t.Fatalf("moves len = %d, want 1", len(result.Moves))
	}

	// Apply must write exactly one open MoveIntent for the queued job.
	intent, err := db.GetOpenMoveIntent(database, queuedJob)
	if err != nil {
		t.Fatalf("GetOpenMoveIntent: %v", err)
	}
	if intent == nil {
		t.Fatalf("expected an open MoveIntent for job %d, got none", queuedJob)
	}
	if intent.TargetKind != db.MoveTargetExisting {
		t.Fatalf("intent.TargetKind = %q, want %q", intent.TargetKind, db.MoveTargetExisting)
	}
	if intent.TargetLaunchID == nil || *intent.TargetLaunchID != dstID {
		t.Fatalf("intent.TargetLaunchID = %v, want %d", intent.TargetLaunchID, dstID)
	}
	if intent.SourceLaunchID == nil || *intent.SourceLaunchID != srcID {
		t.Fatalf("intent.SourceLaunchID = %v, want %d", intent.SourceLaunchID, srcID)
	}
	if intent.State != db.MoveIntentStateOpen {
		t.Fatalf("intent.State = %q, want open", intent.State)
	}

	// And the async submit must have been dispatched exactly once with that
	// intent and the planned destination.
	if len(*captured) != 1 {
		t.Fatalf("captured submits = %d, want 1", len(*captured))
	}
	got := (*captured)[0]
	if got.intent.ID != intent.ID {
		t.Fatalf("captured intent id = %d, want %d", got.intent.ID, intent.ID)
	}
	if got.move.ToInstanceID != dstID {
		t.Fatalf("captured move.ToInstanceID = %d, want %d", got.move.ToInstanceID, dstID)
	}
	if got.job == nil || got.job.ID != queuedJob {
		t.Fatalf("captured job mismatch")
	}
}

func TestSubmitRebalanceMoveAsyncLeavesSuccessfulIntentOpen(t *testing.T) {
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	jobID := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	dst := dstID
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &dst,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.SourceLaunchID == nil || *intent.SourceLaunchID != srcID {
		t.Fatalf("source_launch_id = %v, want %d", intent.SourceLaunchID, srcID)
	}

	done := make(chan struct{})
	origSubmit := submitJobsToInstanceForMoveFn
	submitJobsToInstanceForMoveFn = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		close(done)
		return nil
	}
	t.Cleanup(func() {
		submitJobsToInstanceForMoveFn = origSubmit
	})

	submitRebalanceMoveAsync(database, &r2.Client{}, job, intent, QueueRebalanceMove{
		JobID:          jobID,
		FromInstanceID: srcID,
		ToInstanceID:   dstID,
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submitRebalanceMoveAsync did not call submit")
	}

	got, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != db.MoveIntentStateOpen {
		t.Fatalf("move intent state = %q, want open until target ack reconciliation", got.State)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_ApplySkipsJobWithOpenIntent(t *testing.T) {
	withDefaultRebalanceDurations(t)
	captured := captureRebalanceSubmits(t)
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

	// Open an intent on the job out-of-band; rebalance must skip it.
	dst := dstID
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          queuedJob,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &dst,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:    true,
		R2Client: &r2.Client{},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances apply: %v", err)
	}
	if len(result.Moves) != 0 {
		t.Fatalf("moves len = %d, want 0 (job is already moving)", len(result.Moves))
	}
	if len(*captured) != 0 {
		t.Fatalf("captured submits = %d, want 0", len(*captured))
	}
}

func TestRebalanceQueuedJobsAcrossInstances_CoolsDownRecentFailedMoves(t *testing.T) {
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

	for i := 0; i < rebalanceMoveFailureLimit; i++ {
		intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
			JobID:          queuedJob,
			TargetKind:     db.MoveTargetExisting,
			TargetLaunchID: &dstID,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent %d: %v", i, err)
		}
		if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, "destination did not accept"); err != nil {
			t.Fatalf("ResolveMoveIntent %d: %v", i, err)
		}
	}

	callerMoving := map[int64]struct{}{}
	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:      false,
		MovingJobs: callerMoving,
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 0 {
		t.Fatalf("moves len = %d, want 0 while recent failures are cooling down", len(result.Moves))
	}
	if len(callerMoving) != 0 {
		t.Fatalf("opts.MovingJobs mutated by cooldown merge: %v", callerMoving)
	}

	old := time.Now().Add(-rebalanceMoveFailureWindow - time.Minute).Unix()
	if _, err := database.Exec(`UPDATE move_intents SET resolved_at = ? WHERE job_id = ?`, old, queuedJob); err != nil {
		t.Fatalf("age move intents: %v", err)
	}
	result, err = RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:      false,
		MovingJobs: map[int64]struct{}{},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances after cooldown: %v", err)
	}
	if len(result.Moves) != 1 || result.Moves[0].JobID != queuedJob {
		t.Fatalf("moves after cooldown = %+v, want job %d re-selected", result.Moves, queuedJob)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_SkipsConsumerWithIncompleteProducer(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python producer.py", "producer", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU producer: %v", err)
	}
	consumerID := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.SetJobNeeds(database, consumerID, []string{"output/model.pt:" + idsForTest(producerID)}); err != nil {
		t.Fatalf("SetJobNeeds: %v", err)
	}
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
	if len(result.Moves) != 0 {
		t.Fatalf("moves len = %d, want 0 because consumer waits on producer %d", len(result.Moves), producerID)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_UsesDBAccountedDestinationDisk(t *testing.T) {
	withDefaultRebalanceDurations(t)
	database := db.SetupTestDB(t)
	srcID := createRebalanceLaunch(t, database, "A100", 80, 100, 1, "")
	dstID := createRebalanceLaunch(t, database, "A100", 80, 100, 2, "")
	setLaunchDiskGBForRebalanceTest(t, database, dstID, campaign.BaseOverheadGB+2*campaign.CUDAOverheadGB+2)

	runJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, runJob); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	queuedJob := createQueuedLaunchJob(t, database, srcID, "A100", t.TempDir())
	dstDirA := t.TempDir()
	writeTorchPyprojectForRebalanceTest(t, dstDirA)
	_ = createQueuedLaunchJob(t, database, dstID, "A100", dstDirA)
	dstDirB := t.TempDir()
	writeTorchPyprojectForRebalanceTest(t, dstDirB)
	_ = createQueuedLaunchJob(t, database, dstID, "A100", dstDirB)

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:    false,
		JobScope: map[int64]struct{}{queuedJob: {}},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 0 {
		t.Fatalf("moves len = %d, want 0 because destination disk headroom is already consumed", len(result.Moves))
	}
}

func TestRebalanceQueuedJobsAcrossInstances_MovesOffDiskFailedSource(t *testing.T) {
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

	// Disk-failure history makes an instance ineligible as a move
	// destination but must not hide it as a source: it is exactly the
	// instance whose queued jobs should be movable away.
	recordDiskFullAttemptForRebalanceTest(t, database, srcID)

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:    false,
		JobScope: map[int64]struct{}{queuedJob: {}},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 1 || result.Moves[0].JobID != queuedJob || result.Moves[0].ToInstanceID != dstID {
		t.Fatalf("moves = %+v, want job %d moved off disk-failed source %d to %d", result.Moves, queuedJob, srcID, dstID)
	}
}

func TestRebalanceQueuedJobsAcrossInstances_SkipsDiskFailedDestination(t *testing.T) {
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

	recordDiskFullAttemptForRebalanceTest(t, database, dstID)

	result, err := RebalanceQueuedJobsAcrossInstances(context.Background(), database, QueueRebalanceOptions{
		Apply:    false,
		JobScope: map[int64]struct{}{queuedJob: {}},
	})
	if err != nil {
		t.Fatalf("RebalanceQueuedJobsAcrossInstances: %v", err)
	}
	if len(result.Moves) != 0 {
		t.Fatalf("moves = %+v, want 0 because the only destination has disk-failure history", result.Moves)
	}
}

func TestAppendPlacementReason_RunRateReasonSupersedesPrior(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rrtest", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	initial := []string{
		"cloud instance 3342 failed (provider_failure)",
		"run-rate headroom exhausted ($1.17/hr free, this group needs $1.27/hr)",
	}
	if err := db.SetJobPlacementReasons(database, jobID, initial); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}

	appendPlacementReason(database, jobID, "run-rate headroom exhausted ($0.51/hr free, this group needs $1.60/hr)")
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	want := []string{
		"cloud instance 3342 failed (provider_failure)",
		"run-rate headroom exhausted ($0.51/hr free, this group needs $1.60/hr)",
	}
	if strings.Join(job.PlacementReasons, "|") != strings.Join(want, "|") {
		t.Fatalf("placement_reasons = %v, want %v", job.PlacementReasons, want)
	}
}

func TestAppendPlacementReason_NonRunRateDoesNotEvictRunRate(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rrtest", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	initial := []string{
		"run-rate headroom exhausted ($0.51/hr free, this group needs $1.60/hr)",
	}
	if err := db.SetJobPlacementReasons(database, jobID, initial); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}

	appendPlacementReason(database, jobID, "no rental headroom")
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	want := []string{
		"run-rate headroom exhausted ($0.51/hr free, this group needs $1.60/hr)",
		"no rental headroom",
	}
	if strings.Join(job.PlacementReasons, "|") != strings.Join(want, "|") {
		t.Fatalf("placement_reasons = %v, want %v", job.PlacementReasons, want)
	}
}

func TestAppendPlacementReason_RepeatedReasonBecomesLatest(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "recency", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	initial := []string{
		"1 offer rejected (predicted survival 27% < 40%)",
		"Vast.ai unreachable (network or provider outage); Weft will retry",
	}
	if err := db.SetJobPlacementReasons(database, jobID, initial); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}

	appendPlacementReason(database, jobID, initial[0])
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	want := []string{initial[1], initial[0]}
	if strings.Join(job.PlacementReasons, "|") != strings.Join(want, "|") {
		t.Fatalf("placement_reasons = %v, want %v", job.PlacementReasons, want)
	}

	appendPlacementReason(database, jobID, initial[0])
	job, err = db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID after repeat: %v", err)
	}
	if strings.Join(job.PlacementReasons, "|") != strings.Join(want, "|") {
		t.Fatalf("placement_reasons after repeat = %v, want %v", job.PlacementReasons, want)
	}
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

func idsForTest(id int64) string {
	return strconv.FormatInt(id, 10)
}

func setLaunchDiskGBForRebalanceTest(t *testing.T, database *sql.DB, launchID int64, diskGB int) {
	t.Helper()
	if _, err := database.Exec(`UPDATE launches SET disk_gb = ? WHERE id = ?`, diskGB, launchID); err != nil {
		t.Fatalf("set launch disk_gb: %v", err)
	}
}

func writeTorchPyprojectForRebalanceTest(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch\"]\n"), 0o644); err != nil {
		t.Fatalf("write torch pyproject: %v", err)
	}
}

// recordDiskFullAttemptForRebalanceTest gives launchID the disk-failure
// history that campaign.FindReusableInstances screens destinations on.
func recordDiskFullAttemptForRebalanceTest(t *testing.T, database *sql.DB, launchID int64) {
	t.Helper()
	jobID := createQueuedLaunchJob(t, database, launchID, "A100", t.TempDir())
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning(disk job): %v", err)
	}
	exitCode := 1
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exitCode, time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt(disk job): %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET failure_reason = 'disk_full', launch_id = ? WHERE job_id = ?`, launchID, jobID); err != nil {
		t.Fatalf("set disk_full failure_reason: %v", err)
	}
}
