package campaign

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestResetStrandedCloudAfterConsumers(t *testing.T) {
	database := db.SetupTestDB(t)
	sourceLaunchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	producerID := recordCloudQueuedJob(t, database, sourceLaunchID, "echo producer")
	consumerID := recordCloudQueuedJob(t, database, sourceLaunchID, "echo consumer")
	otherProducerID := recordCloudQueuedJob(t, database, sourceLaunchID, "echo other producer")
	otherConsumerID := recordCloudQueuedJob(t, database, sourceLaunchID, "echo other consumer")
	startedConsumerID := recordCloudQueuedJob(t, database, sourceLaunchID, "echo started")

	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/model.pt:%d", producerID)}); err != nil {
		t.Fatalf("SetJobNeeds consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, otherConsumerID, []string{fmt.Sprintf("output/model.pt:%d", otherProducerID)}); err != nil {
		t.Fatalf("SetJobNeeds unrelated consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, startedConsumerID, []string{fmt.Sprintf("output/model.pt:%d", producerID)}); err != nil {
		t.Fatalf("SetJobNeeds started consumer: %v", err)
	}
	if err := db.UpdateStartTime(database, startedConsumerID, time.Now().Unix()); err != nil {
		t.Fatalf("UpdateStartTime started consumer: %v", err)
	}

	before, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("GetJobByID consumer before: %v", err)
	}
	if before.LatestRunID == nil {
		t.Fatal("consumer latest run is nil before reset")
	}
	oldAttemptID := *before.LatestRunID

	reset, err := ResetStrandedCloudAfterConsumers(database, producerID, sourceLaunchID)
	if err != nil {
		t.Fatalf("ResetStrandedCloudAfterConsumers: %v", err)
	}
	if len(reset.JobIDs) != 1 || reset.JobIDs[0] != consumerID {
		t.Fatalf("reset job IDs = %v, want [%d]", reset.JobIDs, consumerID)
	}
	if len(reset.AttemptIDs) != 1 || reset.AttemptIDs[0] != oldAttemptID {
		t.Fatalf("reset attempt IDs = %v, want [%d]", reset.AttemptIDs, oldAttemptID)
	}

	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("GetJobByID consumer after: %v", err)
	}
	if consumer.LaunchID != nil {
		t.Fatalf("consumer launch ID = %v, want nil", *consumer.LaunchID)
	}
	if consumer.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("consumer status = %q, want queued", consumer.EffectiveStatus())
	}
	if consumer.LatestRunID == nil || *consumer.LatestRunID == oldAttemptID {
		t.Fatalf("consumer latest run = %v, want fresh unplaced attempt", consumer.LatestRunID)
	}
	wantReason := fmt.Sprintf("producer wj%d moved off instance wi%d; job returned to unplaced queue", producerID, sourceLaunchID)
	if got := strings.Join(consumer.PlacementReasons, "\n"); got != wantReason {
		t.Fatalf("placement reasons = %q, want %q", got, wantReason)
	}

	assertJobStillOnLaunch(t, database, otherConsumerID, sourceLaunchID)
	assertJobStillOnLaunch(t, database, startedConsumerID, sourceLaunchID)
}

func recordCloudQueuedJob(t *testing.T, database *sql.DB, launchID int64, command string) int64 {
	t.Helper()
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", command, command, "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	return jobID
}

func assertJobStillOnLaunch(t *testing.T, database *sql.DB, jobID, launchID int64) {
	t.Helper()
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID %d: %v", jobID, err)
	}
	if job.LaunchID == nil || *job.LaunchID != launchID {
		t.Fatalf("job %d launch ID = %v, want %d", jobID, job.LaunchID, launchID)
	}
}
