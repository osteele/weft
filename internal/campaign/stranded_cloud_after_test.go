package campaign

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
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

func TestReconcileStaleCloudAfterPins_SameLaunchReattempt(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, producerID, consumerID, pinnedRunID := recordPinnedCloudAfterPair(t, database)

	firstRunID, err := db.GetLatestAttemptID(database, producerID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if firstRunID != pinnedRunID {
		t.Fatalf("first run = %d, want pinned %d", firstRunID, pinnedRunID)
	}
	secondRunID, err := db.CreateAttempt(database, producerID, "", &launchID, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET predecessor_attempt_id = ?, cloud_outcome = ? WHERE id = ?`,
		firstRunID, db.AttemptOutcomeOrphaned, secondRunID,
	); err != nil {
		t.Fatalf("mark second predecessor/orphaned: %v", err)
	}
	thirdRunID, err := db.CreateAttempt(database, producerID, "", &launchID, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt third: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET predecessor_attempt_id = ? WHERE id = ?`, secondRunID, thirdRunID); err != nil {
		t.Fatalf("link third predecessor: %v", err)
	}

	before := mustJob(t, database, consumerID)
	oldConsumerRun := *before.LatestRunID
	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins first: %v", err)
	}
	if len(reset.JobIDs) != 1 || reset.JobIDs[0] != consumerID {
		t.Fatalf("first reset job IDs = %v, want [%d]", reset.JobIDs, consumerID)
	}
	if len(reset.AttemptIDs) != 1 || reset.AttemptIDs[0] != oldConsumerRun {
		t.Fatalf("first reset attempt IDs = %v, want [%d]", reset.AttemptIDs, oldConsumerRun)
	}
	consumer := mustJob(t, database, consumerID)
	if consumer.LaunchID != nil {
		t.Fatalf("consumer launch after reset = %v, want nil", *consumer.LaunchID)
	}

	reset, err = ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins second: %v", err)
	}
	if len(reset.JobIDs) != 0 || len(reset.AttemptIDs) != 0 {
		t.Fatalf("second reset = %+v, want no-op", reset)
	}
}

func TestReconcileStaleCloudAfterPins_CompletedProducerThenRequeued(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, producerID, consumerID, pinnedRunID := recordPinnedCloudAfterPair(t, database)

	if _, err := db.RecordCloudJobCompletion(database, producerID, 0, time.Now().Add(-2*time.Minute).Unix(), time.Now().Add(-time.Minute).Unix(), "", "", time.Time{}, pinnedRunID); err != nil {
		t.Fatalf("RecordCloudJobCompletion producer: %v", err)
	}
	requeuedRunID, err := db.CreateAttempt(database, producerID, "", &launchID, db.StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt requeue: %v", err)
	}
	if requeuedRunID == pinnedRunID {
		t.Fatalf("requeued run ID = pinned run ID %d", pinnedRunID)
	}

	before := mustJob(t, database, consumerID)
	oldConsumerRun := *before.LatestRunID
	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins: %v", err)
	}
	if len(reset.JobIDs) != 1 || reset.JobIDs[0] != consumerID {
		t.Fatalf("reset job IDs = %v, want [%d]", reset.JobIDs, consumerID)
	}
	if len(reset.AttemptIDs) != 1 || reset.AttemptIDs[0] != oldConsumerRun {
		t.Fatalf("reset attempt IDs = %v, want [%d]", reset.AttemptIDs, oldConsumerRun)
	}
	consumer := mustJob(t, database, consumerID)
	if consumer.LaunchID != nil {
		t.Fatalf("consumer launch after reset = %v, want nil", *consumer.LaunchID)
	}
	wantReasonPart := fmt.Sprintf("producer wj%d run changed from %d to %d", producerID, pinnedRunID, requeuedRunID)
	if got := strings.Join(consumer.PlacementReasons, "\n"); !strings.Contains(got, wantReasonPart) {
		t.Fatalf("placement reasons = %q, want %q", got, wantReasonPart)
	}
}

func TestReconcileStaleCloudAfterPins_ProducerMovedLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	_, producerID, consumerID, _ := recordPinnedCloudAfterPair(t, database)
	targetLaunchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	if err := db.TransferJobLaunchID(database, producerID, targetLaunchID); err != nil {
		t.Fatalf("TransferJobLaunchID producer: %v", err)
	}

	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins: %v", err)
	}
	if len(reset.JobIDs) != 1 || reset.JobIDs[0] != consumerID {
		t.Fatalf("reset job IDs = %v, want [%d]", reset.JobIDs, consumerID)
	}
}

func TestReconcileStaleCloudAfterPins_CurrentPinNoReset(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, _, consumerID, _ := recordPinnedCloudAfterPair(t, database)

	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins: %v", err)
	}
	if len(reset.JobIDs) != 0 {
		t.Fatalf("reset job IDs = %v, want none", reset.JobIDs)
	}
	assertJobStillOnLaunch(t, database, consumerID, launchID)
}

func TestReconcileStaleCloudAfterPins_ProducerUnreadableNoReset(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	consumerID := recordCloudQueuedJob(t, database, launchID, "echo consumer")
	consumer := mustJob(t, database, consumerID)
	if err := PersistResolvedCloudAfterPins(database, consumer, []cloud.CloudAfterRef{{JobID: 999999, RunID: 42}}); err != nil {
		t.Fatalf("PersistResolvedCloudAfterPins: %v", err)
	}

	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins: %v", err)
	}
	if len(reset.JobIDs) != 0 {
		t.Fatalf("reset job IDs = %v, want none", reset.JobIDs)
	}
	assertJobStillOnLaunch(t, database, consumerID, launchID)
}

func TestReconcileStaleCloudAfterPins_PinAbsentNoReset(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, producerID, consumerID, _ := recordPinnedCloudAfterPair(t, database)
	consumer := mustJob(t, database, consumerID)
	if err := PersistResolvedCloudAfterPins(database, consumer, nil); err != nil {
		t.Fatalf("clear resolved pins: %v", err)
	}
	if _, err := db.CreateAttempt(database, producerID, "", &launchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateAttempt producer retry: %v", err)
	}

	reset, err := ReconcileStaleCloudAfterPins(database)
	if err != nil {
		t.Fatalf("ReconcileStaleCloudAfterPins: %v", err)
	}
	if len(reset.JobIDs) != 0 {
		t.Fatalf("reset job IDs = %v, want none", reset.JobIDs)
	}
	assertJobStillOnLaunch(t, database, consumerID, launchID)
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

func recordPinnedCloudAfterPair(t *testing.T, database *sql.DB) (int64, int64, int64, int64) {
	t.Helper()
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	producerID := recordCloudQueuedJob(t, database, launchID, "echo producer")
	consumerID := recordCloudQueuedJob(t, database, launchID, "echo consumer")
	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/model.pt:%d", producerID)}); err != nil {
		t.Fatalf("SetJobNeeds consumer: %v", err)
	}
	producer := mustJob(t, database, producerID)
	if producer.LatestRunID == nil || *producer.LatestRunID <= 0 {
		t.Fatalf("producer latest run = %v, want non-zero", producer.LatestRunID)
	}
	consumer := mustJob(t, database, consumerID)
	if err := PersistResolvedCloudAfterPins(database, consumer, []cloud.CloudAfterRef{{
		JobID: producerID,
		RunID: *producer.LatestRunID,
	}}); err != nil {
		t.Fatalf("PersistResolvedCloudAfterPins: %v", err)
	}
	return launchID, producerID, consumerID, *producer.LatestRunID
}

func mustJob(t *testing.T, database *sql.DB, jobID int64) *db.Job {
	t.Helper()
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID %d: %v", jobID, err)
	}
	if job == nil {
		t.Fatalf("job %d not found", jobID)
	}
	return job
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
