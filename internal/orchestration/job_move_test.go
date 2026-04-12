package orchestration

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
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
	if !strings.Contains(warnings[0], "job "+strconv.FormatInt(completedJobID, 10)+" has status completed, skipping") {
		t.Fatalf("warning = %q, want completed-status skip warning", warnings[0])
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
