package db

import (
	"testing"
	"time"
)

func TestSetAttemptLaunch_NormalizesPendingPlacement(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(1001)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	if err := SetAttemptPendingStatus(database, jobID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if err := SetAttemptLaunch(database, jobID, launchID); err != nil {
		t.Fatalf("SetAttemptLaunch: %v", err)
	}

	var gotLaunchID int64
	var gotHost, gotPending string
	if err := database.QueryRow(
		`SELECT COALESCE(launch_id, 0), host, COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&gotLaunchID, &gotHost, &gotPending); err != nil {
		t.Fatalf("query attempt: %v", err)
	}

	if gotLaunchID != launchID {
		t.Fatalf("launch_id = %d, want %d", gotLaunchID, launchID)
	}
	if gotHost != LaunchHost(launchID) {
		t.Fatalf("host = %q, want %q", gotHost, LaunchHost(launchID))
	}
	if gotPending != StatusQueued {
		t.Fatalf("pending_status = %q, want %q", gotPending, StatusQueued)
	}
}

func TestNormalizePendingPlacementForLaunch_SkipsCompletedNonOrphaned(t *testing.T) {
	database := setupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	plannedLaunchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusPlanned})
	if err != nil {
		t.Fatalf("CreateLaunch(planned): %v", err)
	}

	jobQueued := int64(2001)
	insertTestJob(t, database, jobQueued, "echo queued", "/tmp", StatusQueued, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobQueued, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(queued): %v", err)
	}

	jobCompleted := int64(2002)
	insertTestJob(t, database, jobCompleted, "echo completed", "/tmp", StatusCompleted, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobCompleted, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(completed): %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ? WHERE job_id = ?`, AttemptOutcomeCompleted, jobCompleted); err != nil {
		t.Fatalf("set cloud_outcome completed: %v", err)
	}

	jobOrphaned := int64(2003)
	insertTestJob(t, database, jobOrphaned, "echo orphaned", "/tmp", StatusCompleted, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, jobOrphaned, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(orphaned): %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET cloud_outcome = ? WHERE job_id = ?`, AttemptOutcomeOrphaned, jobOrphaned); err != nil {
		t.Fatalf("set cloud_outcome orphaned: %v", err)
	}

	jobPlanned := int64(2004)
	insertTestJob(t, database, jobPlanned, "echo planned", "/tmp", StatusQueued, withLaunch(plannedLaunchID))
	if err := SetAttemptPendingStatus(database, jobPlanned, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(planned): %v", err)
	}

	normalized, err := NormalizePendingPlacementForLaunch(database, launchID)
	if err != nil {
		t.Fatalf("NormalizePendingPlacementForLaunch: %v", err)
	}
	if normalized != 2 {
		t.Fatalf("normalized = %d, want 2", normalized)
	}

	checkPending := func(jobID int64, want string) {
		t.Helper()
		var got string
		if err := database.QueryRow(
			`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
			jobID,
		).Scan(&got); err != nil {
			t.Fatalf("query pending_status for job %d: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("job %d pending_status = %q, want %q", jobID, got, want)
		}
	}

	checkPending(jobQueued, StatusQueued)
	checkPending(jobCompleted, StatusPendingPlacement)
	checkPending(jobOrphaned, StatusQueued)
	checkPending(jobPlanned, StatusPendingPlacement)
}

func TestNormalizeStalePendingPlacementNoLaunch(t *testing.T) {
	database := setupTestDB(t)

	staleID := int64(3001)
	insertTestJob(t, database, staleID, "echo stale", "/tmp", StatusQueued)
	if err := SetAttemptPendingStatus(database, staleID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(stale): %v", err)
	}
	stalePendingAt := time.Now().Unix() - stalePendingPlacementNoLaunchMaxAgeSeconds - 10
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		stalePendingAt, staleID,
	); err != nil {
		t.Fatalf("set stale pending_at: %v", err)
	}

	freshID := int64(3002)
	insertTestJob(t, database, freshID, "echo fresh", "/tmp", StatusQueued)
	if err := SetAttemptPendingStatus(database, freshID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(fresh): %v", err)
	}
	freshPendingAt := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		freshPendingAt, freshID,
	); err != nil {
		t.Fatalf("set fresh pending_at: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	anchoredID := int64(3003)
	insertTestJob(t, database, anchoredID, "echo anchored", "/tmp", StatusQueued, withLaunch(launchID))
	if err := SetAttemptPendingStatus(database, anchoredID, StatusPendingPlacement); err != nil {
		t.Fatalf("SetAttemptPendingStatus(anchored): %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET pending_at = ? WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		stalePendingAt, anchoredID,
	); err != nil {
		t.Fatalf("set anchored pending_at: %v", err)
	}

	updated, err := NormalizeStalePendingPlacementNoLaunch(database)
	if err != nil {
		t.Fatalf("NormalizeStalePendingPlacementNoLaunch: %v", err)
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	checkPending := func(jobID int64, want string) {
		t.Helper()
		var got string
		if err := database.QueryRow(
			`SELECT COALESCE(pending_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
			jobID,
		).Scan(&got); err != nil {
			t.Fatalf("query pending_status for job %d: %v", jobID, err)
		}
		if got != want {
			t.Fatalf("job %d pending_status = %q, want %q", jobID, got, want)
		}
	}

	checkPending(staleID, StatusQueued)
	checkPending(freshID, StatusPendingPlacement)
	checkPending(anchoredID, StatusPendingPlacement)
}

func TestSetJobLaunchID_PreservesCloudDependencyMetadata(t *testing.T) {
	database := setupTestDB(t)

	jobID := int64(4001)
	insertTestJob(t, database, jobID, "echo stage", "/tmp", StatusQueued)
	meta := &JobMetadata{
		Dependencies: &JobDependencyMetadata{
			CloudNeeds: []string{"output/model.pt:1046"},
			CloudAfter: []JobDependencyRef{{JobID: 1046}},
		},
	}
	if err := SetJobMetadata(database, jobID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Metadata == nil || job.Metadata.Dependencies == nil {
		t.Fatalf("expected dependency metadata, got %+v", job)
	}
	if got := job.Metadata.Dependencies.CloudNeeds; len(got) != 1 || got[0] != "output/model.pt:1046" {
		t.Fatalf("cloud_needs = %v", got)
	}
	if got := job.Metadata.Dependencies.CloudAfter; len(got) != 1 || got[0].JobID != 1046 {
		t.Fatalf("cloud_after = %v", got)
	}
}
