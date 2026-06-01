package cmd

import (
	"context"
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/syncorch"
)

func TestSyncHostWaitTimeout(t *testing.T) {
	if got := syncHostWaitTimeout(false); got != FastSyncHostTimeout {
		t.Fatalf("syncHostWaitTimeout(false) = %s, want %s", got, FastSyncHostTimeout)
	}
	if got := syncHostWaitTimeout(true); got != NormalSyncHostTimeout {
		t.Fatalf("syncHostWaitTimeout(true) = %s, want %s", got, NormalSyncHostTimeout)
	}
	if NormalSyncHostTimeout <= FastSyncHostTimeout {
		t.Fatalf("NormalSyncHostTimeout = %s, want > %s", NormalSyncHostTimeout, FastSyncHostTimeout)
	}
	if NormalSyncHostTimeout < 5*time.Minute {
		t.Fatalf("NormalSyncHostTimeout = %s, want a generous full-sync timeout", NormalSyncHostTimeout)
	}
}

func TestBase64EncodingPreservesSpecialCharacters(t *testing.T) {
	// Test that base64 encoding properly handles commands with shell-special characters
	// like parentheses which caused issues with nested shell quoting
	testCases := []struct {
		name    string
		command string
	}{
		{
			name:    "parentheses in codec names",
			command: `uv run compression-lab report --codecs "v2f-buckets(64),v2f-buckets(128)"`,
		},
		{
			name:    "single quotes",
			command: `echo 'hello world'`,
		},
		{
			name:    "double quotes",
			command: `echo "hello world"`,
		},
		{
			name:    "dollar signs",
			command: `echo $HOME && ls $TMPDIR`,
		},
		{
			name:    "backticks",
			command: "echo `date`",
		},
		{
			name:    "mixed special chars",
			command: `cd ~/dir && uv run script.py --opt="value(1)" --flag='test'`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			jobLine := "123\t/home/user\t" + tc.command + "\ttest description\t\t"

			// Encode and decode
			encoded := base64.StdEncoding.EncodeToString([]byte(jobLine))
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("failed to decode: %v", err)
			}

			if string(decoded) != jobLine {
				t.Errorf("roundtrip failed:\n  got:  %q\n  want: %q", string(decoded), jobLine)
			}

			// Verify encoded string has no shell-special characters
			shellSpecial := []string{"(", ")", "'", "\"", "$", "`", "\\", ";", "&", "|", "<", ">"}
			for _, char := range shellSpecial {
				if strings.Contains(encoded, char) {
					t.Errorf("encoded string contains shell-special character %q: %s", char, encoded)
				}
			}
		})
	}
}

func TestRecordCloudJobCompletion_ClosesAttempt(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	updatedInstanceID, err := db.RecordCloudJobCompletion(database, jobID, 0, 10, 20, "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}
	if updatedInstanceID != instanceID {
		t.Fatalf("updatedInstanceID = %d, want %d", updatedInstanceID, instanceID)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
	if job.LatestRunID == nil {
		t.Fatal("job latest_run_id = nil, want non-nil")
	}

	// Verify the attempt (latest_run_id now maps to job_attempts.id)
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
	if job.StartTime != 10 {
		t.Fatalf("job start_time = %d, want 10", job.StartTime)
	}
	if job.EndTime == nil || *job.EndTime != 20 {
		t.Fatalf("job end_time = %v, want 20", job.EndTime)
	}

	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != db.AttemptOutcomeCompleted {
		t.Fatalf("attempt outcome = %q, want %q", attempts[0].Outcome, db.AttemptOutcomeCompleted)
	}
	if attempts[0].EndedAt == nil {
		t.Fatal("attempt ended_at = nil, want non-nil")
	}
}

// TestRecordCloudJobCompletion_UpdatesClosedAttempt verifies that
// RecordCloudJobCompletion updates the latest attempt even when it already has
// end_time set (e.g., after cleanupStaleAttempts created a replacement).
func TestRecordCloudJobCompletion_UpdatesClosedAttempt(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Simulate cleanupStaleAttempts: close the original attempt and create a
	// new empty one (losing the launch_id).
	now := int64(1000)
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = 'canceled', end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		now, jobID,
	); err != nil {
		t.Fatalf("close attempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create replacement attempt: %v", err)
	}

	// Verify the job now shows as queued
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("pre-condition: job status = %q, want %q", job.Status, db.StatusQueued)
	}

	// RecordCloudJobCompletion should update the latest attempt (the new empty one)
	if _, err := db.RecordCloudJobCompletion(database, jobID, 0, 10, 20, "", time.Time{}, 0); err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}

	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after completion: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
}

// TestRecordCloudJobCompletion_InfersLaunchID verifies that when the latest
// attempt has no launch_id (blank replacement from cleanupStaleAttempts), the
// completion function infers the correct launch by finding a sibling launch
// that ran other jobs from the same original (failed) launch.
func TestRecordCloudJobCompletion_InfersLaunchID(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a failed launch (original assignment).
	failedLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
		GPUClass: "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch (failed): %v", err)
	}

	// Create a completed replacement launch with same GPU class.
	goodLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "A100",
		GPUClass: "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch (good): %v", err)
	}

	// Create two jobs, both originally assigned to the failed launch.
	jobA, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo A", "test", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	jobB, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo B", "test", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	// Assign both to failed launch.
	if err := db.SetJobLaunchID(database, jobA, failedLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobB, failedLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Simulate cleanup for jobA: cancel the failed-launch attempt, create blank replacement.
	database.Exec(
		`UPDATE job_attempts SET status = 'canceled', end_time = 1000 WHERE job_id = ? AND launch_id = ?`,
		jobA, failedLaunchID,
	)
	if _, err := db.CreateAttempt(database, jobA, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// JobB was properly reassigned to the good launch and completed.
	if _, err := db.CreateAttempt(database, jobB, "", &goodLaunchID, db.StatusCompleted); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Complete jobA — should infer launch from jobB's sibling relationship.
	returnedID, err := db.RecordCloudJobCompletion(database, jobA, 0, 200, 300, "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("RecordCloudJobCompletion: %v", err)
	}
	if returnedID != goodLaunchID {
		t.Errorf("returned launch_id = %d, want %d", returnedID, goodLaunchID)
	}

	job, err := db.GetJobByID(database, jobA)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != goodLaunchID {
		t.Errorf("job launch_id = %v, want %d", job.LaunchID, goodLaunchID)
	}
	if job.Host != "" {
		t.Errorf("job host = %q, want empty for rental job", job.Host)
	}
	if !job.IsLaunchJob() {
		t.Error("expected IsLaunchJob() = true after launch inference")
	}
}

func TestSyncCloudJobResults_RepairsFailedTerminalInstanceJobsWithoutR2(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, jobID); err != nil {
		t.Fatalf("set job running: %v", err)
	}

	updated := syncorch.SyncCloudJobResults(context.Background(), &config.Config{}, database, false)
	if updated != 1 {
		t.Fatalf("syncCloudJobResults updated %d rows, want 1", updated)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}

	outcomes, err := db.GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByLaunch: %v", err)
	}
	if outcomes[jobID] != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[jobID], db.AttemptOutcomeOrphaned)
	}
}

func TestAllowCompletedMarkerFallback_AllowsQueuedUnplacedPlaceholder(t *testing.T) {
	database := db.SetupTestDB(t)

	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, end_time = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCanceled, int64(1000), jobID,
	); err != nil {
		t.Fatalf("close attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusQueued, jobID); err != nil {
		t.Fatalf("set requested_status: %v", err)
	}

	var status string
	var currentLaunchID sql.NullInt64
	if err := database.QueryRow(`SELECT status, launch_id FROM job_status WHERE id = ?`, jobID).Scan(&status, &currentLaunchID); err != nil {
		t.Fatalf("query job_status: %v", err)
	}
	if status != db.StatusQueued {
		t.Fatalf("status = %q, want %q", status, db.StatusQueued)
	}
	if currentLaunchID.Valid {
		t.Fatalf("launch_id = %v, want NULL", currentLaunchID)
	}
	if !allowCompletedMarkerFallback(status, currentLaunchID, false) {
		t.Fatal("allowCompletedMarkerFallback = false, want true")
	}
}

func TestAllowCompletedMarkerFallback_BlocksRelaunchedJob(t *testing.T) {
	database := db.SetupTestDB(t)

	originalLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch (original): %v", err)
	}
	replacementLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch (replacement): %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, originalLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID (original): %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, end_time = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCanceled, int64(1000), jobID,
	); err != nil {
		t.Fatalf("close original attempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &replacementLaunchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt (replacement): %v", err)
	}

	var status string
	var currentLaunchID sql.NullInt64
	if err := database.QueryRow(`SELECT status, launch_id FROM job_status WHERE id = ?`, jobID).Scan(&status, &currentLaunchID); err != nil {
		t.Fatalf("query job_status: %v", err)
	}
	if status != db.StatusQueued {
		t.Fatalf("status = %q, want %q", status, db.StatusQueued)
	}
	if !currentLaunchID.Valid || currentLaunchID.Int64 != replacementLaunchID {
		t.Fatalf("launch_id = %v, want %d", currentLaunchID, replacementLaunchID)
	}
	if allowCompletedMarkerFallback(status, currentLaunchID, false) {
		t.Fatal("allowCompletedMarkerFallback = true, want false")
	}
}

// TestAllowCompletedMarkerFallback_AllowsTerminalBackfill verifies that
// terminal jobs needing backfill can use AnyCompletedKey to find a marker at
// an older run_id (the case where cleanupStaleAttempts advanced
// latest_run_id past the run that actually completed).
func TestAllowCompletedMarkerFallback_AllowsTerminalBackfill(t *testing.T) {
	launchID := sql.NullInt64{Int64: 42, Valid: true}
	if !allowCompletedMarkerFallback(db.StatusCompleted, launchID, true) {
		t.Fatal("allowCompletedMarkerFallback(completed, launched, needsBackfill=true) = false, want true")
	}
	if !allowCompletedMarkerFallback(db.StatusFailed, launchID, true) {
		t.Fatal("allowCompletedMarkerFallback(failed, launched, needsBackfill=true) = false, want true")
	}
	if allowCompletedMarkerFallback(db.StatusCompleted, launchID, false) {
		t.Fatal("allowCompletedMarkerFallback(completed, launched, needsBackfill=false) = true, want false")
	}
	if allowCompletedMarkerFallback(db.StatusRunning, launchID, true) {
		t.Fatal("allowCompletedMarkerFallback(running, launched, true) = true, want false (non-terminal)")
	}
}

func TestJobEligibleForStartedMarker_SkipsQueuedJobWithoutLaunch(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ?, launch_id = NULL, host = ''
		 WHERE job_id = ? AND end_time IS NULL`,
		db.StatusQueued, 123, jobID,
	); err != nil {
		t.Fatalf("seed job state: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (runID=%d)", runID)
	}
}

func TestJobEligibleForStartedMarker_SkipsQueuedJobOnFailedInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (runID=%d)", runID)
	}
}

func TestJobEligibleForStartedMarker_AllowsQueuedJobOnRunningInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if runID == 0 {
		t.Fatal("runID = 0, want non-zero latest run ID")
	}
}

func TestBuildStaleDataNote_OfflineOnly(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "alpha",
		LastUpdated: time.Now().Add(-2 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	note := buildStaleDataNote(database, []string{"alpha"}, nil)

	if note == "" {
		t.Fatal("note empty, want non-empty")
	}
	if !strings.Contains(note, "Could not reach host alpha") {
		t.Errorf("note = %q, want 'Could not reach host alpha'", note)
	}
	if strings.Contains(note, "currently offline") {
		t.Errorf("note still uses old phrasing: %q", note)
	}
	if strings.Contains(note, "ssh directly") {
		t.Errorf("note still claims ssh won't help: %q", note)
	}
}

func TestBuildStaleDataNote_SlowOnly(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "beta",
		LastUpdated: time.Now().Add(-2 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	note := buildStaleDataNote(database, nil, []string{"beta"})

	if note == "" {
		t.Fatal("note empty, want non-empty")
	}
	if !strings.Contains(note, "host beta") {
		t.Errorf("note = %q, want mention of beta", note)
	}
	if !strings.Contains(note, "may be reachable but slow") {
		t.Errorf("note = %q, want slow phrasing", note)
	}
	if strings.Contains(note, "Could not reach") {
		t.Errorf("note uses offline phrasing for slow host: %q", note)
	}
}

func TestBuildStaleDataNote_OfflineAndSlow(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	for _, h := range []string{"alpha", "beta"} {
		if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
			Name:        h,
			LastUpdated: now.Add(-2 * time.Minute).Unix(),
		}); err != nil {
			t.Fatalf("SaveCachedHostInfo %s: %v", h, err)
		}
	}

	note := buildStaleDataNote(database, []string{"alpha"}, []string{"beta"})

	if !strings.Contains(note, "Could not reach host alpha") {
		t.Errorf("missing offline clause: %q", note)
	}
	if !strings.Contains(note, "host beta") || !strings.Contains(note, "may be reachable but slow") {
		t.Errorf("missing slow clause: %q", note)
	}
}

func TestBuildStaleDataNote_DedupesHostInBoth(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "alpha",
		LastUpdated: time.Now().Add(-2 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	// A host that appears in both slices should be reported as unreachable only.
	note := buildStaleDataNote(database, []string{"alpha"}, []string{"alpha"})

	if !strings.Contains(note, "Could not reach host alpha") {
		t.Errorf("missing offline clause: %q", note)
	}
	if strings.Contains(note, "may be reachable but slow") {
		t.Errorf("alpha appears in slow clause too: %q", note)
	}
}

func TestBuildStaleDataNote_EmptyInputsReturnsEmpty(t *testing.T) {
	database := db.SetupTestDB(t)
	if got := buildStaleDataNote(database, nil, nil); got != "" {
		t.Errorf("note = %q, want empty", got)
	}
}

func TestHostSyncWarningsIncludesQueueDispatchFailure(t *testing.T) {
	warnings := hostSyncWarnings("studio", ops.HostSyncResult{
		QueueDispatchError: "job 289 input staging failed: context deadline exceeded",
	})

	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], "queued jobs were not dispatched on studio") {
		t.Fatalf("warning = %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "job 289 input staging failed") {
		t.Fatalf("warning = %q", warnings[0])
	}
}
