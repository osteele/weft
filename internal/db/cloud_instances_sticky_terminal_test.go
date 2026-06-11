// Regression tests for sticky terminal launch statuses: once an instance
// reaches a terminal status (completed/failed/canceled), late non-terminal
// writes — e.g. LaunchInstance's final "running" write landing after a user
// terminate — must not revive the row. Otherwise the reconciler later
// re-fails the instance, recording a user cancellation as an infra failure.
package db

import (
	"errors"
	"testing"
	"time"
)

func TestUpdateLaunchStatus_RunningWriteCannotReviveTerminal(t *testing.T) {
	database := SetupTestDB(t)
	id := createTestLaunch(t, database, LaunchStatusLaunching)

	// User terminates while the launch flow is still in flight.
	if err := UpdateLaunchStatus(database, id, LaunchStatusCancelled, TerminationReasonCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The launch flow's late "running" write must be rejected.
	err := UpdateLaunchStatus(database, id, LaunchStatusRunning)
	if !errors.Is(err, ErrLaunchTerminal) {
		t.Fatalf("UpdateLaunchStatus(running) error = %v, want ErrLaunchTerminal", err)
	}

	launch, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != LaunchStatusCancelled {
		t.Errorf("status = %q, want %q (terminal status must be sticky)", launch.Status, LaunchStatusCancelled)
	}
	if launch.TerminationReason != TerminationReasonCancelled {
		t.Errorf("termination_reason = %q, want %q", launch.TerminationReason, TerminationReasonCancelled)
	}
}

func TestUpdateLaunchStatus_DefaultBranchCannotReviveTerminal(t *testing.T) {
	database := SetupTestDB(t)
	id := createTestLaunch(t, database, LaunchStatusRunning)

	if err := UpdateLaunchStatus(database, id, LaunchStatusFailed, TerminationReasonJobFailure); err != nil {
		t.Fatalf("fail: %v", err)
	}

	for _, status := range []string{LaunchStatusLaunching, LaunchStatusPaused} {
		err := UpdateLaunchStatus(database, id, status)
		if !errors.Is(err, ErrLaunchTerminal) {
			t.Fatalf("UpdateLaunchStatus(%s) error = %v, want ErrLaunchTerminal", status, err)
		}
	}

	launch, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != LaunchStatusFailed {
		t.Errorf("status = %q, want %q", launch.Status, LaunchStatusFailed)
	}
}

func TestUpdateLaunchStatus_MissingRowStaysSilentNoOp(t *testing.T) {
	database := SetupTestDB(t)
	if err := UpdateLaunchStatus(database, 999999, LaunchStatusRunning); err != nil {
		t.Fatalf("UpdateLaunchStatus on missing row = %v, want nil (no-op)", err)
	}
}

func TestSetLaunchGraceStarted_CannotReviveTerminal(t *testing.T) {
	database := SetupTestDB(t)
	id := createTestLaunch(t, database, LaunchStatusRunning)

	if err := UpdateLaunchStatus(database, id, LaunchStatusCancelled, TerminationReasonCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute).Unix()
	err := SetLaunchGraceStarted(database, id, deadline)
	if !errors.Is(err, ErrLaunchTerminal) {
		t.Fatalf("SetLaunchGraceStarted error = %v, want ErrLaunchTerminal", err)
	}

	launch, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.Status != LaunchStatusCancelled {
		t.Errorf("status = %q, want %q", launch.Status, LaunchStatusCancelled)
	}
	if launch.GraceStartedAt != nil {
		t.Errorf("grace_started_at = %v, want nil", *launch.GraceStartedAt)
	}
	if launch.GraceDeadline != nil {
		t.Errorf("grace_deadline = %v, want nil", *launch.GraceDeadline)
	}
}

func TestSetLaunchTerminationRequested_StampsAndPreservesEarlierValue(t *testing.T) {
	database := SetupTestDB(t)
	id := createTestLaunch(t, database, LaunchStatusRunning)

	if err := SetLaunchTerminationRequested(database, id); err != nil {
		t.Fatalf("SetLaunchTerminationRequested: %v", err)
	}
	launch, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.TerminationRequestedAt == nil {
		t.Fatal("termination_requested_at = nil, want stamped")
	}
	first := *launch.TerminationRequestedAt

	// Force a distinct earlier timestamp, then re-stamp: the original
	// request time must be preserved.
	if _, err := database.Exec(`UPDATE launches SET termination_requested_at = ? WHERE id = ?`, first-100, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := SetLaunchTerminationRequested(database, id); err != nil {
		t.Fatalf("SetLaunchTerminationRequested (second): %v", err)
	}
	launch, err = GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.TerminationRequestedAt == nil || *launch.TerminationRequestedAt != first-100 {
		t.Errorf("termination_requested_at = %v, want preserved %d", launch.TerminationRequestedAt, first-100)
	}
}
