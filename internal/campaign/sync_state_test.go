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

func TestSyncInstanceState_ReconcilesDisplayPhaseFromDBAndR2(t *testing.T) {
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

	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string { return "" }
	syncFetchHeartbeat = func(_ context.Context, _ *r2.Client, _ int64) (*HeartbeatSample, time.Duration) { return nil, 0 }
	syncFetchJobProgress = func(_ context.Context, _ *r2.Client, _ string, _ []*db.Job) (int64, int, int) {
		return 0, -1, 0
	}
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, _ int64) (*instanceintent.Marker, error) {
		return nil, nil
	}
	syncCheckR2GraceStatus = func(_ *r2.Client, _ *db.Launch, _ *sql.DB) bool { return false }

	tests := []struct {
		name         string
		rawPhase     string
		jobs         []*db.Job
		wantPhase    string
		wantRawPhase string
	}{
		{
			name:         "setup with running DB job shows running",
			rawPhase:     "setup:42",
			jobs:         []*db.Job{{ID: 42, Status: db.StatusRunning, StartTime: time.Now().Add(-10 * time.Minute).Unix()}},
			wantPhase:    "running:42",
			wantRawPhase: "setup:42",
		},
		{
			name:         "setup with queued DB job stays setup",
			rawPhase:     "setup:42",
			jobs:         []*db.Job{{ID: 42, Status: db.StatusQueued}},
			wantPhase:    "setup:42",
			wantRawPhase: "setup:42",
		},
		{
			name:         "empty raw phase falls back to running DB job",
			rawPhase:     "",
			jobs:         []*db.Job{{ID: 73, Status: db.StatusRunning, StartTime: time.Now().Add(-7 * time.Minute).Unix()}},
			wantPhase:    "running:73",
			wantRawPhase: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
				return tt.rawPhase
			}
			synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, tt.jobs, JobState{}, SyncInstanceStateOpts{AgentVersionFetched: true})
			if synced.InstancePhase != tt.wantPhase {
				t.Fatalf("InstancePhase = %q, want %q", synced.InstancePhase, tt.wantPhase)
			}
			if synced.RawInstancePhase != tt.wantRawPhase {
				t.Fatalf("RawInstancePhase = %q, want %q", synced.RawInstancePhase, tt.wantRawPhase)
			}
		})
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
		return &HeartbeatSample{Ts: time.Now().Unix(), Phase: "setup:1760", AgentAlive: &alive}, time.Second
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
	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, jobs, JobState{HasStartedJob: true}, SyncInstanceStateOpts{AgentVersionFetched: true})
	if synced.RawInstancePhase != "uploading:1383" {
		t.Fatalf("RawInstancePhase = %q, want %q", synced.RawInstancePhase, "uploading:1383")
	}
	if synced.InstancePhase != "setup:1760" {
		t.Fatalf("InstancePhase = %q, want %q", synced.InstancePhase, "setup:1760")
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
	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
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

	SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
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
	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
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

	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.BootstrapStage != "" {
		t.Fatalf("BootstrapStage = %q, want empty current fetch", synced.BootstrapStage)
	}
	if !synced.BootstrapActivitySeen {
		t.Fatal("BootstrapActivitySeen = false, want true from prior cached stage")
	}
	params := synced.CheckParams(ci, &r2.Client{}, JobState{}, time.Now())
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

	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if !synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = false on R2 error; defensive policy requires true so the dud watchdog cannot fire on a missed read")
	}

	// Successful "exists=false" must still pass through honestly — only
	// the *error* case is treated as presumed-present.
	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, nil
	}
	synced = SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = true on confirmed-absent probe; want false so the dud watchdog can fire on a real dud")
	}

	syncCheckOnStartProbe = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}
	synced = SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	if !synced.OnStartProbePresent {
		t.Fatalf("OnStartProbePresent = false on confirmed-present probe; want true")
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

	_ = SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{HasStartedJob: true}, SyncInstanceStateOpts{})

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

	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{HasStartedJob: false}, SyncInstanceStateOpts{})

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
	calls := 0
	flake := func() bool {
		calls++
		return calls%flakeEvery == 0
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
	syncFetchInstancePhase = func(_ context.Context, _ *r2.Client, _ int64) string {
		if flake() {
			return ""
		}
		return ""
	}
	syncFetchBootstrapStage = func(_ context.Context, _ *r2.Client, _ int64) string {
		if flake() {
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
		synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
		params := synced.CheckParams(ci, &r2.Client{}, JobState{}, time.Now())
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
	synced := SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})
	params := synced.CheckParams(ci, &r2.Client{}, JobState{}, time.Now())
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
	SyncInstanceState(context.Background(), database, ci, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

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
	SyncInstanceState(context.Background(), database, got, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

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
	SyncInstanceState(context.Background(), database, ci2, &r2.Client{}, nil, JobState{}, SyncInstanceStateOpts{})

	got2, _ := db.GetLaunch(database, another)
	if got2.FirstOnStartProbeSeenUnix != nil {
		t.Fatalf("R2-errored 'presumed-present' must not persist a first-seen time; corrupts survival training data. got %v", *got2.FirstOnStartProbeSeenUnix)
	}
}
