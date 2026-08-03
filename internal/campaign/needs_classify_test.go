package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func TestClassifyNeedsForLaunch_OnPremProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	producerID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo p", "on-prem producer")
	if err != nil {
		t.Fatalf("RecordQueued producer: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ []string) ([]cloud.CloudNeed, error) {
		t.Fatal("resolver should not be called for on-prem producers")
		return nil, nil
	}

	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, 0)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if len(cloudNeeds) != 0 || len(cloudAfter) != 0 {
		t.Fatalf("expected no refs, got cloudNeeds=%v cloudAfter=%v", cloudNeeds, cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_RentalSameLiveInstance(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo p", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ []string) ([]cloud.CloudNeed, error) {
		t.Fatal("resolver should not be called when producer is co-located")
		return nil, nil
	}

	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, instanceID)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if len(cloudNeeds) != 0 {
		t.Fatalf("expected no cloudNeeds, got %v", cloudNeeds)
	}
	if len(cloudAfter) != 1 || cloudAfter[0].JobID != producerID {
		t.Fatalf("expected 1 cloudAfter pointing at %d, got %v", producerID, cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_RentalOtherLiveInstance_Fallback(t *testing.T) {
	database := db.SetupTestDB(t)
	producerInstanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch producer: %v", err)
	}
	targetInstanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo p", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, producerInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	called := false
	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
		called = true
		if len(specs) != 1 {
			t.Fatalf("expected 1 spec, got %v", specs)
		}
		return []cloud.CloudNeed{{Spec: specs[0], Path: "out/x.pkl", R2Key: "mock"}}, nil
	}

	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, targetInstanceID)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if !called {
		t.Fatal("expected resolver to be called for cross-instance producer")
	}
	if len(cloudNeeds) != 1 {
		t.Fatalf("expected 1 cloudNeed, got %v", cloudNeeds)
	}
	if len(cloudAfter) != 0 {
		t.Fatalf("expected no cloudAfter for cross-instance producer, got %v", cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_MoveTargetAttemptHiddenBeforeAcceptance(t *testing.T) {
	database := db.SetupTestDB(t)
	sourceLaunchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	targetLaunchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo p", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU producer: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, sourceLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID producer source: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:      producerID,
		TargetKind: db.MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, producerID, "", &targetLaunchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	called := false
	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
		called = true
		return []cloud.CloudNeed{{Spec: specs[0], Path: "out/x.pkl", R2Key: "mock"}}, nil
	}

	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, targetLaunchID)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if !called {
		t.Fatal("expected resolver to be called because open move target is hidden")
	}
	if len(cloudNeeds) != 1 {
		t.Fatalf("cloudNeeds = %v, want one fallback need", cloudNeeds)
	}
	if len(cloudAfter) != 0 {
		t.Fatalf("cloudAfter = %v, want none while target attempt is hidden", cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_DeadProducerInstance_Fallback(t *testing.T) {
	database := db.SetupTestDB(t)
	deadInstance, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusCompleted, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch dead: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo p", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, deadInstance); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, specs []string) ([]cloud.CloudNeed, error) {
		return []cloud.CloudNeed{{Spec: specs[0], Path: "out/x.pkl", R2Key: "mock"}}, nil
	}

	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, deadInstance)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if len(cloudNeeds) != 1 {
		t.Fatalf("expected 1 cloudNeed for dead-producer fallback, got %v", cloudNeeds)
	}
	if len(cloudAfter) != 0 {
		t.Fatalf("expected no cloudAfter, got %v", cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_RentalSameBatch_Launching(t *testing.T) {
	// Mid-batch co-location: both producer and consumer are being placed on
	// the same instance in one instance launch. The instance hasn't yet
	// reached "running" status but is pinned, so co-location must be
	// recognized (shared LaunchID = shared filesystem).
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo p", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	consumer := &db.Job{ID: 999, Needs: []string{fmt.Sprintf("out/x.pkl:%d", producerID)}}

	prev := resolveCloudNeedsFunc
	t.Cleanup(func() { resolveCloudNeedsFunc = prev })
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ []string) ([]cloud.CloudNeed, error) {
		t.Fatal("resolver should not be called when producer is co-located in same launching batch")
		return nil, nil
	}

	_, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, consumer, instanceID)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if len(cloudAfter) != 1 || cloudAfter[0].JobID != producerID {
		t.Fatalf("expected 1 cloudAfter for same-batch producer, got %v", cloudAfter)
	}
}

func TestClassifyNeedsForLaunch_EmptyNeeds(t *testing.T) {
	database := db.SetupTestDB(t)
	cloudNeeds, cloudAfter, _, err := ClassifyNeedsForLaunch(context.Background(), database, nil, &db.Job{ID: 1}, 0)
	if err != nil {
		t.Fatalf("ClassifyNeedsForLaunch: %v", err)
	}
	if cloudNeeds != nil || cloudAfter != nil {
		t.Fatalf("expected nils for empty needs, got cloudNeeds=%v cloudAfter=%v", cloudNeeds, cloudAfter)
	}
}
