package cmd

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestLoadUnprocessedCountsIncludesTerminalProjects(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()

	completedAugur, err := db.RecordQueued(database, "cool30", "/tmp/augur", "echo ok", "completed")
	if err != nil {
		t.Fatalf("record completed augur: %v", err)
	}
	if err := db.SetJobProject(database, completedAugur, "augur"); err != nil {
		t.Fatalf("set completed augur project: %v", err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, completedAugur, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close completed augur: %v", err)
	}

	completedUnset, err := db.RecordQueued(database, "cool30", "/tmp", "echo ok", "completed unset")
	if err != nil {
		t.Fatalf("record completed unset: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET project = '' WHERE id = ?`, completedUnset); err != nil {
		t.Fatalf("clear completed unset project: %v", err)
	}
	if err := db.CloseAttempt(database, completedUnset, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close completed unset: %v", err)
	}

	failedAugur, err := db.RecordQueued(database, "cool30", "/tmp/augur", "false", "failed")
	if err != nil {
		t.Fatalf("record failed augur: %v", err)
	}
	if err := db.SetJobProject(database, failedAugur, "augur"); err != nil {
		t.Fatalf("set failed augur project: %v", err)
	}
	exitOne := 1
	if err := db.CloseAttempt(database, failedAugur, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatalf("close failed augur: %v", err)
	}

	completedBad, err := db.RecordQueued(database, "cool30", "/tmp/bad-project", "false", "completed bad")
	if err != nil {
		t.Fatalf("record completed bad: %v", err)
	}
	if err := db.SetJobProject(database, completedBad, "bad-project"); err != nil {
		t.Fatalf("set completed bad project: %v", err)
	}
	if err := db.CloseAttempt(database, completedBad, db.StatusCompleted, &exitOne, now); err != nil {
		t.Fatalf("close completed bad: %v", err)
	}

	killedJob, err := db.RecordQueued(database, "cool30", "/tmp/ignored", "sleep 10", "killed")
	if err != nil {
		t.Fatalf("record killed job: %v", err)
	}
	if err := db.SetJobProject(database, killedJob, "ignored-project"); err != nil {
		t.Fatalf("set killed project: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusKilled, now, killedJob); err != nil {
		t.Fatalf("mark killed: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusKilled, killedJob); err != nil {
		t.Fatalf("request killed: %v", err)
	}

	canceledJob, err := db.RecordQueued(database, "cool30", "/tmp/ignored", "sleep 10", "canceled")
	if err != nil {
		t.Fatalf("record canceled job: %v", err)
	}
	if err := db.SetJobProject(database, canceledJob, "ignored-project"); err != nil {
		t.Fatalf("set canceled project: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusCanceled, now, canceledJob); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusCanceled, canceledJob); err != nil {
		t.Fatalf("request canceled: %v", err)
	}

	processedFailed, err := db.RecordQueued(database, "cool30", "/tmp/processed", "false", "processed failed")
	if err != nil {
		t.Fatalf("record processed failed: %v", err)
	}
	if err := db.SetJobProject(database, processedFailed, "processed-project"); err != nil {
		t.Fatalf("set processed failed project: %v", err)
	}
	if err := db.CloseAttempt(database, processedFailed, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatalf("close processed failed: %v", err)
	}
	if err := db.AddJobTag(database, processedFailed, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed failed: %v", err)
	}

	processedCompleted, err := db.RecordQueued(database, "cool30", "/tmp/processed", "echo ok", "processed completed")
	if err != nil {
		t.Fatalf("record processed completed: %v", err)
	}
	if err := db.SetJobProject(database, processedCompleted, "processed-project"); err != nil {
		t.Fatalf("set processed completed project: %v", err)
	}
	if err := db.CloseAttempt(database, processedCompleted, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close processed completed: %v", err)
	}
	if err := db.AddJobTag(database, processedCompleted, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed completed: %v", err)
	}

	counts, err := loadUnprocessedCounts(database, "")
	if err != nil {
		t.Fatalf("loadUnprocessedCounts: %v", err)
	}
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", counts.Failed)
	}
	if got := counts.CompletedProjects; len(got) != 1 || got[0] != "augur" {
		t.Fatalf("CompletedProjects = %v, want [augur]", got)
	}
	if got := counts.FailedProjects; len(got) != 2 || got[0] != "augur" || got[1] != "bad-project" {
		t.Fatalf("FailedProjects = %v, want [augur bad-project]", got)
	}
}
