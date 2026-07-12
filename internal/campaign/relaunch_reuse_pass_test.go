package campaign

import (
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
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
	got := tryPlaceOntoExistingInstances(cfg, []*db.Job{job})
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
	cfg := RelaunchConfig{
		Database: database,
		R2Cfg: cloud.R2Config{
			AccountID:       "stub",
			AccessKeyID:     "stub",
			SecretAccessKey: "stub",
			Bucket:          "stub",
		},
	}
	got := tryPlaceOntoExistingInstances(cfg, []*db.Job{job})
	if len(got) != 1 || got[0].ID != jobID {
		t.Fatalf("expected job to fall through unchanged, got %v", got)
	}
}
