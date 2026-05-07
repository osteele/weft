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
