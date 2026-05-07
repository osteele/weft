package orchestration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestWaitForLaunchedInstanceReady_TerminalStatusIncludesTerminationDetail(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	detail := `provider instance stuck in "loading" status for 5m0s - terminating`
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, detail); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	_, err = waitForLaunchedInstanceReady(
		context.Background(),
		database,
		nil,
		nil,
		nil,
		[]int64{instanceID},
		time.Minute,
		time.Second,
		nil,
	)
	if err == nil {
		t.Fatal("waitForLaunchedInstanceReady err = nil, want terminal error")
	}
	message := err.Error()
	for _, want := range []string{
		"instance " + ids.FormatInstanceID(instanceID) + " ended before agent_ready (failed)",
		detail,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want substring %q", message, want)
		}
	}
}

func TestWaitForLaunchedInstanceReady_TerminalStatusIncludesTerminationReason(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonBootstrapTimeout); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	_, err = waitForLaunchedInstanceReady(
		context.Background(),
		database,
		nil,
		nil,
		nil,
		[]int64{instanceID},
		time.Minute,
		time.Second,
		nil,
	)
	if err == nil {
		t.Fatal("waitForLaunchedInstanceReady err = nil, want terminal error")
	}
	if want := "bootstrap timeout"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}
}

func TestFormatNewInstanceWaitStatusIncludesElapsedAndPhase(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		BootstrapStage: "deps_installed",
		InstancePhase:  "setup:uv",
		JobProgressID:  42,
		JobProgressPct: 17,
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	got := formatNewInstanceWaitStatus(database, []int64{instanceID}, 65*time.Second)
	for _, want := range []string{
		"elapsed 1m 5s",
		"bootstrap=deps_installed",
		"phase=setup:uv",
		"wj42 17%",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status = %q, want substring %q", got, want)
		}
	}
}

func TestIsRetryableNewInstanceLaunchError(t *testing.T) {
	if !IsRetryableNewInstanceLaunchError(ErrNoNewInstanceOffer) {
		t.Fatal("no new-instance offer should be retryable")
	}

	retryable := &InstanceEndedBeforeReadyError{
		InstanceID: 1,
		Launch: &db.Launch{
			Status:            db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonInfraFailure,
		},
	}
	if !IsRetryableNewInstanceLaunchError(retryable) {
		t.Fatal("infra failure before agent_ready should be retryable")
	}

	jobFailure := &InstanceEndedBeforeReadyError{
		InstanceID: 2,
		Launch: &db.Launch{
			Status:            db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonJobFailure,
		},
	}
	if IsRetryableNewInstanceLaunchError(jobFailure) {
		t.Fatal("job failure before agent_ready should not be retryable")
	}
}
