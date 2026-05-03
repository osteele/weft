package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestListRecentFailedCloudLaunches_WindowCutoff(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()

	now := time.Now().Unix()
	insertFailedLaunch(t, database, now-100, TerminationReasonProviderFailure)  // in window
	insertFailedLaunch(t, database, now-50, TerminationReasonBootstrapTimeout)  // in window
	insertFailedLaunch(t, database, now-3600, TerminationReasonProviderFailure) // outside window

	out, err := ListRecentFailedCloudLaunches(database, now-300)
	if err != nil {
		t.Fatalf("ListRecentFailedCloudLaunches: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 in-window launches, got %d", len(out))
	}
	if out[0].EndedAt == nil || out[1].EndedAt == nil {
		t.Fatalf("ended_at must be set on returned rows")
	}
	if *out[0].EndedAt < *out[1].EndedAt {
		t.Fatalf("expected ended_at desc, got %d before %d", *out[0].EndedAt, *out[1].EndedAt)
	}
}

func TestLaunchSuccessorsRecovered(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()
	now := time.Now().Unix()

	failedID := insertFailedLaunch(t, database, now-100, TerminationReasonProviderFailure)
	failedNoSuccessorID := insertFailedLaunch(t, database, now-90, TerminationReasonProviderFailure)
	failedFailedSuccessorID := insertFailedLaunch(t, database, now-80, TerminationReasonProviderFailure)

	insertSuccessorLaunch(t, database, failedID, LaunchStatusRunning, now)
	insertSuccessorLaunch(t, database, failedFailedSuccessorID, LaunchStatusFailed, now)

	got, err := LaunchSuccessorsRecovered(database, []int64{failedID, failedNoSuccessorID, failedFailedSuccessorID})
	if err != nil {
		t.Fatalf("LaunchSuccessorsRecovered: %v", err)
	}
	if !got[failedID] {
		t.Fatalf("expected failedID %d marked recovered", failedID)
	}
	if got[failedNoSuccessorID] {
		t.Fatalf("expected failedNoSuccessorID %d NOT recovered", failedNoSuccessorID)
	}
	if got[failedFailedSuccessorID] {
		t.Fatalf("expected failedFailedSuccessorID %d NOT recovered (successor itself failed)", failedFailedSuccessorID)
	}
}

func insertSuccessorLaunch(t *testing.T, database *sql.DB, predecessorID int64, status string, createdAt int64) int64 {
	t.Helper()
	res, err := database.Exec(
		`INSERT INTO launches (status, provider, created_at, replaced_instance_id) VALUES (?, ?, ?, ?)`,
		status, "vastai", createdAt, predecessorID,
	)
	if err != nil {
		t.Fatalf("insert successor launch: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func insertFailedLaunch(t *testing.T, database *sql.DB, endedAt int64, reason string) int64 {
	t.Helper()
	res, err := database.Exec(
		`INSERT INTO launches (status, provider, created_at, ended_at, termination_reason)
		 VALUES (?, ?, ?, ?, ?)`,
		LaunchStatusFailed, "vastai", endedAt-10, endedAt, reason,
	)
	if err != nil {
		t.Fatalf("insert failed launch: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}
