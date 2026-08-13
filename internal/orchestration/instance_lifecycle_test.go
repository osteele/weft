package orchestration

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
)

// User-initiated terminate must stamp termination_requested_at
// (UserTerminatesInstance in specs/campaign-lifecycle.allium) alongside the
// canceled status.
func TestTerminateInstancesParallel_StampsTerminationRequestedAt(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-stamp"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	clientForProvider := func(provider string) (cloud.Client, error) {
		return &cloud.MockClient{ProviderVal: cloud.Provider(provider)}, nil
	}

	terminated, errs := terminateInstancesParallel(database, []int64{instanceID}, clientForProvider)
	for _, e := range errs {
		t.Errorf("terminate error: %v", e)
	}
	if terminated != 1 {
		t.Fatalf("terminated = %d, want 1", terminated)
	}

	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusCancelled {
		t.Errorf("status = %q, want %q", launch.Status, db.LaunchStatusCancelled)
	}
	if launch.TerminationReason != db.TerminationReasonCancelled {
		t.Errorf("termination_reason = %q, want %q", launch.TerminationReason, db.TerminationReasonCancelled)
	}
	if launch.TerminationRequestedAt == nil {
		t.Error("termination_requested_at = nil, want stamped")
	}
}

func TestTerminateInstancesParallel_MissingProviderIDFailsClosed(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	terminated, errs := terminateInstancesParallel(database, []int64{instanceID}, func(string) (cloud.Client, error) {
		t.Fatal("provider client must not be created without a provider instance ID")
		return nil, nil
	})
	if terminated != 0 || len(errs) != 1 {
		t.Fatalf("termination result = (%d, %v), want one fail-closed error", terminated, errs)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning || launch.TerminationRequestedAt != nil {
		t.Fatalf("launch after missing provider ID = status %q, requested %v; want unchanged", launch.Status, launch.TerminationRequestedAt)
	}
}

func TestTerminateInstancesParallel_DestroyFailurePreservesLaunchAndJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	destroyErr := errors.New("provider API unavailable")
	clientForProvider := func(provider string) (cloud.Client, error) {
		return &cloud.MockClient{
			ProviderVal: cloud.Provider(provider),
			DestroyInstanceFunc: func(string) error {
				return destroyErr
			},
		}, nil
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: string(cloud.ProviderVastai),
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-123"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "echo test", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	terminated, errs := terminateInstancesParallel(database, []int64{instanceID}, clientForProvider)
	if terminated != 0 {
		t.Fatalf("terminated = %d, want 0", terminated)
	}
	if len(errs) != 1 || !errors.Is(errs[0], destroyErr) {
		t.Fatalf("errors = %v, want one provider destroy error", errs)
	}

	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q", launch.Status, db.LaunchStatusRunning)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("job launch = %v, want %d", job.LaunchID, instanceID)
	}
}

// Once the provider destroy succeeds, all corresponding local lifecycle
// writes form one state transition. A failure while requeuing the jobs must
// not leave the launch terminal while its job still points at that launch.
func TestTerminateInstancesParallel_LocalFailureRollsBackLifecycleTransition(t *testing.T) {
	database := db.SetupTestDB(t)

	var destroyCalls atomic.Int32
	clientForProvider := func(provider string) (cloud.Client, error) {
		return &cloud.MockClient{
			ProviderVal: cloud.Provider(provider),
			DestroyInstanceFunc: func(string) error {
				if destroyCalls.Add(1) == 1 {
					return nil
				}
				return cloud.ErrInstanceNotFound
			},
		}, nil
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: string(cloud.ProviderVastai),
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-atomicity"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "echo test", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	if _, err := database.Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_termination_job_reset
		BEFORE UPDATE ON jobs
		WHEN OLD.id = %d
		BEGIN
			SELECT RAISE(ABORT, 'injected job reset failure');
		END`, jobID)); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	terminated, errs := terminateInstancesParallel(database, []int64{instanceID}, clientForProvider)
	if terminated != 0 {
		t.Fatalf("terminated = %d, want 0", terminated)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one injected local failure", errs)
	}
	if destroyCalls.Load() != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyCalls.Load())
	}

	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Errorf("launch status = %q, want rollback to %q", launch.Status, db.LaunchStatusRunning)
	}
	if launch.TerminationIntent == nil || launch.TerminationIntent.EffectiveState() != instanceintent.StateDestroying {
		t.Errorf("termination intent = %+v, want durable destroying intent for crash recovery", launch.TerminationIntent)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Errorf("job launch = %v, want rollback to %d", job.LaunchID, instanceID)
	}

	if _, err := database.Exec(`DROP TRIGGER fail_termination_job_reset`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	terminated, errs = terminateInstancesParallel(database, []int64{instanceID}, clientForProvider)
	if len(errs) != 0 || terminated != 1 {
		t.Fatalf("retry after confirmed provider absence = (%d, %v), want (1, nil)", terminated, errs)
	}
	launch, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch after retry: %v", err)
	}
	if launch.Status != db.LaunchStatusCancelled {
		t.Errorf("launch status after retry = %q, want %q", launch.Status, db.LaunchStatusCancelled)
	}
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after retry: %v", err)
	}
	if job.LaunchID != nil {
		t.Errorf("job launch after retry = %v, want detached", job.LaunchID)
	}
}

func TestTerminateInstancesParallel_RecoversDestroyIntentAfterCrash(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: string(cloud.ProviderVastai),
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-crash-recovery"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "echo test", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := db.RecordLaunchDestroyIntent(database, instanceID, db.LaunchStatusCancelled, db.TerminationReasonCancelled); err != nil {
		t.Fatalf("seed pre-crash destroy intent: %v", err)
	}

	terminated, errs := terminateInstancesParallel(database, []int64{instanceID}, func(provider string) (cloud.Client, error) {
		return &cloud.MockClient{
			ProviderVal: cloud.Provider(provider),
			DestroyInstanceFunc: func(string) error {
				return cloud.ErrInstanceNotFound
			},
		}, nil
	})
	if terminated != 1 || len(errs) != 0 {
		t.Fatalf("recovery result = (%d, %v), want one finalized termination", terminated, errs)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusCancelled || launch.TerminationIntent == nil || launch.TerminationIntent.EffectiveState() != instanceintent.StateSucceeded {
		t.Fatalf("recovered launch = status %q, intent %+v; want cancelled/succeeded", launch.Status, launch.TerminationIntent)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID != nil {
		t.Fatalf("job launch = %v, want detached after recovered destroy", job.LaunchID)
	}
}

func TestTerminateInstancesParallel_UnknownProviderFailsClosed(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "future-cloud",
		GPUSpec:  "GPU",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-123"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}

	terminated, errs := TerminateInstancesParallel(database, []int64{instanceID})
	if terminated != 0 {
		t.Fatalf("terminated = %d, want 0", terminated)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one unsupported-provider error", errs)
	}

	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q", launch.Status, db.LaunchStatusRunning)
	}
}
