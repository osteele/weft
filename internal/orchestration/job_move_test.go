package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestResolveEligibleJobs_FromHostSuppressesStatusWarnings(t *testing.T) {
	database := db.SetupTestDB(t)

	queuedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	completedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo done", "done")
	if err != nil {
		t.Fatalf("record completed job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), completedJobID,
	); err != nil {
		t.Fatalf("mark completed attempt: %v", err)
	}

	var warnings []string
	jobs, err := ResolveEligibleJobs(database, nil, "", "cool30", false, JobMoveCallbacks{
		OnWarning: func(message string) {
			warnings = append(warnings, message)
		},
	})
	if err != nil {
		t.Fatalf("ResolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != queuedJobID {
		t.Fatalf("eligible jobs = %#v, want only queued job %d", jobs, queuedJobID)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestResolveEligibleJobs_ExplicitJobIDsWarnOnSkippedStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	queuedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	completedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo done", "done")
	if err != nil {
		t.Fatalf("record completed job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), completedJobID,
	); err != nil {
		t.Fatalf("mark completed attempt: %v", err)
	}

	var warnings []string
	jobs, err := ResolveEligibleJobs(database, []int64{queuedJobID, completedJobID}, "", "", false, JobMoveCallbacks{
		OnWarning: func(message string) {
			warnings = append(warnings, message)
		},
	})
	if err != nil {
		t.Fatalf("ResolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != queuedJobID {
		t.Fatalf("eligible jobs = %#v, want only queued job %d", jobs, queuedJobID)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings len = %d, want 1 (warnings=%v)", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "job "+ids.FormatJobID(completedJobID)+" has status completed, skipping") {
		t.Fatalf("warning = %q, want completed-status skip warning", warnings[0])
	}
}

func TestResolveEligibleJobsRejectsExternalExecutorJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "PENDING", NormalizedStatus: db.StatusQueued, Command: "python train.py",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	var warnings []string
	jobs, err := ResolveEligibleJobs(database, []int64{binding.JobID}, "", "", false, JobMoveCallbacks{
		OnWarning: func(message string) { warnings = append(warnings, message) },
	})
	if err == nil || !strings.Contains(err.Error(), "no eligible") {
		t.Fatalf("ResolveEligibleJobs = %v, %v; want no eligible jobs", jobs, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "external executor") {
		t.Fatalf("warnings = %v, want external-executor refusal", warnings)
	}
}

func TestResolveEligibleJobs_IncludesJobsWithOpenMoveIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	movingJobID, err := db.RecordQueued(database, "cool100", t.TempDir(), "echo moving", "moving")
	if err != nil {
		t.Fatalf("record moving job: %v", err)
	}
	queuedJobID, err := db.RecordQueued(database, "cool100", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:      movingJobID,
		TargetKind: db.MoveTargetNew,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	var warnings []string
	jobs, err := ResolveEligibleJobs(database, nil, "", "cool100", false, JobMoveCallbacks{
		OnWarning: func(message string) {
			warnings = append(warnings, message)
		},
	})
	if err != nil {
		t.Fatalf("ResolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("eligible jobs len = %d, want 2", len(jobs))
	}
	got := map[int64]bool{jobs[0].ID: true, jobs[1].ID: true}
	if !got[movingJobID] || !got[queuedJobID] {
		t.Fatalf("eligible IDs = %v, want moving %d and queued %d", got, movingJobID, queuedJobID)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestMoveJobsToHost_SupersedesOpenMoveIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool100", t.TempDir(), "echo move", "move")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: db.MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	moved, err := MoveJobsToHost(database, []*db.Job{job}, "cool30", JobMoveCallbacks{})
	if err != nil {
		t.Fatalf("MoveJobsToHost: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	gotIntent, err := db.GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if gotIntent.State != db.MoveIntentStateCanceled || gotIntent.Resolution != "superseded by explicit move" {
		t.Fatalf("intent = (%s, %q), want canceled superseded", gotIntent.State, gotIntent.Resolution)
	}
	movedJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if movedJob.Host != "cool30" {
		t.Fatalf("host = %q, want cool30", movedJob.Host)
	}
}

func TestMoveJobsToHost_SupersedesOpenPlacementIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool100", t.TempDir(), "echo move", "move")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	intent, err := db.CreatePlacementIntent(database, jobID, "bulk_move")
	if err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	moved, err := MoveJobsToHost(database, []*db.Job{job}, "cool30", JobMoveCallbacks{})
	if err != nil {
		t.Fatalf("MoveJobsToHost: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	gotIntent, err := db.GetPlacementIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetPlacementIntent: %v", err)
	}
	if gotIntent.State != db.PlacementIntentStateCanceled || gotIntent.Resolution != "superseded by explicit move" {
		t.Fatalf("intent = (%s, %q), want canceled superseded", gotIntent.State, gotIntent.Resolution)
	}
}

func TestResolveEligibleJobs_ProjectSelectsQueuedAndPendingPlacement(t *testing.T) {
	database := db.SetupTestDB(t)

	queuedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobProject(database, queuedJobID, "alpha"); err != nil {
		t.Fatalf("set project on queued job: %v", err)
	}

	pendingJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo pending", "pending")
	if err != nil {
		t.Fatalf("record pending job: %v", err)
	}
	if err := db.SetJobProject(database, pendingJobID, "alpha"); err != nil {
		t.Fatalf("set project on pending job: %v", err)
	}
	if err := db.SetPendingStatus(database, pendingJobID, db.StatusPendingPlacement); err != nil {
		t.Fatalf("set pending_placement status: %v", err)
	}

	completedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo done", "done")
	if err != nil {
		t.Fatalf("record completed job: %v", err)
	}
	if err := db.SetJobProject(database, completedJobID, "alpha"); err != nil {
		t.Fatalf("set project on completed job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), completedJobID,
	); err != nil {
		t.Fatalf("mark completed attempt: %v", err)
	}

	otherProjectJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo other", "other")
	if err != nil {
		t.Fatalf("record other-project job: %v", err)
	}
	if err := db.SetJobProject(database, otherProjectJobID, "beta"); err != nil {
		t.Fatalf("set project on other-project job: %v", err)
	}

	var warnings []string
	jobs, err := ResolveEligibleJobs(database, nil, "alpha", "", false, JobMoveCallbacks{
		OnWarning: func(message string) {
			warnings = append(warnings, message)
		},
	})
	if err != nil {
		t.Fatalf("ResolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("eligible jobs len = %d, want 2", len(jobs))
	}
	ids := map[int64]bool{jobs[0].ID: true, jobs[1].ID: true}
	if !ids[queuedJobID] || !ids[pendingJobID] {
		t.Fatalf("eligible IDs = %v, want queued %d and pending %d", ids, queuedJobID, pendingJobID)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestResolveEligibleJobs_FromInstanceUnknownReportsNotFound(t *testing.T) {
	database := db.SetupTestDB(t)

	_, err := ResolveEligibleJobs(database, nil, "", "wi9999", false, JobMoveCallbacks{})
	if err == nil {
		t.Fatalf("ResolveEligibleJobs: want error, got nil")
	}
	want := "instance " + ids.FormatInstanceID(9999) + " not found or has no associated jobs"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestResolveEligibleJobs_FromInstanceOnlyHistoricalReportsHistorical(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo historical", "historical")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// ResetLaunchJobs clears LaunchID on the job and the attempt, leaving only
	// the launch_job_membership row — i.e. a historical attempt with no
	// currently-assigned job.
	if _, err := db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}

	_, err = ResolveEligibleJobs(database, nil, "", ids.FormatInstanceID(instanceID), false, JobMoveCallbacks{})
	if err == nil {
		t.Fatalf("ResolveEligibleJobs: want error, got nil")
	}
	want := "instance " + ids.FormatInstanceID(instanceID) + " has 1 historical job attempt(s) but no currently-assigned jobs"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestResolveEligibleJobs_FromInstanceNonQueuedReportsStatusBreakdown(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo completed", "completed")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// Mark the open attempt completed so the job's effective status is no
	// longer queued/pending_placement, but the job remains currently assigned
	// to the instance (LaunchID still points there via the open attempt was
	// closed; job_status reads from the latest attempt). Using completed
	// rather than running keeps the test independent of the view's
	// transient-state remapping logic.
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusCompleted, time.Now().Unix(), jobID,
	); err != nil {
		t.Fatalf("mark completed attempt: %v", err)
	}

	_, err = ResolveEligibleJobs(database, nil, "", ids.FormatInstanceID(instanceID), false, JobMoveCallbacks{})
	if err == nil {
		t.Fatalf("ResolveEligibleJobs: want error, got nil")
	}
	got := err.Error()
	wantPrefix := "instance " + ids.FormatInstanceID(instanceID) + " has 1 job(s) but none are queued or pending_placement"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("error = %q, want prefix %q", got, wantPrefix)
	}
	// The status breakdown is parenthesized; require it to be present and
	// non-trivial. The exact status label is what the job_status view
	// projects (e.g. "completed" → "dead" for an instance whose launch is
	// not in a terminal state); this test does not pin that mapping.
	if !strings.Contains(got, "(") || !strings.Contains(got, ": 1)") {
		t.Fatalf("error = %q, want a `(<status>: 1)` breakdown", got)
	}
}
