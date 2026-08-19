package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	weftlogging "github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/r2"
)

func TestSyncInstanceState_GraceToRunning(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create instance in grace status with an expired deadline
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	pastDeadline := time.Now().Add(-1 * time.Minute).Unix()
	if err := db.SetLaunchGraceStarted(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set grace: %v", err)
	}

	// Verify it's in grace
	ci, _ := db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusGrace {
		t.Fatalf("status = %q, want grace", ci.Status)
	}

	// Mock R2 fetches: phase is "running:544" (agent picked up a resubmitted job)
	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return "running:544"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		return ""
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckR2GraceStatus = checkR2GraceStatus

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	// Instance should have exited grace
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("ci.Status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
	if synced.InstancePhase != "running:544" {
		t.Errorf("InstancePhase = %q, want %q", synced.InstancePhase, "running:544")
	}

	// Verify DB was updated
	ci, _ = db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("DB status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
	if ci.GraceDeadline != nil {
		t.Errorf("GraceDeadline should be nil, got %v", *ci.GraceDeadline)
	}
}

func TestConfirmMoveIntentsForReadyLaunchStopsCloudSource(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, command, tombstoned)
		 VALUES (101, '/tmp', 'RTX_3090', 'python train.py', 0)`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.SetJobLaunchID(database, 101, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, 101)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          101,
		TargetKind:     db.MoveTargetNew,
		TargetLaunchID: &targetLaunch,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, 101, "", &targetLaunch, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}

	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() { sendGraceCancelAttempts = prevSendCancel })
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, launchID int64, attemptIDs []int64) error {
		canceledByLaunch[launchID] = append(canceledByLaunch[launchID], attemptIDs...)
		return nil
	}

	confirmMoveIntentsForReadyLaunch(context.Background(), database, &r2.Client{}, targetLaunch)

	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateConfirmed {
		t.Fatalf("intent state = %q, want confirmed", gotIntent.State)
	}
	if got := canceledByLaunch[sourceLaunch]; len(got) != 1 || got[0] != sourceAttemptID {
		t.Fatalf("source cancel-attempts = %v, want [%d]", canceledByLaunch, sourceAttemptID)
	}
	var abandonedReason string
	if err := database.QueryRow(`SELECT abandoned_reason FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&abandonedReason); err != nil {
		t.Fatalf("read source abandoned reason: %v", err)
	}
	if abandonedReason != db.AttemptAbandonedMoveTargetAccepted {
		t.Fatalf("source abandoned reason = %q, want %q", abandonedReason, db.AttemptAbandonedMoveTargetAccepted)
	}
}

func TestSyncInstanceState_RefreshesJobsBeforeDisplayPhase(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "L40",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set launch: %v", err)
	}
	staleJobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("get stale jobs: %v", err)
	}
	if len(staleJobs) != 1 || staleJobs[0].Status != db.StatusQueued {
		t.Fatalf("stale jobs = %+v, want one queued job", staleJobs)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origOnStart := syncFetchOnStartStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchOnStartStage = origOnStart
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "ready" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchOnStartStage = func(_ context.Context, _ *r2.Client, _ int64) (string, *time.Time) { return "", nil }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, staleJobs, JobState{}, SyncInstanceStateOpts{AgentVersionFetched: true})
	want := fmt.Sprintf("running:%d", jobID)
	if synced.InstancePhase != want {
		t.Fatalf("InstancePhase = %q, want %q", synced.InstancePhase, want)
	}
	live, err := db.GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if live == nil || live.InstancePhase != want {
		t.Fatalf("stored live state = %+v, want phase %q", live, want)
	}
}

func TestSyncInstanceState_PhaseConfirmsHiddenMoveTarget(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "runpod", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, command, tombstoned)
		 VALUES (104, '/tmp', 'RTX_3090', 'python train.py', 0)`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.SetJobLaunchID(database, 104, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, 104)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          104,
		TargetKind:     db.MoveTargetNew,
		TargetLaunchID: &targetLaunch,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, 104, "", &targetLaunch, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}

	ci, err := db.GetLaunch(database, targetLaunch)
	if err != nil {
		t.Fatalf("GetLaunch target: %v", err)
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		sendGraceCancelAttempts = prevSendCancel
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return "running:104"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		return ""
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 104, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, launchID int64, attemptIDs []int64) error {
		canceledByLaunch[launchID] = append(canceledByLaunch[launchID], attemptIDs...)
		return nil
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	if synced.JobsUpdated != 1 {
		t.Fatalf("JobsUpdated = %d, want 1", synced.JobsUpdated)
	}
	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateConfirmed {
		t.Fatalf("intent state = %q, want confirmed", gotIntent.State)
	}
	job, err := db.GetJobByID(database, 104)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("job status = %q, want running", job.Status)
	}
	if job.LaunchID == nil || *job.LaunchID != targetLaunch {
		t.Fatalf("job launch = %v, want %d", job.LaunchID, targetLaunch)
	}
	if job.LatestRunID == nil || *job.LatestRunID != targetAttemptID {
		t.Fatalf("latest run = %v, want target attempt %d", job.LatestRunID, targetAttemptID)
	}
	if got := canceledByLaunch[sourceLaunch]; len(got) != 1 || got[0] != sourceAttemptID {
		t.Fatalf("source cancel-attempts = %v, want [%d]", canceledByLaunch, sourceAttemptID)
	}
	ci, err = db.GetLaunch(database, targetLaunch)
	if err != nil {
		t.Fatalf("GetLaunch refreshed: %v", err)
	}
	if ci.AgentReadyAtUnix == nil {
		t.Fatal("agent_ready_at_unix was not recorded")
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("read source abandoned reason: %v", err)
	}
	if sourceReason != db.AttemptAbandonedMoveTargetAccepted {
		t.Fatalf("source abandoned reason = %q, want %q", sourceReason, db.AttemptAbandonedMoveTargetAccepted)
	}
}

func TestReconcilePendingMoveTargetRequestAckConfirmsAndStopsSource(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, command, tombstoned)
		 VALUES (102, '/tmp', 'RTX_3090', 'python train.py', 0)`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.SetJobLaunchID(database, 102, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, 102)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          102,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &targetLaunch,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, 102, "", &targetLaunch, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.SetMoveIntentTargetRequest(database, intent.ID, string(controlplane.GraceCommandJobs), "req-accepted"); err != nil {
		t.Fatalf("SetMoveIntentTargetRequest: %v", err)
	}

	prevCheckAck := syncCheckGraceCommandAck
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() {
		syncCheckGraceCommandAck = prevCheckAck
		sendGraceCancelAttempts = prevSendCancel
	})
	syncCheckGraceCommandAck = func(_ context.Context, _ controlplane.GraceStore, instanceID int64, requestID string) (*controlplane.GraceCommandAck, bool, error) {
		if instanceID != targetLaunch || requestID != "req-accepted" {
			t.Fatalf("ack lookup = (%d, %q), want (%d, req-accepted)", instanceID, requestID, targetLaunch)
		}
		return &controlplane.GraceCommandAck{Kind: controlplane.GraceCommandJobs, Accepted: true}, true, nil
	}
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, launchID int64, attemptIDs []int64) error {
		canceledByLaunch[launchID] = append(canceledByLaunch[launchID], attemptIDs...)
		return nil
	}

	reconcilePendingMoveTargetRequestAcks(context.Background(), database, &r2.Client{}, targetLaunch)

	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateConfirmed {
		t.Fatalf("intent state = %q, want confirmed", gotIntent.State)
	}
	if got := canceledByLaunch[sourceLaunch]; len(got) != 1 || got[0] != sourceAttemptID {
		t.Fatalf("source cancel-attempts = %v, want [%d]", canceledByLaunch, sourceAttemptID)
	}
	var targetReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&targetReason); err != nil {
		t.Fatalf("read target abandoned reason: %v", err)
	}
	if targetReason != "" {
		t.Fatalf("target abandoned reason = %q, want authoritative target", targetReason)
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("read source abandoned reason: %v", err)
	}
	if sourceReason != db.AttemptAbandonedMoveTargetAccepted {
		t.Fatalf("source abandoned reason = %q, want %q", sourceReason, db.AttemptAbandonedMoveTargetAccepted)
	}
}

func TestReconcilePendingMoveTargetRequestAckRejectedCancelsMove(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	sourceLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, gpu_class, command, tombstoned)
		 VALUES (103, '/tmp', 'RTX_3090', 'python train.py', 0)`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.SetJobLaunchID(database, 103, sourceLaunch); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, 103)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          103,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &targetLaunch,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, 103, "", &targetLaunch, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.SetMoveIntentTargetRequest(database, intent.ID, string(controlplane.GraceCommandJobs), "req-rejected"); err != nil {
		t.Fatalf("SetMoveIntentTargetRequest: %v", err)
	}

	prevCheckAck := syncCheckGraceCommandAck
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() {
		syncCheckGraceCommandAck = prevCheckAck
		sendGraceCancelAttempts = prevSendCancel
	})
	syncCheckGraceCommandAck = func(_ context.Context, _ controlplane.GraceStore, instanceID int64, requestID string) (*controlplane.GraceCommandAck, bool, error) {
		if instanceID != targetLaunch || requestID != "req-rejected" {
			t.Fatalf("ack lookup = (%d, %q), want (%d, req-rejected)", instanceID, requestID, targetLaunch)
		}
		return &controlplane.GraceCommandAck{Kind: controlplane.GraceCommandJobs, Accepted: false, Message: "queue full"}, true, nil
	}
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, launchID int64, attemptIDs []int64) error {
		canceledByLaunch[launchID] = append(canceledByLaunch[launchID], attemptIDs...)
		return nil
	}

	reconcilePendingMoveTargetRequestAcks(context.Background(), database, &r2.Client{}, targetLaunch)

	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateCanceled {
		t.Fatalf("intent state = %q, want canceled", gotIntent.State)
	}
	if len(canceledByLaunch) != 0 {
		t.Fatalf("source cancel-attempts = %v, want none", canceledByLaunch)
	}
	var targetReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&targetReason); err != nil {
		t.Fatalf("read target abandoned reason: %v", err)
	}
	if targetReason != db.AttemptAbandonedMoveDestinationRejected {
		t.Fatalf("target abandoned reason = %q, want %q", targetReason, db.AttemptAbandonedMoveDestinationRejected)
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("read source abandoned reason: %v", err)
	}
	if sourceReason != "" {
		t.Fatalf("source abandoned reason = %q, want still authoritative", sourceReason)
	}
}

func TestSyncInstanceState_GraceStaysWhenPhaseIsGrace(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	futureDeadline := time.Now().Add(5 * time.Minute).Unix()
	if err := db.SetLaunchGraceStarted(database, instanceID, futureDeadline); err != nil {
		t.Fatalf("set grace: %v", err)
	}

	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	// Phase is still "grace" — agent is waiting, not running a job
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return "grace"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		return ""
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckR2GraceStatus = checkR2GraceStatus

	SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	// Should remain in grace
	if ci.Status != db.LaunchStatusGrace {
		t.Errorf("ci.Status = %q, want %q", ci.Status, db.LaunchStatusGrace)
	}
}

func TestSyncInstanceState_DoesNotEnterGraceWithActiveJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	prevLogger := slog.Default()
	capture := weftlogging.NewCapturingHandler(slog.LevelWarn)
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "active job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "grace" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	graceChecks := 0
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool {
		graceChecks++
		return true
	}

	SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	if ci.Status != db.LaunchStatusRunning {
		t.Fatalf("ci.Status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
	if graceChecks != 0 {
		t.Fatalf("grace status check calls = %d, want 0", graceChecks)
	}
	for _, message := range capture.Messages() {
		if strings.Contains(message, "ignoring grace transition while launch has active jobs") {
			t.Fatalf("unexpected warning-level grace-transition log: %q", message)
		}
	}
}

// TestSyncInstanceState_FetchesInParallel verifies that R2 marker reads in
// SyncInstanceState overlap rather than running back-to-back. Each mocked
// fetch sleeps for delay; with 4 fetches (termIntent, phase, jobProgress,
// heartbeat) the serial cost would be ~4*delay, the parallel cost ~2*delay
// (wave 1: termIntent + phase; wave 2: jobProgress + heartbeat).
func TestSyncInstanceState_FetchesInParallel(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	const delay = 150 * time.Millisecond
	var active atomic.Int32
	var maxActive atomic.Int32
	delayed := func() {
		n := active.Add(1)
		for {
			current := maxActive.Load()
			if n <= current || maxActive.CompareAndSwap(current, n) {
				break
			}
		}
		time.Sleep(delay)
		active.Add(-1)
	}
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		delayed()
		return "running:1"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		delayed()
		return ""
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		delayed()
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		delayed()
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		delayed()
		return nil, nil
	}
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }

	// AgentVersionFetched skips the third wave-1 fetch (agent version), so the
	// observed waves run: {termIntent, phase} then {jobProgress, heartbeat}.
	SyncInstanceState(
		context.Background(), database, ci, nil, &r2.Client{}, nil,
		JobState{HasStartedJob: true},
		SyncInstanceStateOpts{AgentVersionFetched: true},
	)
	if got := maxActive.Load(); got < 2 {
		t.Fatalf("maximum concurrent R2 fetches = %d, want at least 2", got)
	}
}

func TestSyncInstanceState_ReconcilesDisplayPhaseFromDBAndR2(t *testing.T) {
	tests := []struct {
		name         string
		rawPhase     func(int64) string
		running      bool
		wantPhase    func(int64) string
		wantRawPhase func(int64) string
	}{
		{
			name:         "setup with running DB job shows running",
			rawPhase:     func(jobID int64) string { return fmt.Sprintf("setup:%d", jobID) },
			running:      true,
			wantPhase:    func(jobID int64) string { return fmt.Sprintf("running:%d", jobID) },
			wantRawPhase: func(jobID int64) string { return fmt.Sprintf("setup:%d", jobID) },
		},
		{
			name:         "setup with queued DB job stays setup",
			rawPhase:     func(jobID int64) string { return fmt.Sprintf("setup:%d", jobID) },
			running:      false,
			wantPhase:    func(jobID int64) string { return fmt.Sprintf("setup:%d", jobID) },
			wantRawPhase: func(jobID int64) string { return fmt.Sprintf("setup:%d", jobID) },
		},
		{
			name:         "empty raw phase falls back to running DB job",
			rawPhase:     func(int64) string { return "" },
			running:      true,
			wantPhase:    func(jobID int64) string { return fmt.Sprintf("running:%d", jobID) },
			wantRawPhase: func(int64) string { return "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()

			instanceID, err := db.CreateLaunch(database, &db.Launch{
				Status:  db.LaunchStatusRunning,
				GPUSpec: "RTX_4090",
			})
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}
			ci, _ := db.GetLaunch(database, instanceID)
			jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "phase job")
			if err != nil {
				t.Fatalf("RecordQueued: %v", err)
			}
			if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
				t.Fatalf("SetJobLaunchID: %v", err)
			}
			if tt.running {
				if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
					t.Fatalf("MarkQueuedJobRunning: %v", err)
				}
			}

			origPhase := syncFetchInstancePhase
			origBootstrap := syncFetchBootstrapStage
			origHB := syncFetchHeartbeat
			origProgress := syncFetchJobProgress
			origIntent := syncFetchTermIntent
			origGrace := syncCheckR2GraceStatus
			t.Cleanup(func() {
				syncFetchInstancePhase = origPhase
				syncFetchBootstrapStage = origBootstrap
				syncFetchHeartbeat = origHB
				syncFetchJobProgress = origProgress
				syncFetchTermIntent = origIntent
				syncCheckR2GraceStatus = origGrace
			})

			syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
			syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
				return nil, 0
			}
			syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
				return 0, -1, 0
			}
			syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
				return nil, nil
			}
			syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }
			syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
				return tt.rawPhase(jobID)
			}
			synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{AgentVersionFetched: true})
			if synced.InstancePhase != tt.wantPhase(jobID) {
				t.Fatalf("InstancePhase = %q, want %q", synced.InstancePhase, tt.wantPhase(jobID))
			}
			if synced.RawInstancePhase != tt.wantRawPhase(jobID) {
				t.Fatalf("RawInstancePhase = %q, want %q", synced.RawInstancePhase, tt.wantRawPhase(jobID))
			}
		})
	}
}

func TestSyncInstanceState_SetupPhaseMarksQueuedJobStarting(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "setup job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return fmt.Sprintf("setup:%d", jobID)
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.JobsUpdated != 1 {
		t.Fatalf("JobsUpdated = %d, want 1", synced.JobsUpdated)
	}
	if synced.InstancePhase != fmt.Sprintf("setup:%d", jobID) {
		t.Fatalf("InstancePhase = %q, want setup phase", synced.InstancePhase)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusStarting {
		t.Fatalf("job.Status = %q, want %q", job.Status, db.StatusStarting)
	}

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return fmt.Sprintf("running:%d", jobID)
	}
	ci, _ = db.GetLaunch(database, instanceID)
	synced = SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, []*db.Job{job}, JobState{}, SyncInstanceStateOpts{})
	if synced.JobsUpdated != 1 {
		t.Fatalf("running JobsUpdated = %d, want 1", synced.JobsUpdated)
	}
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after running: %v", err)
	}
	if job.Status != db.StatusRunning {
		t.Fatalf("job.Status after running = %q, want %q", job.Status, db.StatusRunning)
	}
}

func TestSyncInstanceState_DisplayOnlyPhaseDoesNotBlockBootstrapTimeout(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchBootstrapDeadline(database, instanceID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("set bootstrap deadline: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origOnStart := syncFetchOnStartStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchOnStartStage = origOnStart
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})

	bootstrapCalls := 0
	progressCalls := 0
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		return "post_job_uploads_drained:3583"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		bootstrapCalls++
		return ""
	}
	syncFetchOnStartStage = func(_ context.Context, _ *r2.Client, _ int64) (string, *time.Time) {
		return "", nil
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		progressCalls++
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{AgentVersionFetched: true})
	if synced.InstancePhase != "post_job_uploads_drained:3583" {
		t.Fatalf("InstancePhase = %q, want display-only phase preserved", synced.InstancePhase)
	}
	if bootstrapCalls == 0 {
		t.Fatal("bootstrap stage was not fetched despite display-only phase")
	}
	if progressCalls != 0 {
		t.Fatalf("job progress fetched %d time(s), want 0 for display-only phase", progressCalls)
	}

	params := synced.CheckParams(database, ci, &r2.Client{}, nil, nil, JobState{}, NewProviderObservation(nil, nil, 0), NewSurvivalThresholds(nil, nil), time.Now())
	if params.InstancePhase != "" {
		t.Fatalf("CheckParams.InstancePhase = %q, want empty for watchdogs", params.InstancePhase)
	}
	action := NewReconciler().CheckInstance(params)
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d), message=%q", action.Kind, ActionBootstrapStalled, action.StallMessage)
	}
	if action.TerminationReason != db.TerminationReasonBootstrapTimeout {
		t.Fatalf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonBootstrapTimeout)
	}
}

func TestSyncInstanceState_PrefersFreshHeartbeatPhase(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(`UPDATE launches SET agent_ready_at_unix = ? WHERE id = ?`, now, instanceID); err != nil {
		t.Fatalf("set agent ready: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origGrace := syncCheckR2GraceStatus
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckR2GraceStatus = origGrace
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "uploading:1383" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		alive := true
		return &HeartbeatSample{
			Ts:            time.Now().Unix(),
			Phase:         "setup:1760",
			AgentAlive:    &alive,
			AgentProtocol: controlplane.AgentProtocolVersion,
		}, time.Second
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }

	jobs := []*db.Job{
		{ID: 1383, Status: db.StatusFailed},
		{ID: 1760, Status: db.StatusQueued},
	}
	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, jobs, JobState{HasStartedJob: true}, SyncInstanceStateOpts{AgentVersionFetched: true})
	if synced.RawInstancePhase != "uploading:1383" {
		t.Fatalf("RawInstancePhase = %q, want %q", synced.RawInstancePhase, "uploading:1383")
	}
	if synced.InstancePhase != "setup:1760" {
		t.Fatalf("InstancePhase = %q, want %q", synced.InstancePhase, "setup:1760")
	}
	live, err := db.GetLaunchLiveState(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunchLiveState: %v", err)
	}
	if live == nil || live.AgentProtocol != controlplane.AgentProtocolVersion {
		t.Fatalf("stored agent protocol = %v, want %d", live, controlplane.AgentProtocolVersion)
	}
}

func TestSyncInstanceState_ExtendsBootstrapDeadlineFromFirstStage(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX_A6000",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	pastDeadline := time.Now().Add(-time.Minute)
	if err := db.SetLaunchBootstrapDeadline(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set bootstrap deadline: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "agent_starting" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	before := time.Now()
	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.BootstrapStage != "agent_starting" {
		t.Fatalf("BootstrapStage = %q, want agent_starting", synced.BootstrapStage)
	}
	if ci.BootstrapDeadlineUnix == nil {
		t.Fatal("BootstrapDeadlineUnix is nil")
	}
	if got := time.Unix(*ci.BootstrapDeadlineUnix, 0); got.Before(before.Add(10 * time.Minute)) {
		t.Fatalf("BootstrapDeadlineUnix = %s, want deadline extended from first stage", got)
	}

	stored, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if stored.BootstrapDeadlineUnix == nil || *stored.BootstrapDeadlineUnix != *ci.BootstrapDeadlineUnix {
		t.Fatalf("stored deadline = %v, want %v", stored.BootstrapDeadlineUnix, ci.BootstrapDeadlineUnix)
	}
}

func TestSyncInstanceState_DoesNotExtendBootstrapDeadlineAfterStageAlreadySeen(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX_A6000",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	pastDeadline := time.Now().Add(-time.Minute)
	if err := db.SetLaunchBootstrapDeadline(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set bootstrap deadline: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		BootstrapStage: "agent_starting",
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "agent_starting" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if ci.BootstrapDeadlineUnix == nil || *ci.BootstrapDeadlineUnix != pastDeadline.Unix() {
		t.Fatalf("BootstrapDeadlineUnix = %v, want unchanged %d", ci.BootstrapDeadlineUnix, pastDeadline.Unix())
	}
}

func TestSyncInstanceState_ExtendsBootstrapDeadlineFromLaterStageProgress(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX_A6000",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		BootstrapStage: "agent_starting",
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	pastDeadline := time.Now().Add(-time.Minute)
	if err := db.SetLaunchBootstrapDeadline(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set bootstrap deadline: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "sources_extracted" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	before := time.Now()
	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.BootstrapStage != "sources_extracted" {
		t.Fatalf("BootstrapStage = %q, want sources_extracted", synced.BootstrapStage)
	}
	if ci.BootstrapDeadlineUnix == nil {
		t.Fatal("BootstrapDeadlineUnix is nil")
	}
	if got := time.Unix(*ci.BootstrapDeadlineUnix, 0); got.Before(before.Add(10 * time.Minute)) {
		t.Fatalf("BootstrapDeadlineUnix = %s, want deadline extended from later bootstrap progress", got)
	}
}

func TestSyncInstanceState_RemembersBootstrapActivityAcrossEmptyFetch(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX_A6000",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		BootstrapStage: "sources_extracting:0/1",
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})

	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.BootstrapStage != "" {
		t.Fatalf("BootstrapStage = %q, want empty current fetch", synced.BootstrapStage)
	}
	if !synced.BootstrapActivitySeen {
		t.Fatal("BootstrapActivitySeen = false, want true from prior cached stage")
	}
	params := synced.CheckParams(database, ci, &r2.Client{}, nil, nil, JobState{}, NewProviderObservation(nil, nil, 0), NewSurvivalThresholds(nil, nil), time.Now())
	if !params.BootstrapActivitySeen {
		t.Fatal("CheckParams BootstrapActivitySeen = false, want true")
	}
}

// TestSyncInstanceState_OnStartProbePresumesPresentOnError: probe-check
// errors yield OnStartProbePresent=true. Rule 4d gates on positive
// absence, not unobserved. Regression for EXP-021.
func TestSyncInstanceState_OnStartProbePresumesPresentOnError(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-9 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)
	if ci.LaunchedAt == nil {
		t.Fatalf("LaunchedAt not persisted")
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckOnStartProbe = origProbe
	})
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, context.DeadlineExceeded
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if !synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = false on R2 error; defensive policy requires true so the dud watchdog cannot fire on a missed read")
	}

	// Successful "exists=false" must still pass through honestly — only
	// the *error* case is treated as presumed-present.
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, nil
	}
	synced = SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = true on confirmed-absent probe; want false so the dud watchdog can fire on a real dud")
	}

	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}
	synced = SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if !synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = false on confirmed-present probe; want true")
	}
}

// TestSyncInstanceState_OnStartScriptVerifyGating pins when weft is willing to
// SSH a container to read its start script: only once the probe has been
// confirmed absent, and only past the grace period. Every SSH here lands on a
// rental that is still being paid for, and a container that has not yet been
// given its script must not be misread as one that never will be.
func TestSyncInstanceState_OnStartScriptVerifyGating(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sinceLaunch time.Duration
		probeExists bool
		wantCalls   int
	}{
		{"probe absent past grace", 9 * time.Minute, false, 1},
		{"probe absent inside grace", 30 * time.Second, false, 0},
		{"probe present", 9 * time.Minute, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()

			instanceID, err := db.CreateLaunch(database, &db.Launch{
				Status:  db.LaunchStatusRunning,
				GPUSpec: "RTX_4090",
			})
			if err != nil {
				t.Fatalf("create instance: %v", err)
			}
			launchedAt := time.Now().Add(-tc.sinceLaunch).Unix()
			if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
				t.Fatalf("set launched_at: %v", err)
			}
			ci, _ := db.GetLaunch(database, instanceID)

			origProbe := syncCheckOnStartProbe
			origVerify := syncVerifyOnStartScript
			t.Cleanup(func() {
				syncCheckOnStartProbe = origProbe
				syncVerifyOnStartScript = origVerify
				resetOnStartVerifyMemo()
			})
			resetOnStartVerifyMemo()
			syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
				return tc.probeExists, nil
			}
			verifyCalls := 0
			syncVerifyOnStartScript = func(_ context.Context, _ *cloud.Instance, _ time.Duration) cloud.OnStartVerification {
				verifyCalls++
				return cloud.OnStartConfirmedMissing
			}

			providerInst := &cloud.Instance{Provider: cloud.ProviderVastai, SSHHost: "ssh.example.invalid"}
			synced := SyncInstanceState(context.Background(), database, ci, providerInst, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

			if verifyCalls != tc.wantCalls {
				t.Fatalf("syncVerifyOnStartScript called %d times, want %d", verifyCalls, tc.wantCalls)
			}
			wantVerdict := cloud.OnStartVerificationUnknown
			if tc.wantCalls > 0 {
				wantVerdict = cloud.OnStartConfirmedMissing
			}
			if synced.OnStartScriptVerification != wantVerdict {
				t.Fatalf("OnStartScriptVerification = %v, want %v", synced.OnStartScriptVerification, wantVerdict)
			}
		})
	}
}

// TestSyncInstanceState_OnStartScriptVerifySettledVerdictIsNotReasked: a
// script that is on disk does not come off, so the confirmed-installed verdict
// is asked once. Without the memo a healthy-but-slow container is SSHed on
// every reconcile tick for the rest of its dud window.
func TestSyncInstanceState_OnStartScriptVerifySettledVerdictIsNotReasked(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	launchedAt := time.Now().Add(-9 * time.Minute).Unix()
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	origProbe := syncCheckOnStartProbe
	origVerify := syncVerifyOnStartScript
	t.Cleanup(func() {
		syncCheckOnStartProbe = origProbe
		syncVerifyOnStartScript = origVerify
		resetOnStartVerifyMemo()
	})
	resetOnStartVerifyMemo()
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) { return false, nil }
	verifyCalls := 0
	syncVerifyOnStartScript = func(_ context.Context, _ *cloud.Instance, _ time.Duration) cloud.OnStartVerification {
		verifyCalls++
		return cloud.OnStartConfirmedInstalled
	}

	providerInst := &cloud.Instance{Provider: cloud.ProviderVastai, SSHHost: "ssh.example.invalid"}
	for range 3 {
		synced := SyncInstanceState(context.Background(), database, ci, providerInst, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
		if synced.OnStartScriptVerification != cloud.OnStartConfirmedInstalled {
			t.Fatalf("OnStartScriptVerification = %v, want installed", synced.OnStartScriptVerification)
		}
	}
	if verifyCalls != 1 {
		t.Fatalf("syncVerifyOnStartScript called %d times across 3 passes, want 1", verifyCalls)
	}
}

// TestSyncInstanceState_OnStartProbeSkippedOutsideDudWindow verifies that the
// R2 round-trip for the OnStart probe is skipped once the agent has reached
// ready (AgentReadyAtUnix set), since the dud watchdog only cares about the
// post-running, pre-ready window. This keeps reconcile passes cheap on
// healthy fleets — see the comment block on the live binding.
func TestSyncInstanceState_OnStartProbeSkippedOutsideDudWindow(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-1 * time.Hour).Unix()
	readyAt := time.Now().Add(-30 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	_ = db.SetLaunchAgentReadyAtIfUnset(database, instanceID, time.Unix(readyAt, 0))
	ci, _ := db.GetLaunch(database, instanceID)

	probeCalls := 0
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() { syncCheckOnStartProbe = origProbe })
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		probeCalls++
		return true, nil
	}

	_ = SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{HasStartedJob: true}, SyncInstanceStateOpts{})

	if probeCalls != 0 {
		t.Fatalf("syncCheckOnStartProbe called %d times for ready instance; want 0", probeCalls)
	}
}

// TestSyncInstanceState_HeartbeatFetchedInDudWindow: heartbeat must be
// fetched in the dud window so the agent's synchronous-at-startup first
// heartbeat is visible to the watchdog. Regression for EXP-021.
func TestSyncInstanceState_HeartbeatFetchedInDudWindow(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-5 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)

	hbCalls := 0
	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckOnStartProbe = origProbe
	})
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "agent_starting" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		hbCalls++
		return &HeartbeatSample{Ts: time.Now().Unix()}, 1 * time.Second
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}

	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{HasStartedJob: false}, SyncInstanceStateOpts{})

	if hbCalls == 0 {
		t.Fatalf("heartbeat not fetched inside dud-detection window; previous gating left the watchdog blind to the agent's first heartbeat")
	}
	if synced.Heartbeat == nil {
		t.Fatalf("synced.Heartbeat = nil, want non-nil from fetch")
	}
	if synced.HeartbeatAge == 0 {
		t.Fatalf("synced.HeartbeatAge = 0, want > 0 (fresh heartbeat)")
	}
}

func TestSyncInstanceState_LiveStateWriteFailureUsesCurrentObservationTime(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "echo test", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("attach job: %v", err)
	}
	if _, err := database.Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_live_state_write
		BEFORE INSERT ON launch_live_state
		WHEN NEW.launch_id = %d
		BEGIN
			SELECT RAISE(ABORT, 'injected live-state write failure');
		END`, instanceID)); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	jobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID)
	if err != nil {
		t.Fatalf("get launch jobs: %v", err)
	}

	origPhase := syncFetchInstancePhase
	origHeartbeat := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchHeartbeat = origHeartbeat
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
	})
	syncFetchInstancePhase = func(context.Context, *r2.Client, int64) string {
		return fmt.Sprintf("setup:%d", jobID)
	}
	syncFetchHeartbeat = func(context.Context, *r2.Client, int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(context.Context, *r2.Client, string, []*db.Job) (int64, int, int) {
		return jobID, -1, 0
	}
	syncFetchTermIntent = func(context.Context, *r2.Client, int64) (*instanceintent.Marker, error) {
		return nil, nil
	}

	started := time.Now()
	synced := SyncInstanceState(
		context.Background(), database, ci, nil, &r2.Client{}, jobs, JobState{},
		SyncInstanceStateOpts{AgentVersionFetched: true},
	)
	if synced.PhaseChangedAt == nil {
		t.Fatal("phase changed time is unknown after a current phase observation whose cache write failed")
	}
	if synced.PhaseChangedAt.Before(started.Add(-time.Second)) {
		t.Fatalf("phase changed time = %s, want current observation time", synced.PhaseChangedAt)
	}
}

// TestDudWatchdogSurvivesFlakyR2: cross-cutting fault-injection regression
// for EXP-021. A /3-cycle flaky R2 on a healthy instance must not false-fire
// rule 4d across 60 sync+check ticks.
func TestDudWatchdogSurvivesFlakyR2(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-10 * time.Minute).Unix() // past dud timeout
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}

	// Healthy-bootstrap baseline: agent has reached agent_starting and is
	// emitting heartbeats. Without R2 flakiness, rule 4d's `BootstrapStage
	// == ""` and `HeartbeatAge == 0` conjuncts would each be false.
	const healthyStage = "agent_starting"
	healthyHB := func() (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Unix()}, 1 * time.Second
	}

	// Deterministic "flaky" decorator: every Nth call returns the failure
	// path. Using a counter rather than rand keeps the test reproducible
	// across CI runs.
	const flakeEvery = 3 // ~33% failure rate
	var calls atomic.Int64
	flake := func() bool {
		return calls.Add(1)%flakeEvery == 0
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckOnStartProbe = origProbe
	})
	syncFetchInstancePhase = func(ctx context.Context, _ *r2.Client, _ int64) string {
		if flake() {
			recordMarkerObservationError(ctx, context.DeadlineExceeded)
			return ""
		}
		return ""
	}
	syncFetchBootstrapStage = func(ctx context.Context, _ *r2.Client, _ int64) string {
		if flake() {
			recordMarkerObservationError(ctx, context.DeadlineExceeded)
			return ""
		}
		return healthyStage
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		if flake() {
			return nil, 0
		}
		return healthyHB()
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		if flake() {
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		if flake() {
			return false, context.DeadlineExceeded
		}
		return true, nil
	}

	// Loop enough times to hit every flake combination across all
	// fetchers. With 6 fetchers and a /3 cycle the combination space
	// repeats inside ~18 ticks; we run 60 to leave wide margin.
	rec := NewReconciler()
	ci, _ := db.GetLaunch(database, instanceID)
	for i := 0; i < 60; i++ {
		synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
		params := synced.CheckParams(database, ci, &r2.Client{}, nil, nil, JobState{}, NewProviderObservation(nil, nil, 0), NewSurvivalThresholds(nil, nil), time.Now())
		action := rec.CheckInstance(params)
		if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "dud provider") {
			t.Fatalf("tick %d: rule 4d false-fired on a healthy-but-R2-flaky instance:\n  stall=%q\n  params=%+v",
				i, action.StallMessage, params)
		}
	}
}

// TestDudWatchdogFiresOnRealDud: confirms the defensive policy did not
// over-defend. On a confirmed dud (honest R2, no probe, no heartbeat, no
// bootstrap activity) rule 4d still fires after the timeout.
func TestDudWatchdogFiresOnRealDud(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-10 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckOnStartProbe = origProbe
	})
	// Honest R2: every read confirms absence with no errors.
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, nil // confirmed-absent
	}

	rec := NewReconciler()
	ci, _ := db.GetLaunch(database, instanceID)
	synced := SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	params := synced.CheckParams(database, ci, &r2.Client{}, nil, nil, JobState{}, NewProviderObservation(nil, nil, 0), NewSurvivalThresholds(nil, nil), time.Now())
	action := rec.CheckInstance(params)

	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("rule 4d did NOT fire on a confirmed dud (action=%v, msg=%q); over-defended", action.Kind, action.StallMessage)
	}
	if !strings.Contains(action.StallMessage, "dud provider") {
		t.Fatalf("fired action is not the dud watchdog: %q", action.StallMessage)
	}
}

// TestSyncInstanceState_PersistsFirstOnStartProbeSeen: first confirmed
// probe observation persists to launches.first_onstart_probe_seen_unix
// (set-once); presumed-present-on-error observations must NOT persist
// (uncontaminated survival training data). See EXP-021.
func TestSyncInstanceState_PersistsFirstOnStartProbeSeen(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchedAt := time.Now().Add(-3 * time.Minute).Unix() // inside dud window
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, instanceID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	ci, _ := db.GetLaunch(database, instanceID)
	if ci.FirstOnStartProbeSeenUnix != nil {
		t.Fatalf("FirstOnStartProbeSeenUnix should start NULL, got %v", *ci.FirstOnStartProbeSeenUnix)
	}

	origPhase := syncFetchInstancePhase
	origBootstrap := syncFetchBootstrapStage
	origHB := syncFetchHeartbeat
	origProgress := syncFetchJobProgress
	origIntent := syncFetchTermIntent
	origProbe := syncCheckOnStartProbe
	t.Cleanup(func() {
		syncFetchInstancePhase = origPhase
		syncFetchBootstrapStage = origBootstrap
		syncFetchHeartbeat = origHB
		syncFetchJobProgress = origProgress
		syncFetchTermIntent = origIntent
		syncCheckOnStartProbe = origProbe
	})
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	// First check returns "exists=true" with no error: probe is present.
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}

	t0 := time.Now()
	SyncInstanceState(context.Background(), database, ci, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	got, _ := db.GetLaunch(database, instanceID)
	if got.FirstOnStartProbeSeenUnix == nil {
		t.Fatalf("FirstOnStartProbeSeenUnix not persisted after first observation")
	}
	if delta := time.Unix(*got.FirstOnStartProbeSeenUnix, 0).Sub(t0); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("FirstOnStartProbeSeenUnix = %v, want close to %v (delta %v)", *got.FirstOnStartProbeSeenUnix, t0, delta)
	}

	// A subsequent observation must NOT overwrite (set-once semantics).
	originalTs := *got.FirstOnStartProbeSeenUnix
	time.Sleep(1100 * time.Millisecond)
	SyncInstanceState(context.Background(), database, got, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	again, _ := db.GetLaunch(database, instanceID)
	if again.FirstOnStartProbeSeenUnix == nil || *again.FirstOnStartProbeSeenUnix != originalTs {
		t.Fatalf("second observation overwrote first-seen: was %d, now %v",
			originalTs, again.FirstOnStartProbeSeenUnix)
	}

	// A presumed-present-on-error observation must NOT persist (we only
	// want true positives in the training data).
	another, err := db.CreateLaunch(database, &db.Launch{
		Status:  db.LaunchStatusRunning,
		GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create second instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt, another); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, context.DeadlineExceeded // R2 errored; presumed-present
	}
	ci2, _ := db.GetLaunch(database, another)
	SyncInstanceState(context.Background(), database, ci2, nil, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	got2, _ := db.GetLaunch(database, another)
	if got2.FirstOnStartProbeSeenUnix != nil {
		t.Fatalf("R2-errored 'presumed-present' must not persist a first-seen time; corrupts survival training data. got %v", *got2.FirstOnStartProbeSeenUnix)
	}
}

func TestReconcileAbandonedMoveTargetAttempts_SendsCancelMarkers(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, GPUSpec: "RTX_4090"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, GPUSpec: "RTX_4090"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	attemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, "launch reset; move abandoned"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() { sendGraceCancelAttempts = prevSendCancel })
	var gotLaunch int64
	var gotAttempts []int64
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, launchID int64, attemptIDs []int64) error {
		gotLaunch = launchID
		gotAttempts = append(gotAttempts, attemptIDs...)
		return nil
	}

	reconcileAbandonedMoveTargetAttempts(context.Background(), database, nil, target)

	if gotLaunch != target {
		t.Fatalf("cancel sent to launch %d, want %d", gotLaunch, target)
	}
	if len(gotAttempts) != 1 || gotAttempts[0] != attemptID {
		t.Fatalf("cancel attempts = %v, want [%d]", gotAttempts, attemptID)
	}

	// No recent abandons on the source launch: nothing sent.
	gotLaunch, gotAttempts = 0, nil
	reconcileAbandonedMoveTargetAttempts(context.Background(), database, nil, src)
	if gotLaunch != 0 || len(gotAttempts) != 0 {
		t.Fatalf("unexpected cancel send for source launch: launch=%d attempts=%v", gotLaunch, gotAttempts)
	}
}

func TestReconcileAbandonedMoveTargetAttempts_RecordsCancelMarkerSendFailure(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	target, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, GPUSpec: "RTX_4090"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "train")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     db.MoveTargetExisting,
		TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	attemptID, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, "launch reset; move abandoned"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() { sendGraceCancelAttempts = prevSendCancel })
	sendErr := errors.New("r2 put failed")
	sendGraceCancelAttempts = func(context.Context, controlplane.GraceStore, int64, []int64) error {
		return sendErr
	}

	reconcileAbandonedMoveTargetAttempts(context.Background(), database, nil, target)

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind:     db.EventGraceCancelAttemptsFailed,
		LaunchID: target,
	})
	if err != nil {
		t.Fatalf("ListLifecycleEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("cancel-attempt failure events = %d, want 1", len(events))
	}
	if events[0].JobCount != 1 {
		t.Fatalf("event job_count = %d, want 1", events[0].JobCount)
	}
	if !strings.Contains(events[0].Detail, fmt.Sprintf("attempt_ids=[%d]", attemptID)) {
		t.Fatalf("event detail = %q, want attempt id %d", events[0].Detail, attemptID)
	}
	if events[0].ErrorText != sendErr.Error() {
		t.Fatalf("event error = %q, want %q", events[0].ErrorText, sendErr.Error())
	}
}
