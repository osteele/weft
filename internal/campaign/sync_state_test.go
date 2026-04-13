package campaign

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

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

	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

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

	SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

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

	jobs := []*db.Job{{ID: 1, Status: db.StatusRunning, StartTime: time.Now().Unix()}}
	SyncInstanceState(context.Background(), database, ci, &r2.Client{}, jobs, JobState{HasStartedJob: true}, SyncInstanceStateOpts{})

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
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		time.Sleep(delay)
		return "running:1"
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		time.Sleep(delay)
		return ""
	}
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) {
		time.Sleep(delay)
		return nil, 0
	}
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		time.Sleep(delay)
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		time.Sleep(delay)
		return nil, nil
	}
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }

	// AgentVersionFetched skips the third wave-1 fetch (agent version), so the
	// observed waves run: {termIntent, phase} then {jobProgress, heartbeat}.
	start := time.Now()
	SyncInstanceState(
		context.Background(), database, ci, &r2.Client{}, nil,
		JobState{HasStartedJob: true},
		SyncInstanceStateOpts{AgentVersionFetched: true},
	)
	elapsed := time.Since(start)

	// Serial would be ~4*delay (600ms). Parallel should be ~2*delay (300ms).
	// Allow generous headroom for CI jitter.
	if elapsed >= 3*delay {
		t.Fatalf("SyncInstanceState took %v; expected < %v (3*delay). R2 fetches are not running in parallel.", elapsed, 3*delay)
	}
}
