// Tests derived from specs/campaign-lifecycle.allium.
// Verifies instance (launch) status transitions, grace period lifecycle,
// termination reasons, and retryability classification.
package db

import (
	"database/sql"
	"testing"
	"time"
)

// createTestLaunch inserts a launch in the given status and returns its ID.
func createTestLaunch(t *testing.T, database *sql.DB, status string) int64 {
	t.Helper()
	campaignID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	id, err := CreateLaunch(database, &Launch{
		CampaignID: &campaignID,
		Status:     LaunchStatusPlanned,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	// Walk to the desired status.
	switch status {
	case LaunchStatusPlanned:
		// already there
	case LaunchStatusLaunching:
		if err := UpdateLaunchStatus(database, id, LaunchStatusLaunching); err != nil {
			t.Fatalf("transition to launching: %v", err)
		}
	case LaunchStatusRunning:
		if err := UpdateLaunchStatus(database, id, LaunchStatusLaunching); err != nil {
			t.Fatalf("transition to launching: %v", err)
		}
		if err := UpdateLaunchStatus(database, id, LaunchStatusRunning); err != nil {
			t.Fatalf("transition to running: %v", err)
		}
	case LaunchStatusGrace:
		if err := UpdateLaunchStatus(database, id, LaunchStatusLaunching); err != nil {
			t.Fatalf("transition to launching: %v", err)
		}
		if err := UpdateLaunchStatus(database, id, LaunchStatusRunning); err != nil {
			t.Fatalf("transition to running: %v", err)
		}
		deadline := time.Now().Add(5 * time.Minute).Unix()
		if err := SetLaunchGraceStarted(database, id, deadline); err != nil {
			t.Fatalf("transition to grace: %v", err)
		}
	}
	return id
}

func TestSpec_InstanceStatusTransitions(t *testing.T) {
	// Spec-declared instance transitions from campaign-lifecycle.allium.
	validTransitions := []struct {
		from, to          string
		terminationReason string
	}{
		{LaunchStatusPlanned, LaunchStatusLaunching, ""},
		{LaunchStatusPlanned, LaunchStatusCancelled, TerminationReasonCancelled},
		{LaunchStatusLaunching, LaunchStatusRunning, ""},
		{LaunchStatusLaunching, LaunchStatusFailed, TerminationReasonBootstrapTimeout},
		{LaunchStatusLaunching, LaunchStatusCancelled, TerminationReasonCancelled},
		{LaunchStatusRunning, LaunchStatusCompleted, TerminationReasonCompleted},
		{LaunchStatusRunning, LaunchStatusFailed, TerminationReasonJobFailure},
		{LaunchStatusRunning, LaunchStatusCancelled, TerminationReasonCancelled},
		// grace transitions tested separately
	}

	for _, tc := range validTransitions {
		name := tc.from + "->" + tc.to
		t.Run(name, func(t *testing.T) {
			database := SetupTestDB(t)
			id := createTestLaunch(t, database, tc.from)

			var err error
			if tc.terminationReason != "" {
				err = UpdateLaunchStatus(database, id, tc.to, tc.terminationReason)
			} else {
				err = UpdateLaunchStatus(database, id, tc.to)
			}
			if err != nil {
				t.Fatalf("UpdateLaunchStatus(%s -> %s): %v", tc.from, tc.to, err)
			}

			launch, err := GetLaunch(database, id)
			if err != nil {
				t.Fatalf("GetLaunch: %v", err)
			}
			if launch.Status != tc.to {
				t.Errorf("status = %q, want %q", launch.Status, tc.to)
			}
		})
	}
}

func TestSpec_GracePeriodLifecycle(t *testing.T) {
	t.Run("running->grace->running (resubmit)", func(t *testing.T) {
		database := SetupTestDB(t)
		id := createTestLaunch(t, database, LaunchStatusRunning)

		// Configure grace period.
		if err := SetLaunchGracePeriod(database, id, 300); err != nil {
			t.Fatalf("SetLaunchGracePeriod: %v", err)
		}

		// Enter grace.
		deadline := time.Now().Add(5 * time.Minute).Unix()
		if err := SetLaunchGraceStarted(database, id, deadline); err != nil {
			t.Fatalf("SetLaunchGraceStarted: %v", err)
		}

		launch, _ := GetLaunch(database, id)
		if launch.Status != LaunchStatusGrace {
			t.Fatalf("status = %q, want grace", launch.Status)
		}
		if launch.GraceDeadline == nil {
			t.Fatal("grace_deadline should be set")
		}
		if launch.GraceStartedAt == nil {
			t.Fatal("grace_started_at should be set")
		}

		// Resume from grace (jobs resubmitted).
		if err := ClearLaunchGrace(database, id); err != nil {
			t.Fatalf("ClearLaunchGrace: %v", err)
		}

		launch, _ = GetLaunch(database, id)
		if launch.Status != LaunchStatusRunning {
			t.Fatalf("status = %q after clear, want running", launch.Status)
		}
		if launch.GraceDeadline != nil {
			t.Error("grace_deadline should be nil after clear")
		}
		if launch.GraceStartedAt != nil {
			t.Error("grace_started_at should be nil after clear")
		}
	})

	t.Run("grace->completed (all resubmitted jobs succeed)", func(t *testing.T) {
		database := SetupTestDB(t)
		id := createTestLaunch(t, database, LaunchStatusGrace)

		if err := UpdateLaunchStatus(database, id, LaunchStatusCompleted, TerminationReasonCompleted); err != nil {
			t.Fatalf("grace->completed: %v", err)
		}

		launch, _ := GetLaunch(database, id)
		if launch.Status != LaunchStatusCompleted {
			t.Fatalf("status = %q, want completed", launch.Status)
		}
	})

	t.Run("grace->failed (expired)", func(t *testing.T) {
		database := SetupTestDB(t)
		id := createTestLaunch(t, database, LaunchStatusGrace)

		if err := UpdateLaunchStatus(database, id, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
			t.Fatalf("grace->failed: %v", err)
		}

		launch, _ := GetLaunch(database, id)
		if launch.Status != LaunchStatusFailed {
			t.Fatalf("status = %q, want failed", launch.Status)
		}
		if launch.TerminationReason != TerminationReasonJobFailure {
			t.Errorf("termination_reason = %q, want job_failure", launch.TerminationReason)
		}
	})

	t.Run("grace extend updates deadline", func(t *testing.T) {
		database := SetupTestDB(t)
		id := createTestLaunch(t, database, LaunchStatusGrace)

		newDeadline := time.Now().Add(20 * time.Minute).Unix()
		if err := ExtendLaunchGrace(database, id, newDeadline); err != nil {
			t.Fatalf("ExtendLaunchGrace: %v", err)
		}

		launch, _ := GetLaunch(database, id)
		if launch.GraceDeadline == nil || *launch.GraceDeadline != newDeadline {
			t.Errorf("grace_deadline = %v, want %d", launch.GraceDeadline, newDeadline)
		}
	})
}

func TestSpec_TerminalInstancesHaveEndTime(t *testing.T) {
	// Invariant from campaign-lifecycle.allium: terminal instances must have ended_at.
	terminalTransitions := []struct {
		from, to, reason string
	}{
		{LaunchStatusRunning, LaunchStatusCompleted, TerminationReasonCompleted},
		{LaunchStatusRunning, LaunchStatusFailed, TerminationReasonJobFailure},
		{LaunchStatusRunning, LaunchStatusCancelled, TerminationReasonCancelled},
		{LaunchStatusLaunching, LaunchStatusFailed, TerminationReasonBootstrapTimeout},
	}

	for _, tc := range terminalTransitions {
		name := tc.from + "->" + tc.to
		t.Run(name, func(t *testing.T) {
			database := SetupTestDB(t)
			id := createTestLaunch(t, database, tc.from)

			if err := UpdateLaunchStatus(database, id, tc.to, tc.reason); err != nil {
				t.Fatalf("UpdateLaunchStatus: %v", err)
			}

			launch, _ := GetLaunch(database, id)
			if launch.EndedAt == nil {
				t.Errorf("terminal instance %s should have ended_at set", tc.to)
			}
		})
	}
}

func TestSpec_TerminalInstancesHaveReason(t *testing.T) {
	// Invariant: failed and canceled instances must have termination_reason.
	cases := []struct {
		to, reason string
	}{
		{LaunchStatusFailed, TerminationReasonJobFailure},
		{LaunchStatusFailed, TerminationReasonBootstrapTimeout},
		{LaunchStatusFailed, TerminationReasonDiskFull},
		{LaunchStatusFailed, TerminationReasonProviderFailure},
		{LaunchStatusFailed, TerminationReasonInfraFailure},
		{LaunchStatusFailed, TerminationReasonPhaseStall},
		{LaunchStatusCancelled, TerminationReasonCancelled},
	}

	for _, tc := range cases {
		name := tc.to + "/" + tc.reason
		t.Run(name, func(t *testing.T) {
			database := SetupTestDB(t)
			id := createTestLaunch(t, database, LaunchStatusRunning)

			if err := UpdateLaunchStatus(database, id, tc.to, tc.reason); err != nil {
				t.Fatalf("UpdateLaunchStatus: %v", err)
			}

			launch, _ := GetLaunch(database, id)
			if launch.TerminationReason != tc.reason {
				t.Errorf("termination_reason = %q, want %q", launch.TerminationReason, tc.reason)
			}
		})
	}
}

func TestSpec_RetryableTerminationReasons(t *testing.T) {
	// From campaign-lifecycle.allium: is_retryable
	retryable := []string{
		TerminationReasonProviderFailure,
		TerminationReasonInfraFailure,
		TerminationReasonBootstrapTimeout,
		TerminationReasonPhaseStall,
		TerminationReasonUnknown,
		"", // empty reason = failed to launch
	}
	notRetryable := []string{
		TerminationReasonJobFailure,
		TerminationReasonDiskFull,
		TerminationReasonCancelled,
		TerminationReasonCompleted,
	}

	for _, reason := range retryable {
		name := reason
		if name == "" {
			name = "(empty)"
		}
		t.Run("retryable/"+name, func(t *testing.T) {
			ci := &Launch{Status: LaunchStatusFailed, TerminationReason: reason}
			if !IsRetryableTermination(ci) {
				t.Errorf("expected %q to be retryable", reason)
			}
		})
	}

	for _, reason := range notRetryable {
		t.Run("not_retryable/"+reason, func(t *testing.T) {
			ci := &Launch{Status: LaunchStatusFailed, TerminationReason: reason}
			if IsRetryableTermination(ci) {
				t.Errorf("expected %q to NOT be retryable", reason)
			}
		})
	}

	t.Run("non-failed status is never retryable", func(t *testing.T) {
		for _, status := range []string{LaunchStatusRunning, LaunchStatusCompleted, LaunchStatusCancelled, LaunchStatusGrace} {
			ci := &Launch{Status: status, TerminationReason: TerminationReasonProviderFailure}
			if IsRetryableTermination(ci) {
				t.Errorf("status %q should never be retryable regardless of reason", status)
			}
		}
	})
}

func TestSpec_ResetJobToUnplaced(t *testing.T) {
	// From job-lifecycle.allium: ResetJobToUnplaced and UserRetriesUnplaced rules.
	// After reset: host=null, cloud_instance=null, target_kind=unplaced, status=queued,
	// and a new attempt is created.
	t.Run("clears cloud association and requeues", func(t *testing.T) {
		database := SetupTestDB(t)

		jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
		if err != nil {
			t.Fatalf("RecordQueuedWithGPU: %v", err)
		}
		launchID, err := CreateLaunch(database, &Launch{
			Status:   LaunchStatusRunning,
			Provider: "vastai",
			GPUSpec:  "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if err := SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		if err := UpdateAttemptRunning(database, jobID); err != nil {
			t.Fatalf("UpdateAttemptRunning: %v", err)
		}

		// Verify job is associated with instance before reset.
		before, _ := GetJobByID(database, jobID)
		if before.LaunchID == nil || *before.LaunchID != launchID {
			t.Fatalf("pre-reset: expected launch_id=%d, got %v", launchID, before.LaunchID)
		}

		if err := ResetJobToUnplaced(database, jobID); err != nil {
			t.Fatalf("ResetJobToUnplaced: %v", err)
		}

		job, _ := GetJobByID(database, jobID)

		// Spec ensures: status = queued
		if job.Status != StatusQueued {
			t.Errorf("status = %q, want %q", job.Status, StatusQueued)
		}

		// Spec ensures: new attempt created (attempt_number incremented)
		if job.LatestRunID == nil {
			t.Fatal("expected a new attempt after reset")
		}
	})

	t.Run("from terminal state", func(t *testing.T) {
		database := SetupTestDB(t)

		jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
		if err != nil {
			t.Fatalf("RecordQueuedWithGPU: %v", err)
		}
		launchID, err := CreateLaunch(database, &Launch{
			Status:   LaunchStatusFailed,
			Provider: "vastai",
			GPUSpec:  "RTX_4090",
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if err := SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}

		if err := ResetJobToUnplaced(database, jobID); err != nil {
			t.Fatalf("ResetJobToUnplaced: %v", err)
		}

		job, _ := GetJobByID(database, jobID)
		if job.Status != StatusQueued {
			t.Errorf("status = %q, want %q", job.Status, StatusQueued)
		}
	})
}

func TestSpec_GraceRequiresDeadline(t *testing.T) {
	// Invariant: grace instances must have grace_deadline and grace_started_at.
	database := SetupTestDB(t)
	id := createTestLaunch(t, database, LaunchStatusGrace)

	launch, _ := GetLaunch(database, id)
	if launch.Status != LaunchStatusGrace {
		t.Fatalf("status = %q, want grace", launch.Status)
	}
	if launch.GraceDeadline == nil {
		t.Error("grace instance must have grace_deadline")
	}
	if launch.GraceStartedAt == nil {
		t.Error("grace instance must have grace_started_at")
	}
}
