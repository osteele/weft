package db

import "testing"

func TestUpsertExternalJobFromObservationIsIdempotent(t *testing.T) {
	database := SetupTestDB(t)

	obs := ExternalJobObservation{
		Executor:                ExternalExecutorSkyPilot,
		ExternalJobID:           "42",
		ExternalTaskID:          "task-a",
		ExternalClusterName:     "sky-cluster",
		RawStatus:               "RUNNING",
		RawStatusMessage:        "healthy",
		NormalizedStatus:        StatusRunning,
		SubmittedFromWorkingDir: "/tmp/project",
		Command:                 "python train.py",
		Description:             "train",
		Tags:                    []string{"processed", "compute-intensive"},
	}
	binding, created, err := UpsertExternalJobFromObservation(database, obs)
	if err != nil {
		t.Fatalf("UpsertExternalJobFromObservation: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Backend != BackendSkyPilot {
		t.Fatalf("Backend = %q, want %q", job.Backend, BackendSkyPilot)
	}
	if got := job.TargetKind(); got != JobTargetExternal {
		t.Fatalf("TargetKind = %q, want %q", got, JobTargetExternal)
	}
	if job.EffectiveStatus() != StatusRunning {
		t.Fatalf("status = %q, want %q", job.EffectiveStatus(), StatusRunning)
	}

	obs.RawStatus = "SUCCEEDED"
	obs.NormalizedStatus = StatusCompleted
	refreshed, created, err := UpsertExternalJobFromObservation(database, obs)
	if err != nil {
		t.Fatalf("second UpsertExternalJobFromObservation: %v", err)
	}
	if created {
		t.Fatal("second created = true, want false")
	}
	if refreshed.JobID != binding.JobID {
		t.Fatalf("second JobID = %d, want %d", refreshed.JobID, binding.JobID)
	}
	job, err = GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID after refresh: %v", err)
	}
	if job.EffectiveStatus() != StatusCompleted {
		t.Fatalf("status after refresh = %q, want %q", job.EffectiveStatus(), StatusCompleted)
	}
	if job.EndTime == nil {
		t.Fatal("completed external job should have end_time")
	}
}

func TestMarkExternalSyncWarningLeavesStatusUnchanged(t *testing.T) {
	database := SetupTestDB(t)

	binding, _, err := UpsertExternalJobFromObservation(database, ExternalJobObservation{
		Executor:                ExternalExecutorSkyPilot,
		ExternalJobID:           "42",
		RawStatus:               "RUNNING",
		RawStatusMessage:        "healthy",
		NormalizedStatus:        StatusRunning,
		SubmittedFromWorkingDir: "/tmp/project",
		Command:                 "python train.py",
	})
	if err != nil {
		t.Fatalf("UpsertExternalJobFromObservation: %v", err)
	}
	if err := MarkExternalSyncWarning(database, binding.JobID, "sky unavailable"); err != nil {
		t.Fatalf("MarkExternalSyncWarning: %v", err)
	}

	job, err := GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got := job.EffectiveStatus(); got != StatusRunning {
		t.Fatalf("status after warning = %q, want %q", got, StatusRunning)
	}
	refreshed, err := GetExternalJobBindingByJobID(database, binding.JobID)
	if err != nil {
		t.Fatalf("GetExternalJobBindingByJobID: %v", err)
	}
	if refreshed.RawStatus != "RUNNING" {
		t.Fatalf("raw status after warning = %q, want RUNNING", refreshed.RawStatus)
	}
	if refreshed.RawStatusMessage != "sky unavailable" {
		t.Fatalf("raw status message = %q, want sync warning", refreshed.RawStatusMessage)
	}
}
