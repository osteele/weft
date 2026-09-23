package db

import (
	"fmt"
	"testing"
)

func TestFailCloudDependencyPreservesNewerJobIntent(t *testing.T) {
	for _, intent := range []string{StatusCanceled, StatusDraft, "new placement", "open move"} {
		t.Run(intent, func(t *testing.T) {
			database := SetupTestDB(t)
			id, err := RecordQueued(database, "", "/tmp/project", "consume", "consumer")
			if err != nil {
				t.Fatal(err)
			}
			observed, err := GetJobByID(database, id)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := intent
			switch intent {
			case "new placement", "open move":
				launchID := mustCreateLaunch(t, database)
				if err := SetJobLaunchID(database, id, launchID); err != nil {
					t.Fatal(err)
				}
				wantStatus = StatusQueued
				if intent == "open move" {
					observed, err = GetJobByID(database, id)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := CreateMoveIntent(database, CreateMoveIntentParams{JobID: id, TargetKind: MoveTargetNew}); err != nil {
						t.Fatal(err)
					}
				}
			default:
				if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, intent, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := FailCloudDependency(database, observed, "required artifact was not published"); err != nil {
				t.Fatal(err)
			}
			got, err := GetJobByID(database, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != wantStatus || got.FailureReason != "" {
				t.Fatalf("new intent overwritten: status=%s reason=%s", got.Status, got.FailureReason)
			}
		})
	}
}

func TestCancelLaunchForCloudDependencyIsAtomic(t *testing.T) {
	database := SetupTestDB(t)
	launchID := mustCreateLaunch(t, database)
	insertTestJob(t, database, 100, "consume", "/tmp/project", StatusQueued, withLaunch(launchID))
	insertTestJob(t, database, 101, "sibling", "/tmp/project", StatusQueued, withLaunch(launchID))
	consumer, err := GetJobByID(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Failure after the consumer and launch writes must roll back both, not
	// leave a failed launch with an unfailed consumer selected for retries.
	if _, err := database.Exec(`CREATE TRIGGER reject_sibling_reset BEFORE UPDATE OF requested_status ON jobs
		WHEN NEW.id = 101 BEGIN SELECT RAISE(ABORT, 'injected sibling reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := CancelLaunchForCloudDependency(database, launchID, consumer, "required artifact missing"); err == nil {
		t.Fatal("expected transaction rollback")
	}
	launch, err := GetLaunch(database, launchID)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = GetJobByID(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Status != LaunchStatusRunning || consumer.Status != StatusQueued || consumer.FailureReason != "" {
		t.Fatalf("partial dependency transition: launch=%s consumer=%s reason=%s", launch.Status, consumer.Status, consumer.FailureReason)
	}
}

func TestCancelLaunchForCloudDependencyPreservesMoveSource(t *testing.T) {
	database := SetupTestDB(t)
	sourceID := mustCreateLaunch(t, database)
	targetID, err := CreateLaunch(database, &Launch{Status: LaunchStatusLaunching})
	if err != nil {
		t.Fatal(err)
	}
	insertTestJob(t, database, 100, "consume", "/tmp/project", StatusQueued, withLaunch(sourceID))
	source, err := GetJobByID(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{JobID: 100, TargetKind: MoveTargetNew, TargetLaunchID: &targetID})
	if err != nil {
		t.Fatal(err)
	}
	targetAttempt, err := CreateMoveTargetAttempt(database, intent.ID, 100, "", &targetID, StatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	observed := *source
	observed.LatestRunID = &targetAttempt
	observed.LaunchID = &targetID
	if err := CancelLaunchForCloudDependency(database, targetID, &observed, "dependency missing at destination"); err != nil {
		t.Fatal(err)
	}
	got, err := GetJobByID(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.LaunchID == nil || *got.LaunchID != sourceID || got.LatestRunID == nil || *got.LatestRunID != *source.LatestRunID {
		t.Fatalf("source authority changed: %+v", got)
	}
	gotIntent, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotIntent.State != MoveIntentStateCanceled {
		t.Fatalf("move left retryable: %s", gotIntent.State)
	}
	var abandoned string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttempt).Scan(&abandoned); err != nil {
		t.Fatal(err)
	}
	if abandoned != AttemptAbandonedMoveDestinationRejected {
		t.Fatal(fmt.Sprintf("target abandonment = %q, want destination rejection", abandoned))
	}
}
