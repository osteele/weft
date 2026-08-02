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
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	weftlogging "github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
)

func TestCompletionManifestCoversLaunchJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	for _, jobID := range []int64{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'echo ok', 0)`, jobID); err != nil {
			t.Fatalf("create job %d: %v", jobID, err)
		}
		attemptID, err := db.CreateAttempt(database, jobID, "", &instanceID, db.StatusQueued)
		if err != nil {
			t.Fatalf("CreateAttempt(%d): %v", jobID, err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ?, end_time = ? WHERE id = ?`, db.AttemptOutcomeCompleted, time.Now().Unix(), attemptID); err != nil {
			t.Fatalf("close attempt %d: %v", attemptID, err)
		}
	}

	manifest := &runner.InstanceCompletionManifest{
		Jobs: []runner.JobCompletionSummary{
			{JobID: 1, ExitCode: 0, UploadStatus: "ok"},
			{JobID: 2, ExitCode: 0, UploadStatus: "ok"},
		},
	}
	if !completionManifestCoversLaunchJobs(database, instanceID, manifest) {
		t.Fatal("expected manifest to cover launch jobs")
	}
}

func TestCompletionManifestCoversLaunchJobs_MissingJob(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	for _, spec := range []struct {
		jobID   int64
		outcome string
	}{
		{jobID: 1, outcome: db.AttemptOutcomeCompleted},
		{jobID: 2, outcome: db.AttemptOutcomeOrphaned},
	} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'echo ok', 0)`, spec.jobID); err != nil {
			t.Fatalf("create job %d: %v", spec.jobID, err)
		}
		attemptID, err := db.CreateAttempt(database, spec.jobID, "", &instanceID, db.StatusQueued)
		if err != nil {
			t.Fatalf("CreateAttempt(%d): %v", spec.jobID, err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ?, end_time = ? WHERE id = ?`, spec.outcome, time.Now().Unix(), attemptID); err != nil {
			t.Fatalf("close attempt %d: %v", attemptID, err)
		}
	}

	manifest := &runner.InstanceCompletionManifest{
		Jobs: []runner.JobCompletionSummary{
			{JobID: 1, ExitCode: 0, UploadStatus: "ok"},
		},
	}
	if completionManifestCoversLaunchJobs(database, instanceID, manifest) {
		t.Fatal("expected missing orphaned job to fail coverage")
	}
}

// TestCompletionManifestCoversLaunchJobs_UncreditedAttempt reproduces the
// false-negative behind the "completed but results upload was partial/failed"
// label: an instance terminates "completed" before the per-job completion is
// credited from R2, so the attempt still reads "queued" (no cloud_outcome, no
// end_time) even though the manifest reports the upload OK. Coverage must key
// on launch_id (written at dispatch), not on a credited cloud_outcome, or the
// job's results are spuriously marked unverified.
func TestCompletionManifestCoversLaunchJobs_UncreditedAttempt(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'echo ok', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	// Attempt dispatched to the launch but left uncredited: status queued,
	// cloud_outcome NULL, end_time NULL — the stuck-on-completed-launch state.
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	manifest := &runner.InstanceCompletionManifest{
		Jobs: []runner.JobCompletionSummary{{JobID: 1, ExitCode: 0, UploadStatus: "ok"}},
	}
	if !completionManifestCoversLaunchJobs(database, instanceID, manifest) {
		t.Fatal("uncredited attempt dispatched to the launch must still be covered by the manifest")
	}
}

func TestResultsVerifyVerdict(t *testing.T) {
	for _, tc := range []struct {
		name         string
		uploadsOK    bool
		covers       bool
		wantVerified bool
		wantDetail   string
	}{
		{"verified", true, true, true, ""},
		{"upload failure", false, true, false, db.ResultsVerifyDetailUploadsIncomplete},
		{"coverage miss", true, false, false, db.ResultsVerifyDetailManifestMissingJobs},
		{"upload failure takes precedence", false, false, false, db.ResultsVerifyDetailUploadsIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, detail := resultsVerifyVerdict(tc.uploadsOK, tc.covers)
			if verified != tc.wantVerified || detail != tc.wantDetail {
				t.Fatalf("resultsVerifyVerdict(%v, %v) = (%v, %q); want (%v, %q)",
					tc.uploadsOK, tc.covers, verified, detail, tc.wantVerified, tc.wantDetail)
			}
		})
	}
}

// TestUpdateLaunchResultsVerifiedPersistsDetail round-trips the verdict through
// the DB to verify the results_verify_detail column and its scan are wired up.
func TestUpdateLaunchResultsVerifiedPersistsDetail(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.UpdateLaunchResultsVerified(database, instanceID, false, db.ResultsVerifyDetailManifestMissingJobs); err != nil {
		t.Fatalf("UpdateLaunchResultsVerified: %v", err)
	}
	got, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.ResultsVerified == nil || *got.ResultsVerified {
		t.Fatalf("ResultsVerified = %v; want false", got.ResultsVerified)
	}
	if got.ResultsVerifyDetail != db.ResultsVerifyDetailManifestMissingJobs {
		t.Fatalf("ResultsVerifyDetail = %q; want %q", got.ResultsVerifyDetail, db.ResultsVerifyDetailManifestMissingJobs)
	}
}

// attemptStateForJob returns the latest attempt's status, cloud_outcome, and
// exit_code for a job, plus the total number of attempts.
func attemptStateForJob(t *testing.T, database *sql.DB, jobID int64) (status, outcome string, exitCode sql.NullInt64, attemptCount int) {
	t.Helper()
	if err := database.QueryRow(
		`SELECT status, COALESCE(cloud_outcome, ''), exit_code
		   FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&status, &outcome, &exitCode); err != nil {
		t.Fatalf("query latest attempt for job %d: %v", jobID, err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ?`, jobID,
	).Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts for job %d: %v", jobID, err)
	}
	return status, outcome, exitCode, attemptCount
}

// TestCreditManifestCompletions_CreditsRunningAttempt is a regression test for
// the provider_dead race where an instance self-destructs after writing its
// instance-level completion manifest but before the per-job .complete marker
// sync observes the per-job markers. The successful attempt must be credited
// from the manifest so CloseLaunchAttempts' completed-branch does not orphan
// and re-run the job.
func TestCreditManifestCompletions_CreditsRunningAttempt(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python eval.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix()); err != nil {
		t.Fatalf("set start_time: %v", err)
	}

	manifest := &runner.InstanceCompletionManifest{
		CompletedAtUnix: time.Now().Unix(),
		Jobs: []runner.JobCompletionSummary{
			{JobID: 1, ExitCode: 0, UploadStatus: "ok"},
		},
	}
	creditManifestCompletions(database, instanceID, manifest)

	status, outcome, exitCode, _ := attemptStateForJob(t, database, 1)
	if status != db.StatusCompleted {
		t.Fatalf("attempt status = %q, want %q", status, db.StatusCompleted)
	}
	if outcome != db.AttemptOutcomeCompleted {
		t.Fatalf("attempt cloud_outcome = %q, want %q", outcome, db.AttemptOutcomeCompleted)
	}
	if !exitCode.Valid || exitCode.Int64 != 0 {
		t.Fatalf("attempt exit_code = %v, want 0", exitCode)
	}

	// CloseLaunchAttempts' completed-branch must now skip the credited job
	// instead of orphaning and requeuing it.
	if err := db.CloseLaunchAttempts(database, instanceID, db.AttemptOutcomeCompleted); err != nil {
		t.Fatalf("CloseLaunchAttempts: %v", err)
	}
	status, outcome, _, attemptCount := attemptStateForJob(t, database, 1)
	if attemptCount != 1 {
		t.Fatalf("job 1 has %d attempts after CloseLaunchAttempts, want 1 (no re-run)", attemptCount)
	}
	if status != db.StatusCompleted || outcome != db.AttemptOutcomeCompleted {
		t.Fatalf("after CloseLaunchAttempts: status=%q outcome=%q, want completed/completed", status, outcome)
	}
}

// TestCreditManifestCompletions_SkipsFailedAndUnlistedJobs verifies that only
// exit-0 manifest jobs are credited: a job the manifest reports as failed and a
// job absent from the manifest are both left non-terminal for the normal
// orphan/failure path.
func TestCreditManifestCompletions_SkipsFailedAndUnlistedJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	for _, jobID := range []int64{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python eval.py', 0)`, jobID); err != nil {
			t.Fatalf("create job %d: %v", jobID, err)
		}
		if _, err := db.CreateAttempt(database, jobID, "", &instanceID, db.StatusRunning); err != nil {
			t.Fatalf("CreateAttempt(%d): %v", jobID, err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = ? AND end_time IS NULL`, time.Now().Unix(), jobID); err != nil {
			t.Fatalf("set start_time for job %d: %v", jobID, err)
		}
	}

	// Job 1 failed per the manifest; job 2 is absent from the manifest.
	manifest := &runner.InstanceCompletionManifest{
		CompletedAtUnix: time.Now().Unix(),
		Jobs: []runner.JobCompletionSummary{
			{JobID: 1, ExitCode: 1, UploadStatus: "ok"},
		},
	}
	creditManifestCompletions(database, instanceID, manifest)

	for _, jobID := range []int64{1, 2} {
		status, _, _, _ := attemptStateForJob(t, database, jobID)
		if status == db.StatusCompleted {
			t.Fatalf("job %d attempt status = %q, want non-completed", jobID, status)
		}
	}
}

func TestReconcileLaunches_CancelledContextSkipsProviderCalls(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	var calls int32
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			atomic.AddInt32(&calls, 1)
			return nil, nil
		},
		ShowInstanceFunc: func(string) (*cloud.Instance, error) {
			atomic.AddInt32(&calls, 1)
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// A cancelled context (as on quit) must stop the reconcile before it issues
	// any provider calls or DB writes, so a quitting TUI's lease release isn't
	// starved by an in-flight reconcile pass.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := NewReconciler()
	done := make(chan struct{})
	go func() {
		_, _ = r.ReconcileLaunches(ctx, database, []cloud.Client{mockClient}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileLaunches did not return promptly under a cancelled context")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("provider called %d times under a cancelled context; want 0", n)
	}
}

func TestReconcileLaunches_DeadInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job and its attempt associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusQueued); err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	// Mock client that reports the instance as dead
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Use zero deadConfirmTime so the instance is terminated immediately (no hysteresis wait).
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: -1}
	result, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify instance is now failed
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	// Verify job was reset to queued (unplaced)
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_status WHERE id = 1`).Scan(&jobStatus); err != nil {
		t.Fatalf("get job status: %v", err)
	}
	if jobStatus != db.StatusQueued {
		t.Errorf("job status = %q, want %q", jobStatus, db.StatusQueued)
	}

	// Verify attempt was closed
	attempts, err := db.GetLaunchAttempts(database, 1)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome != db.AttemptOutcomeOrphaned {
		t.Errorf("expected 1 attempt with outcome %q, got %v", db.AttemptOutcomeOrphaned, attempts)
	}
}

func TestReconcileLaunches_RaisesInterruptibleBidWhenProviderPaused(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	bid := 14
	onDemand := 20
	launchedAt := time.Now().Add(-2 * time.Hour).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		GPUSpec:          "RTX_4090",
		CostPerHourCents: bid,
		InstanceType:     cloud.InstanceTypeInterruptible,
		MaxBidPriceCents: &bid,
		OnDemandRefCents: &onDemand,
		CreatedAt:        launchedAt,
		LaunchedAt:       &launchedAt,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	var gotID string
	var gotPrice float64
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{{ProviderID: "12345", Status: cloud.ProviderStatusStopped}}, nil
		},
		ChangeBidFunc: func(id string, price float64) error {
			gotID = id
			gotPrice = price
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.BidsRaised != 1 {
		t.Fatalf("BidsRaised = %d, want 1", result.BidsRaised)
	}
	if gotID != "12345" || gotPrice != 0.20 {
		t.Fatalf("ChangeBid(%q, %.2f), want (12345, 0.20)", gotID, gotPrice)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if ci.MaxBidPriceCents == nil || *ci.MaxBidPriceCents != onDemand {
		t.Fatalf("MaxBidPriceCents = %v, want %d", ci.MaxBidPriceCents, onDemand)
	}
	if ci.Status != db.LaunchStatusPaused {
		t.Fatalf("Status = %q, want %q", ci.Status, db.LaunchStatusPaused)
	}
}

func TestReconcileLaunches_GraceDetection(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// With nil r2Client, grace detection is skipped — no reconciliation
	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0 (nil r2Client)", result.Reconciled)
	}

	// Instance should still be running
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileLaunches_GraceIgnoredWithActiveJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	prevLogger := slog.Default()
	capture := weftlogging.NewCapturingHandler(slog.LevelWarn)
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job in "running" status associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix())

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// Override grace check to simulate R2 grace marker detection.
	// This should not be invoked while the launch has an active job.
	origGrace := reconcileCheckR2GraceStatus
	t.Cleanup(func() { reconcileCheckR2GraceStatus = origGrace })
	graceChecks := 0
	reconcileCheckR2GraceStatus = func(_ *r2.Client, ci *db.Launch, dbConn *sql.DB) bool {
		graceChecks++
		_ = db.SetLaunchGraceStarted(dbConn, ci.ID, time.Now().Add(5*time.Minute).Unix())
		return true
	}

	// Override job completion sync to track which jobs were synced
	var syncedJobIDs []int64
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, jobID int64) bool {
		syncedJobIDs = append(syncedJobIDs, jobID)
		return true
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}

	// Verify instance remains running
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}

	// Grace marker should be skipped while the job is active.
	if graceChecks != 0 {
		t.Errorf("grace checks = %d, want 0", graceChecks)
	}
	// The proactive completion sync checks R2 for .complete markers on every
	// reconcile pass, even when jobs appear active. This breaks the circular
	// dependency where jobs stay "running" because no one reads the marker.
	if len(syncedJobIDs) != 1 || syncedJobIDs[0] != 1 {
		t.Errorf("synced job IDs = %v, want [1]", syncedJobIDs)
	}
	for _, message := range capture.Messages() {
		if strings.Contains(message, "ignoring grace transition while launch has active jobs") {
			t.Fatalf("unexpected warning-level grace-transition log: %q", message)
		}
	}
}

func TestReconcileLaunches_GraceWhenNoActiveJobs(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	origGrace := reconcileCheckR2GraceStatus
	t.Cleanup(func() { reconcileCheckR2GraceStatus = origGrace })
	reconcileCheckR2GraceStatus = func(_ *r2.Client, ci *db.Launch, dbConn *sql.DB) bool {
		_ = db.SetLaunchGraceStarted(dbConn, ci.ID, time.Now().Add(5*time.Minute).Unix())
		return true
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusGrace {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusGrace)
	}
}

// Regression test: while the agent runs resubmitted jobs it rewrites the
// grace status marker with state "running" and a possibly stale
// (pre-extension) deadline. Reconcile must not treat that marker as grace —
// stamping the stale deadline would let the grace-expiry check force-destroy
// a working instance in the inter-job window.
func TestApplyGraceStatusMarker_RunningStateDoesNotEnterGrace(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}

	staleDeadline := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	marker := fmt.Sprintf(`{"state":"running","deadline":%q,"failed_jobs":[42]}`, staleDeadline)

	if applyGraceStatusMarker(marker, ci, database) {
		t.Fatal("applyGraceStatusMarker = true for state=running, want false")
	}

	ci, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
	if ci.GraceDeadline != nil {
		t.Errorf("GraceDeadline = %v, want nil (stale deadline must not be stamped)", *ci.GraceDeadline)
	}
}

func TestApplyGraceStatusMarker_WaitingStateEntersGrace(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	marker := fmt.Sprintf(`{"state":"waiting","deadline":%q}`, deadline.Format(time.RFC3339))

	if !applyGraceStatusMarker(marker, ci, database) {
		t.Fatal("applyGraceStatusMarker = false for state=waiting, want true")
	}

	ci, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusGrace {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusGrace)
	}
	if ci.GraceDeadline == nil {
		t.Fatal("GraceDeadline = nil, want marker deadline")
	}
	if *ci.GraceDeadline != deadline.Unix() {
		t.Errorf("GraceDeadline = %d, want %d", *ci.GraceDeadline, deadline.Unix())
	}
}

func TestReconcileLaunches_TerminalTransitionSyncsJobCompletions(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create a job in "running" status associated with this instance
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusRunning); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 1 AND end_time IS NULL`, time.Now().Unix())

	// Provider reports instance as exited (dead)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Override job completion sync to track calls and mark job completed in DB
	var syncedJobIDs []int64
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, dbConn *sql.DB, jobID int64) bool {
		syncedJobIDs = append(syncedJobIDs, jobID)
		// Simulate recording the completion so ExecuteAction's ResetLaunchJobs skips it
		dbConn.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
			db.StatusCompleted, time.Now().Unix(), jobID)
		return true
	}

	rec := NewReconciler()
	rec.deadConfirmTime = -1 // skip hysteresis
	result, err := rec.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify job completion sync was called for the running job
	if len(syncedJobIDs) != 1 || syncedJobIDs[0] != 1 {
		t.Errorf("synced job IDs = %v, want [1]", syncedJobIDs)
	}

	// Verify the job was NOT re-queued (it was marked completed by the mock sync)
	var jobStatus string
	database.QueryRow(`SELECT status FROM job_attempts WHERE job_id = 1 ORDER BY id DESC LIMIT 1`).Scan(&jobStatus)
	if jobStatus != string(db.StatusCompleted) {
		t.Errorf("job status = %q, want %q (should not be re-queued)", jobStatus, db.StatusCompleted)
	}
}

func TestReconcileLaunches_SyncFailureOrphansJob(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance with a provider ID
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "H100_SXM",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "33840670"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Create two jobs: one completed, one still running
	now := time.Now().Unix()
	for _, id := range []int{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, id); err != nil {
			t.Fatalf("create job %d: %v", id, err)
		}
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusCompleted); err != nil {
		t.Fatalf("create attempt 1: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ?, exit_code = 0 WHERE job_id = 1`, now-10, now)
	if _, err := db.CreateAttempt(database, 2, "", &instanceID, db.StatusRunning); err != nil {
		t.Fatalf("create attempt 2: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 2 AND end_time IS NULL`, now-10)

	// Provider reports instance as exited (dead)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Sync fails for job 2 (simulates the race: results not uploaded yet)
	origSync := reconcileCheckAndSyncJobComplete
	t.Cleanup(func() { reconcileCheckAndSyncJobComplete = origSync })
	reconcileCheckAndSyncJobComplete = func(_ context.Context, _ *r2.Client, _ *sql.DB, jobID int64) bool {
		return false // results not available
	}

	rec := NewReconciler()
	rec.deadConfirmTime = -1 // skip hysteresis
	_, err = rec.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Job 2 should be orphaned (reset to queued via requested_status)
	job2, err := db.GetJobByID(database, 2)
	if err != nil {
		t.Fatalf("get job 2: %v", err)
	}
	if job2.Status != db.StatusQueued {
		t.Errorf("job 2 status = %q, want %q (should be re-queued after orphaning)", job2.Status, db.StatusQueued)
	}

	// Check the orphaned attempt outcome on job 2's attempts
	attempts, _ := db.GetLaunchAttempts(database, 2)
	var hasOrphaned bool
	for _, a := range attempts {
		if a.Outcome == db.AttemptOutcomeOrphaned {
			hasOrphaned = true
		}
	}
	if !hasOrphaned {
		t.Errorf("expected an attempt with orphaned outcome for job 2")
	}
}

func TestReconcileLaunches_RunningInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a running instance
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that reports the instance as still running
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	// Reconcile — nothing should change
	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}
}

func TestReconcileLaunches_StaleHeartbeatWithoutAgentMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "stale-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	for _, jobID := range []int64{1, 2} {
		if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'python train.py', 0)`, jobID); err != nil {
			t.Fatalf("create job %d: %v", jobID, err)
		}
		if _, err := db.CreateAttempt(database, jobID, "", &instanceID, db.StatusQueued); err != nil {
			t.Fatalf("create attempt for job %d: %v", jobID, err)
		}
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})

	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, nil
	}

	var destroyedID string
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			status := cloud.ProviderStatusRunning
			if destroyed {
				status = cloud.ProviderStatusDestroyed
			}
			return &cloud.Instance{
				ProviderID: id,
				Status:     status,
				SSHHost:    "ssh6.vast.ai",
				SSHPort:    22,
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			destroyed = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}
	if destroyedID != "stale-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "stale-123")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}

	for _, jobID := range []int64{1, 2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.Status != db.StatusQueued {
			t.Fatalf("job %d status = %q, want %q", jobID, job.Status, db.StatusQueued)
		}
	}

	if got := countLifecycleEvents(t, database, db.EventReconcileStaleHeartbeat); got != 1 {
		t.Fatalf("stale-heartbeat lifecycle events = %d, want 1", got)
	}
}

// Regression: when the stale-heartbeat path cannot destroy the provider
// instance (provider API unreachable), the launch must NOT be marked
// failed and its jobs must NOT be requeued — the instance may still be
// running the job, and requeueing would double-run it against the
// abandoned attempt. The terminal transition is deferred to a later pass
// whose destroy succeeds, mirroring ExecuteAction.
func TestReconcileLaunches_StaleHeartbeatDestroyFailure_DefersFailedStatus(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "stale-999"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'python train.py', 0)`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusQueued); err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})
	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	// Probe succeeds and reports the agent gone — positive evidence, so
	// only the destroy failure holds the termination back.
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, nil
	}

	destroyFails := true
	destroyed := false
	var destroyCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			status := cloud.ProviderStatusRunning
			if destroyed {
				status = cloud.ProviderStatusDestroyed
			}
			return &cloud.Instance{ProviderID: id, Status: status}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalls++
			if destroyFails {
				return errors.New("provider API unreachable")
			}
			destroyed = true
			return nil
		},
	}

	r := NewReconciler()

	// Pass 1: destroy fails — launch must stay running, job must stay attached.
	result, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Fatalf("reconciled = %d, want 0 while destroy fails", result.Reconciled)
	}
	if destroyCalls != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyCalls)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Fatalf("instance status = %q, want still %q after failed destroy", ci.Status, db.LaunchStatusRunning)
	}
	job, err := db.GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("job launch = %v, want still attached to launch %d after failed destroy", job.LaunchID, instanceID)
	}

	// Pass 2: destroy succeeds — now the launch fails and the job requeues.
	destroyFails = false
	result, err = r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1 once destroy succeeds", result.Reconciled)
	}
	if destroyCalls != 2 {
		t.Fatalf("destroy calls = %d, want 2 (retried on the later pass)", destroyCalls)
	}
	ci, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	job, err = db.GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch = %d, want detached after requeue", *job.LaunchID)
	}
}

func TestReconcileLaunches_TerminationIntent_DestroysAndMarksFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "intent-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("assign job: %v", err)
	}

	origFetchIntent := syncFetchTermIntent
	t.Cleanup(func() {
		syncFetchTermIntent = origFetchIntent
	})
	syncFetchTermIntent = func(_ context.Context, _ *r2.Client, id int64) (*instanceintent.Marker, error) {
		if id != instanceID {
			return nil, nil
		}
		return &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonDiskFull,
			Phase:             "disk-full:246",
			JobID:             246,
			RequestedAtUnix:   time.Now().Unix(),
		}, nil
	}

	var destroyedID string
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			if destroyed {
				return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusDestroyed}, nil
			}
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			destroyed = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 2 {
		t.Fatalf("reconciled = %d, want 2", result.Reconciled)
	}
	if destroyedID != "intent-123" {
		t.Fatalf("DestroyInstance called with %q, want %q", destroyedID, "intent-123")
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonDiskFull {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonDiskFull)
	}
	if ci.TerminationRequestedAt == nil || *ci.TerminationRequestedAt == 0 {
		t.Fatalf("termination requested at = %v, want non-nil", ci.TerminationRequestedAt)
	}
	if ci.TerminationIntent == nil || ci.TerminationIntent.TerminationReason != db.TerminationReasonDiskFull {
		t.Fatalf("termination intent = %+v, want disk_full marker", ci.TerminationIntent)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}
}

func TestReconcileLaunches_SafetyNetMarksDestroyConfirmedWhenProviderGone(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusCompleted, db.TerminationReasonCompleted); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "intent-gone-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.UpdateLaunchTerminationIntent(database, instanceID, &instanceintent.Marker{
		TerminalStatus:       db.LaunchStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-30 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-25 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("set termination intent: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return nil, cloud.ErrInstanceNotFound
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.TerminationIntent == nil || ci.TerminationIntent.DestroySucceededAtUnix == 0 {
		t.Fatalf("termination intent = %+v, want destroy_succeeded_at_unix set", ci.TerminationIntent)
	}
}

func TestReconcileLaunches_StaleHeartbeatUnreachableProbeRequiresRepeatedFailures(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "probe-err-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("queue job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("assign job: %v", err)
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})

	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, context.DeadlineExceeded
	}

	var destroyCalls int
	destroyed := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			status := cloud.ProviderStatusRunning
			if destroyed {
				status = cloud.ProviderStatusDestroyed
			}
			return &cloud.Instance{ProviderID: id, Status: status}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalls++
			destroyed = true
			return nil
		},
	}

	reconciler := NewReconciler()
	for i := 0; i < minProbeFailureAttempts-1; i++ {
		result, err := reconciler.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", i+1, err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconcile attempt %d = %d, want 0 before threshold", i+1, result.Reconciled)
		}
	}

	// The attempt minimum alone is not enough: the next pass reaches
	// minProbeFailureAttempts but the failure window has not elapsed.
	result, err := reconciler.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile at attempt threshold: %v", err)
	}
	if result.Reconciled != 0 {
		t.Fatalf("reconcile at attempt threshold = %d, want 0 while window not elapsed", result.Reconciled)
	}

	// Backdate the failure-window start so both minimums are met.
	backdateProbeFailureWindow(reconciler, instanceID)

	result, err = reconciler.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile final attempt: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconcile final attempt = %d, want 1", result.Reconciled)
	}
	if destroyCalls != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyCalls)
	}
}

// backdateProbeFailureWindow rewinds the recorded first-failure time so a
// test can satisfy the minProbeFailureWindow elapsed-time minimum without
// sleeping.
func backdateProbeFailureWindow(r *Reconciler, instanceID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.probeFailures[instanceID]
	state.FirstAt = state.FirstAt.Add(-(minProbeFailureWindow + time.Second))
	r.probeFailures[instanceID] = state
}

// Regression: the elapsed window alone must not terminate either. Two probe
// failures spaced one reconcile tick apart satisfy minProbeFailureWindow
// (ticks run minutes apart in production) but not minProbeFailureAttempts —
// an observer-side SSH flap across two ticks must not kill the instance.
func TestReconcileLaunches_StaleHeartbeatProbeWindowAloneDoesNotTerminate(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "probe-window-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	origFetchHeartbeat := fetchReconcileHeartbeat
	origProbeCampaignAgent := probeCampaignAgent
	t.Cleanup(func() {
		fetchReconcileHeartbeat = origFetchHeartbeat
		probeCampaignAgent = origProbeCampaignAgent
	})
	fetchReconcileHeartbeat = func(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
		return &HeartbeatSample{Ts: time.Now().Add(-10 * time.Minute).Unix()}, 10 * time.Minute
	}
	probeCampaignAgent = func(inst *cloud.Instance, timeout time.Duration) (bool, error) {
		return false, context.DeadlineExceeded
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{ProviderID: id, Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			t.Errorf("DestroyInstance must not be called before the attempt minimum is met")
			return nil
		},
	}

	reconciler := NewReconciler()
	// First failure seeds the window; backdate it past minProbeFailureWindow
	// so the second failure arrives with the window elapsed but the attempt
	// count still below the minimum.
	if _, err := reconciler.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{}); err != nil {
		t.Fatalf("reconcile first failure: %v", err)
	}
	backdateProbeFailureWindow(reconciler, instanceID)

	result, err := reconciler.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, &r2.Client{})
	if err != nil {
		t.Fatalf("reconcile second failure: %v", err)
	}
	if result.Reconciled != 0 {
		t.Fatalf("reconciled = %d, want 0 with window elapsed but only 2 probe failures", result.Reconciled)
	}
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Fatalf("instance status = %q, want still %q", ci.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileLaunches_GraceExpiry_DestroysProvider(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a grace-period instance with an expired deadline.
	// GraceRequiresDeadline (00007) needs both fields at INSERT time.
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	graceStarted := pastDeadline
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:         db.LaunchStatusGrace,
		Provider:       "vastai",
		GPUSpec:        "RTX_4090",
		GraceStartedAt: &graceStarted,
		GraceDeadline:  &pastDeadline,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "99999"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.SetLaunchGraceStarted(database, instanceID, pastDeadline); err != nil {
		t.Fatalf("set grace started: %v", err)
	}

	// Track whether DestroyInstance was called
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1", result.Reconciled)
	}

	// Verify DestroyInstance was called with the correct provider ID
	if destroyedID != "99999" {
		t.Errorf("DestroyInstance called with %q, want %q", destroyedID, "99999")
	}

	// Verify instance is now failed
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
}

func TestReconcileLaunches_SafetyNet_DestroysLeakedInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "leaked-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	// Mark it as failed (this sets ended_at to now)
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's still alive, DestroyInstance tracks the call
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	// Reconcile — the main loop won't see this instance (it's already failed),
	// but the safety-net pass should catch and destroy it.
	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("reconciled = %d, want 1 (safety-net destroy)", result.Reconciled)
	}
	if destroyedID != "leaked-123" {
		t.Errorf("DestroyInstance called with %q, want %q", destroyedID, "leaked-123")
	}
}

func TestReconcileLaunches_SafetyNet_SkipsAlreadyDestroyed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an instance already marked as failed (recently)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "dead-456"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Mock client: ShowInstance reports it's already destroyed
	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusDestroyed}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0 (already destroyed)", result.Reconciled)
	}
	if destroyCalled {
		t.Error("DestroyInstance should not be called for already-destroyed instances")
	}
}

func TestReconcileLaunches_DeadInstanceHysteresis(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "12345"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return &cloud.Instance{Status: cloud.ProviderStatusExited}, nil
		},
	}

	// Use a short confirm time so the test doesn't need real wall-clock time.
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: 10 * time.Millisecond}

	// First call: instance appears dead but hasn't been confirmed yet.
	result, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (first): %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("first pass: reconciled = %d, want 0 (hysteresis pending)", result.Reconciled)
	}
	ci, _ := db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("first pass: instance status = %q, want running", ci.Status)
	}

	// Wait for confirm period to elapse.
	time.Sleep(20 * time.Millisecond)

	// Second call: now confirmed dead, should terminate.
	result, err = r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile (second): %v", err)
	}
	if result.Reconciled != 1 {
		t.Errorf("second pass: reconciled = %d, want 1", result.Reconciled)
	}
	ci, _ = db.GetLaunch(database, instanceID)
	if ci.Status != db.LaunchStatusFailed {
		t.Errorf("second pass: instance status = %q, want failed", ci.Status)
	}
}

func TestReconcileCampaigns_RunningCampaignWithNoInstancesBecomesFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns: %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("completed campaigns = %d, want 1", len(completed))
	}
	if completed[0].ID != campaignID {
		t.Fatalf("completed campaign ID = %d, want %d", completed[0].ID, campaignID)
	}
	if completed[0].Status != db.CampaignStatusFailed {
		t.Fatalf("completed campaign status = %q, want %q", completed[0].Status, db.CampaignStatusFailed)
	}

	got, err := db.GetCampaign(database, campaignID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got == nil {
		t.Fatalf("campaign %d not found", campaignID)
	}
	if got.Status != db.CampaignStatusFailed {
		t.Fatalf("campaign status = %q, want %q", got.Status, db.CampaignStatusFailed)
	}
	if got.EndedAt == nil {
		t.Fatal("ended_at was not set for failed campaign")
	}
}

func TestReconcileLaunches_TransientAPIError_SkipsInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "transient-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Mock client that returns a transient error (not ErrInstanceNotFound)
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return nil, fmt.Errorf("API timeout: connection reset")
		},
	}

	// Even with zero hysteresis, transient errors should NOT mark instance dead
	r := &Reconciler{firstDeadAt: make(map[int64]time.Time), lastProviderStatus: make(map[int64]string), deadConfirmTime: -1}

	// Run multiple reconciliation passes — instance must remain running
	for i := 0; i < 5; i++ {
		result, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
		if err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconcile pass %d: reconciled = %d, want 0 (transient error should skip)", i+1, result.Reconciled)
		}
	}

	// Verify instance is still running
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q (transient errors must not kill instance)", ci.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileLaunches_BatchFetch_UsesListAllInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "batch-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	var showCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "batch-123", Status: cloud.ProviderStatusRunning},
			}, nil
		},
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			showCalls++
			return &cloud.Instance{Status: cloud.ProviderStatusRunning}, nil
		},
	}

	result, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reconciled != 0 {
		t.Errorf("reconciled = %d, want 0", result.Reconciled)
	}
	if showCalls != 0 {
		t.Errorf("ShowInstance called %d times, want 0 (should use batch)", showCalls)
	}
}

// TestReconcileLaunches_BatchFetchMissCallsShowInstance verifies the
// authoritative-confirmation behavior: when an instance is absent from
// the batch list, the reconciler MUST call ShowInstance to confirm
// before treating the absence as evidence. Vast's batch endpoint omits
// live instances during transient API gaps; without ShowInstance,
// transient absence kills healthy rentals (regression: 2026-05-05
// wi2329/2331/2332). The instance must still NOT be killed in a single
// pass — dead-confirm hysteresis spans multiple consecutive misses.
func TestReconcileLaunches_BatchFetchMissCallsShowInstance(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	now := time.Now().Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:    db.LaunchStatusRunning,
		Provider:  "vastai",
		GPUSpec:   "RTX_4090",
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "missing-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, now, instanceID); err != nil {
		t.Fatalf("set launched at: %v", err)
	}

	var showCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{}, nil
		},
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			showCalls++
			return nil, cloud.ErrInstanceNotFound
		},
	}

	if _, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if showCalls == 0 {
		t.Errorf("ShowInstance called %d times, want >= 1 (batch miss must be confirmed per-instance)", showCalls)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q (single-pass miss should not kill)", launch.Status, db.LaunchStatusRunning)
	}
}

// TestReconcileLaunches_BatchFetchMissShowAlive verifies that when
// ShowInstance returns alive after a batch miss, the reconciler trusts
// it and the instance keeps running. Closes the false-kill path that
// motivated the heartbeat-first refactor.
func TestReconcileLaunches_BatchFetchMissShowAlive(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	now := time.Now().Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:    db.LaunchStatusRunning,
		Provider:  "vastai",
		GPUSpec:   "RTX_4090",
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "alive-456"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, now, instanceID); err != nil {
		t.Fatalf("set launched at: %v", err)
	}

	var showCalls int
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{}, nil
		},
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			showCalls++
			return &cloud.Instance{ProviderID: id, Provider: cloud.ProviderVastai, Status: cloud.ProviderStatusRunning}, nil
		},
	}

	if _, err := NewReconciler().ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if showCalls == 0 {
		t.Errorf("ShowInstance called %d times, want >= 1", showCalls)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q (ShowInstance reported alive)", launch.Status, db.LaunchStatusRunning)
	}
}

func TestReconcileCampaigns_MixedTerminalInstancesBecomeFailed(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	for _, status := range []string{db.LaunchStatusCompleted, db.LaunchStatusFailed} {
		if _, err := db.CreateLaunch(database, &db.Launch{
			CampaignID: &campaignID,
			Status:     status,
			Provider:   "vastai",
			GPUSpec:    "RTX_4090",
		}); err != nil {
			t.Fatalf("create cloud instance(%s): %v", status, err)
		}
	}

	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns: %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("completed campaigns = %d, want 1", len(completed))
	}
	if completed[0].Status != db.CampaignStatusFailed {
		t.Fatalf("campaign status = %q, want %q", completed[0].Status, db.CampaignStatusFailed)
	}
}

// TestReconcileCampaigns_StaysRunningWhileJobAwaitsRelaunch is a regression
// test for the premature-end bug: a campaign whose instances are all terminal
// must NOT be marked terminal while a job that ran on one of them has been
// requeued for relaunch — otherwise the autopilot relaunches that job into an
// already-ended campaign.
func TestReconcileCampaigns_StaysRunningWhileJobAwaitsRelaunch(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	// A job that ran on the failed instance and was requeued for relaunch:
	// requested_status='queued', with a closed orphaned attempt on the launch.
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned, requested_status) VALUES (1, '/tmp', 'python eval.py', 0, 'queued')`); err != nil {
		t.Fatalf("create job: %v", err)
	}
	attemptID, err := db.CreateAttempt(database, 1, "", &instanceID, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, cloud_outcome = ?, end_time = ? WHERE id = ?`,
		db.StatusCanceled, db.AttemptOutcomeOrphaned, time.Now().Unix(), attemptID,
	); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	// Phase 1: all instances terminal, but job 1 awaits relaunch.
	// The campaign must stay running.
	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns (phase 1): %v", err)
	}
	if len(completed) != 0 {
		t.Fatalf("phase 1: completed campaigns = %d, want 0 (job awaits relaunch)", len(completed))
	}
	c, err := db.GetCampaign(database, campaignID)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if c.Status != db.CampaignStatusRunning {
		t.Fatalf("phase 1: campaign status = %q, want %q", c.Status, db.CampaignStatusRunning)
	}

	// Phase 2: the job finishes. The campaign now ends — failed, because its
	// instance was failed.
	if _, err := database.Exec(`UPDATE jobs SET requested_status = NULL WHERE id = 1`); err != nil {
		t.Fatalf("clear requested_status: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = 0, cloud_outcome = ? WHERE id = ?`,
		db.StatusCompleted, db.AttemptOutcomeCompleted, attemptID,
	); err != nil {
		t.Fatalf("complete attempt: %v", err)
	}
	completed, err = ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns (phase 2): %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("phase 2: completed campaigns = %d, want 1", len(completed))
	}
	if completed[0].Status != db.CampaignStatusFailed {
		t.Fatalf("phase 2: campaign status = %q, want %q", completed[0].Status, db.CampaignStatusFailed)
	}
}

// TestReconcileCampaigns_IgnoresStaleHistoricalAttemptStatus guards that
// a job with a completed final attempt is treated as terminal, even when
// a historical attempt on one of the campaign's launches still echoes
// a non-terminal status via launch_job_membership.
func TestReconcileCampaigns_IgnoresStaleHistoricalAttemptStatus(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	// One terminal launch in this campaign; the job's first attempt ran here.
	failedLaunchID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusFailed,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create failed launch: %v", err)
	}
	// A separate, completed launch outside this campaign where the job's
	// later attempt eventually ran to completion.
	otherCampaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted})
	if err != nil {
		t.Fatalf("create other campaign: %v", err)
	}
	otherLaunchID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &otherCampaignID,
		Status:     db.LaunchStatusCompleted,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create other launch: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, command, tombstoned, requested_status) VALUES (1, '/tmp', 'echo', 0, NULL)`,
	); err != nil {
		t.Fatalf("create job: %v", err)
	}
	// Prior attempt: marked superseded with stale status='queued'. The
	// historical membership view echoes this as status='queued'.
	priorAttemptID, err := db.CreateAttempt(database, 1, "", &failedLaunchID, db.StatusQueued)
	if err != nil {
		t.Fatalf("create prior attempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?, end_time = ? WHERE id = ?`,
		db.AttemptOutcomeSuperseded, time.Now().Unix()-3600, priorAttemptID,
	); err != nil {
		t.Fatalf("supersede prior attempt: %v", err)
	}
	// Second attempt: on the other (completed) launch, the job actually
	// finished. job_status will report status='completed'.
	finalAttemptID, err := db.CreateAttempt(database, 1, "", &otherLaunchID, db.StatusQueued)
	if err != nil {
		t.Fatalf("create final attempt: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = 0, end_time = ?, cloud_outcome = ? WHERE id = ?`,
		db.StatusCompleted, time.Now().Unix(), db.AttemptOutcomeCompleted, finalAttemptID,
	); err != nil {
		t.Fatalf("complete final attempt: %v", err)
	}

	completed, err := ReconcileCampaigns(database)
	if err != nil {
		t.Fatalf("ReconcileCampaigns: %v", err)
	}
	// Campaign should now be terminal — the stale historical 'queued'
	// must not block it because the job has a completed final attempt.
	found := false
	for _, c := range completed {
		if c.ID == campaignID {
			found = true
			if c.Status != db.CampaignStatusFailed {
				t.Errorf("campaign %d status = %q, want %q (instance failed, job completed elsewhere)",
					campaignID, c.Status, db.CampaignStatusFailed)
			}
		}
	}
	if !found {
		c, _ := db.GetCampaign(database, campaignID)
		t.Fatalf("campaign %d not transitioned (status still %q); historical 'queued' attempt is wedging reconcile",
			campaignID, c.Status)
	}
}
