package db

import "testing"

func TestLatestAttemptOutcomeEventsIncludesDerivedFreshOrphan(t *testing.T) {
	database := SetupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1001, "echo test", "/tmp", StatusQueued, withLaunch(launchID))
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`,
		LaunchStatusFailed, int64(9_400), TerminationReasonPhaseStall, launchID,
	); err != nil {
		t.Fatalf("fail launch: %v", err)
	}

	events, err := LatestAttemptOutcomeEvents(database, []int64{1001})
	if err != nil {
		t.Fatalf("LatestAttemptOutcomeEvents: %v", err)
	}
	event, ok := events[1001]
	if !ok {
		t.Fatalf("missing derived orphan event")
	}
	if event.Outcome != AttemptOutcomeOrphaned || event.AtUnix != 9_400 {
		t.Fatalf("event = %+v, want orphaned at 9400", event)
	}
}

func TestLatestAttemptOutcomeEventsUsesLaunchEndForStampedOrphan(t *testing.T) {
	database := SetupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	insertTestJob(t, database, 1002, "echo test", "/tmp", StatusQueued, withLaunch(launchID))
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`,
		LaunchStatusFailed, int64(9_900), TerminationReasonPhaseStall, launchID,
	); err != nil {
		t.Fatalf("fail launch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET end_time = ?, cloud_outcome = ? WHERE job_id = ?`,
		int64(100), AttemptOutcomeOrphaned, int64(1002),
	); err != nil {
		t.Fatalf("stamp old orphan attempt: %v", err)
	}

	events, err := LatestAttemptOutcomeEvents(database, []int64{1002})
	if err != nil {
		t.Fatalf("LatestAttemptOutcomeEvents: %v", err)
	}
	event, ok := events[1002]
	if !ok {
		t.Fatalf("missing stamped orphan event")
	}
	if event.Outcome != AttemptOutcomeOrphaned || event.AtUnix != 9_900 {
		t.Fatalf("event = %+v, want orphaned at launch end 9900", event)
	}
}
