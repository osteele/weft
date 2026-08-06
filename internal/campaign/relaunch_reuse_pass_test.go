package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
)

func TestPreferredInstanceIDsFromNeeds_CollectsLiveProducerInstances(t *testing.T) {
	database := db.SetupTestDB(t)

	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo producer", "p", "")
	if err != nil {
		t.Fatalf("create producer: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090"})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	needs := []string{fmt.Sprintf("output/x.pt:%d", producerID)}
	got := PreferredInstanceIDsFromNeeds(database, needs)
	if len(got) != 1 || got[0] != launchID {
		t.Fatalf("got %v, want [%d]", got, launchID)
	}
}

func TestPreferredInstanceIDsFromNeeds_SkipsUnplacedProducers(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo producer", "p", "")
	if err != nil {
		t.Fatalf("create producer: %v", err)
	}
	needs := []string{fmt.Sprintf("output/x.pt:%d", producerID)}
	got := PreferredInstanceIDsFromNeeds(database, needs)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (producer not yet on a launch)", got)
	}
}

func TestPreferredInstanceIDsFromNeeds_DeduplicatesAndIgnoresMalformed(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo producer", "p", "")
	if err != nil {
		t.Fatalf("create producer: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090"})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	needs := []string{
		fmt.Sprintf("output/a.pt:%d", producerID),
		fmt.Sprintf("output/b.pt:%d", producerID), // same producer, different file
		"malformed_no_version",                    // ignored
		"output/missing.pt:99999999",              // unknown producer, ignored
	}
	got := PreferredInstanceIDsFromNeeds(database, needs)
	if len(got) != 1 || got[0] != launchID {
		t.Fatalf("got %v, want [%d]", got, launchID)
	}
}

func TestReuseSourceCollect_LongQueueRemainsCandidateAndPreferenceLowersWait(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "NVIDIA",
		ResolvedGPUName: "RTX A6000",
		GPUMemGB:        48,
		NumGPUs:         1,
		DiskGB:          120,
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	for i := 0; i < 7; i++ {
		jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo queued", fmt.Sprintf("queued %d", i), "nvidia")
		if err != nil {
			t.Fatalf("create queued job %d: %v", i, err)
		}
		if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID %d: %v", jobID, err)
		}
	}

	source := &ReuseSource{}
	withoutPreference, err := source.Collect(database, placement.Constraints{GPUClass: "nvidia"}, nil)
	if err != nil {
		t.Fatalf("Collect without preference: %v", err)
	}
	if len(withoutPreference) != 1 || withoutPreference[0].Reuse == nil || withoutPreference[0].Reuse.InstanceID != launchID {
		t.Fatalf("without preference candidates = %+v, want long-queue instance %d", withoutPreference, launchID)
	}

	withPreference, err := source.Collect(database, placement.Constraints{
		GPUClass:             "nvidia",
		PreferredInstanceIDs: []int64{launchID},
	}, nil)
	if err != nil {
		t.Fatalf("Collect with preference: %v", err)
	}
	if len(withPreference) != 1 || withPreference[0].Reuse == nil || withPreference[0].Reuse.InstanceID != launchID {
		t.Fatalf("with preference candidates = %+v, want preferred instance %d", withPreference, launchID)
	}
	if !(withPreference[0].Reuse.EstWait.Mean < withoutPreference[0].Reuse.EstWait.Mean) {
		t.Fatalf("preferred wait = %s, want less than non-preferred wait %s",
			withPreference[0].Reuse.EstWait.Mean, withoutPreference[0].Reuse.EstWait.Mean)
	}
}

func TestTryPlaceOntoExistingInstances_NoopWhenR2Unconfigured(t *testing.T) {
	// Without R2 credentials we cannot submit jobs, so the reuse pass is a
	// no-op and must return the original list intact rather than dropping
	// jobs on the floor.
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "j", "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	cfg := RelaunchConfig{
		Database: database,
		// R2Cfg intentionally empty
	}
	got := tryPlaceOntoExistingInstances(cfg, []*db.Job{job}, DefaultMaxCloudAttempts)
	if len(got) != 1 || got[0].ID != jobID {
		t.Fatalf("expected the original job to be returned, got %v", got)
	}
}

func TestTryPlaceOntoExistingInstances_PassesThroughWhenNoReuseCandidate(t *testing.T) {
	// With R2 configured but no compatible existing instance, the job must
	// fall through to the new-launch path (returned in the remaining slice).
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "j", "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	job, _ := db.GetJobByID(database, jobID)

	// Stub R2 config so we exercise the post-credential branch. The
	// placement evaluator returns Unplaced when no instances exist.
	got := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{job}, DefaultMaxCloudAttempts)
	if len(got) != 1 || got[0].ID != jobID {
		t.Fatalf("expected job to fall through unchanged, got %v", got)
	}
}

func TestTryPlaceOntoExistingInstances_CoLocatesConsumerWithRunningProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "nvidia",
		ResolvedGPUName: "RTX 4090",
		GPUMemGB:        24,
		NumGPUs:         1,
		DiskGB:          120,
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo producer", "producer", "nvidia")
	if err != nil {
		t.Fatalf("create producer: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID producer: %v", err)
	}
	if err := db.UpdateQueuedToRunning(database, producerID); err != nil {
		t.Fatalf("UpdateQueuedToRunning producer: %v", err)
	}
	consumerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo consumer", "consumer", "nvidia")
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	needsSpec := fmt.Sprintf("output/model.pt:%d", producerID)
	if err := db.SetJobNeeds(database, consumerID, []string{needsSpec}); err != nil {
		t.Fatalf("SetJobNeeds consumer: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("GetJobByID consumer: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
	})
	uploadSourceToR2 = func(_ context.Context, _ *r2.Client, sourceDir string, _ []string) (weftsync.SourceUploadResult, error) {
		return testSourceUploadResult(sourceDir, "sources/test.tar.gz"), nil
	}
	var gotPayload controlplane.GraceJobsRequest
	sendGraceJobPayloadNoAck = func(_ context.Context, _ controlplane.GraceStore, _ int64, payload controlplane.GraceJobsRequest) (string, error) {
		gotPayload = payload
		return "req-test", nil
	}

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{consumer}, DefaultMaxCloudAttempts)
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want consumer placed on producer instance", remaining)
	}
	if len(gotPayload.Jobs) != 1 || gotPayload.Jobs[0].ID != consumerID {
		t.Fatalf("payload jobs = %#v, want consumer %d", gotPayload.Jobs, consumerID)
	}
	if len(gotPayload.Jobs[0].CloudAfter) != 1 || gotPayload.Jobs[0].CloudAfter[0].JobID != producerID {
		t.Fatalf("cloud_after = %#v, want producer %d", gotPayload.Jobs[0].CloudAfter, producerID)
	}
}

func dependencyReusePassFixture(t *testing.T, database *sql.DB, depSpecSuffix string, finishProducer func(int64)) (*db.Job, *int) {
	t.Helper()

	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "nvidia",
		ResolvedGPUName: "RTX 4090",
		GPUMemGB:        24,
		NumGPUs:         1,
		DiskGB:          120,
	}); err != nil {
		t.Fatalf("create reusable launch: %v", err)
	}

	producerID, err := db.RecordJobStarting(database, "cool30", "/tmp", "echo producer", "producer")
	if err != nil {
		t.Fatalf("RecordJobStarting producer: %v", err)
	}
	if finishProducer != nil {
		finishProducer(producerID)
	}

	consumerID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo consumer", "consumer", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU consumer: %v", err)
	}
	if err := db.SetJobDepSpec(database, consumerID, strconv.FormatInt(producerID, 10)+depSpecSuffix); err != nil {
		t.Fatalf("SetJobDepSpec consumer: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("GetJobByID consumer: %v", err)
	}

	prevSubmit := submitJobsToInstanceForReusePass
	t.Cleanup(func() {
		submitJobsToInstanceForReusePass = prevSubmit
	})
	submitted := 0
	submitJobsToInstanceForReusePass = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ int64, jobs []*db.Job) error {
		submitted += len(jobs)
		return nil
	}

	return consumer, &submitted
}

func TestTryPlaceOntoExistingInstances_AfterDependencyRunningProducerSkipsReuse(t *testing.T) {
	database := db.SetupTestDB(t)
	consumer, submitted := dependencyReusePassFixture(t, database, "", nil)

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{consumer}, DefaultMaxCloudAttempts)
	if *submitted != 0 {
		t.Fatalf("submitted = %d, want 0 while dependency is running", *submitted)
	}
	if len(remaining) != 1 || remaining[0].ID != consumer.ID {
		t.Fatalf("remaining = %v, want consumer passed through while dependency is running", remaining)
	}
	reloaded, err := db.GetJobByID(database, consumer.ID)
	if err != nil {
		t.Fatalf("GetJobByID reload: %v", err)
	}
	if reloaded.LaunchID != nil {
		t.Fatalf("LaunchID = %v, want nil while dependency is running", *reloaded.LaunchID)
	}
}

func TestTryPlaceOntoExistingInstances_AfterDependencySuccessfulProducerPlaces(t *testing.T) {
	database := db.SetupTestDB(t)
	consumer, submitted := dependencyReusePassFixture(t, database, "", func(producerID int64) {
		exitCode := 0
		if err := db.CloseAttempt(database, producerID, db.StatusCompleted, &exitCode, time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt producer: %v", err)
		}
	})

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{consumer}, DefaultMaxCloudAttempts)
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want consumer placed after successful dependency", remaining)
	}
	if *submitted != 1 {
		t.Fatalf("submitted = %d, want 1 after successful dependency", *submitted)
	}
}

func TestTryPlaceOntoExistingInstances_StrictAfterFailedProducerSkipsReuse(t *testing.T) {
	database := db.SetupTestDB(t)
	consumer, submitted := dependencyReusePassFixture(t, database, "", func(producerID int64) {
		exitCode := 1
		if err := db.CloseAttempt(database, producerID, db.StatusCompleted, &exitCode, time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt producer: %v", err)
		}
	})

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{consumer}, DefaultMaxCloudAttempts)
	if *submitted != 0 {
		t.Fatalf("submitted = %d, want 0 after non-zero strict dependency", *submitted)
	}
	if len(remaining) != 1 || remaining[0].ID != consumer.ID {
		t.Fatalf("remaining = %v, want strict consumer passed through after non-zero dependency", remaining)
	}
}

func TestTryPlaceOntoExistingInstances_AfterAnyFailedProducerPlaces(t *testing.T) {
	database := db.SetupTestDB(t)
	consumer, submitted := dependencyReusePassFixture(t, database, ":any", func(producerID int64) {
		exitCode := 1
		if err := db.CloseAttempt(database, producerID, db.StatusFailed, &exitCode, time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt producer: %v", err)
		}
	})

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{consumer}, DefaultMaxCloudAttempts)
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want --after-any consumer placed after terminal dependency", remaining)
	}
	if *submitted != 1 {
		t.Fatalf("submitted = %d, want 1 after terminal --after-any dependency", *submitted)
	}
}

// reusePassRetryFixture builds a running reusable instance, a compatible
// unplaced job with `attempts` prior started-and-orphaned cloud attempts
// (the last one ending at lastEnd), and the submit stubs that record a
// successful reuse placement. Returns the job and a pointer to the count
// of jobs submitted to the instance.
func reusePassRetryFixture(t *testing.T, database *sql.DB, attempts int, lastEnd time.Time) (*db.Job, *int) {
	t.Helper()

	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "nvidia",
		ResolvedGPUName: "RTX 4090",
		GPUMemGB:        24,
		NumGPUs:         1,
		DiskGB:          120,
	}); err != nil {
		t.Fatalf("create reusable launch: %v", err)
	}

	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create failed launch: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo retry", "retry", "nvidia")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	for i := 0; i < attempts; i++ {
		end := lastEnd.Unix()
		start := end - 60
		if _, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, start_time, end_time)
			 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?)`,
			jobID, i+1, failedID, db.StatusFailed, db.AttemptOutcomeOrphaned, start, start, end,
		); err != nil {
			t.Fatalf("insert attempt %d: %v", i+1, err)
		}
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	prevResolveResume := resolveResumeCloudNeedsFunc
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
		resolveResumeCloudNeedsFunc = prevResolveResume
	})
	uploadSourceToR2 = func(_ context.Context, _ *r2.Client, sourceDir string, _ []string) (weftsync.SourceUploadResult, error) {
		return testSourceUploadResult(sourceDir, "sources/test.tar.gz"), nil
	}
	resolveResumeCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ *db.Job) ([]cloud.CloudNeed, error) {
		return nil, nil
	}
	submitted := 0
	sendGraceJobPayloadNoAck = func(_ context.Context, _ controlplane.GraceStore, _ int64, payload controlplane.GraceJobsRequest) (string, error) {
		submitted += len(payload.Jobs)
		return "req-test", nil
	}
	return job, &submitted
}

func reusePassStubCfg(database *sql.DB) RelaunchConfig {
	return RelaunchConfig{
		Database: database,
		R2Cfg: cloud.R2Config{
			AccountID:       "stub",
			AccessKeyID:     "stub",
			SecretAccessKey: "stub",
			Bucket:          "stub",
		},
	}
}

// Control: with attempts below the cap and backoff elapsed, the fixture
// job IS reuse-placed. The two gating tests below differ only in attempt
// count / recency, so this pins that their skips come from the gate, not
// from a missing candidate.
func TestTryPlaceOntoExistingInstances_PastBackoffPlaces(t *testing.T) {
	database := db.SetupTestDB(t)
	job, submitted := reusePassRetryFixture(t, database, 1, time.Now().Add(-10*time.Minute))

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{job}, DefaultMaxCloudAttempts)
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want job placed onto the reusable instance", remaining)
	}
	if *submitted != 1 {
		t.Fatalf("submitted = %d, want 1", *submitted)
	}
}

// Regression: the reuse pass honors the per-job attempt cap. A job that
// instances keep bouncing must not re-submit every autopilot pass after
// the new-instance path has already given up on it (wj889-shape
// non-convergent loop: the submit succeeds, the attempt orphans, and the
// job re-enters the unplaced pool with nothing limiting the cycle).
func TestTryPlaceOntoExistingInstances_MaxAttemptsSkipsReuse(t *testing.T) {
	database := db.SetupTestDB(t)
	job, submitted := reusePassRetryFixture(t, database, DefaultMaxCloudAttempts, time.Now().Add(-10*time.Minute))

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{job}, DefaultMaxCloudAttempts)
	if len(remaining) != 1 || remaining[0].ID != job.ID {
		t.Fatalf("remaining = %v, want job passed through for the eligibility loop to record the skip", remaining)
	}
	if *submitted != 0 {
		t.Fatalf("submitted = %d, want 0 at the attempt cap", *submitted)
	}
}

// Regression: the reuse pass honors the retry backoff window. An attempt
// that just orphaned must not be re-submitted on the very next pass.
func TestTryPlaceOntoExistingInstances_BackoffSkipsReuse(t *testing.T) {
	database := db.SetupTestDB(t)
	job, submitted := reusePassRetryFixture(t, database, 1, time.Now())

	remaining := tryPlaceOntoExistingInstances(reusePassStubCfg(database), []*db.Job{job}, DefaultMaxCloudAttempts)
	if len(remaining) != 1 || remaining[0].ID != job.ID {
		t.Fatalf("remaining = %v, want job passed through while in backoff", remaining)
	}
	if *submitted != 0 {
		t.Fatalf("submitted = %d, want 0 during backoff", *submitted)
	}
}
