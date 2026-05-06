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

	counts, err := loadUnprocessedCounts(database, "")
	if err != nil {
		t.Fatalf("loadUnprocessedCounts: %v", err)
	}
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", counts.Failed)
	}
	if got := counts.CompletedProjects; len(got) != 1 || got[0] != "augur" {
		t.Fatalf("CompletedProjects = %v, want [augur]", got)
	}
	if got := counts.FailedProjects; len(got) != 1 || got[0] != "augur" {
		t.Fatalf("FailedProjects = %v, want [augur]", got)
	}
}
