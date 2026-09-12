package campaign

import (
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// A provider destroy the provider rejects must land where operators look:
// wi7795 accumulated 52 failed destroy attempts whose cause existed only in
// the daemon log while weft info reported teardown completion unknown (wb128).
func TestExecuteActionPersistsDestroyFailureOnIntent(t *testing.T) {
	database := setupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "H100 PCIE"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, launchID, "stalled-1"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	ci, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	destroyErr := errors.New("vastai: destroy request timed out")
	client := &cloud.MockClient{
		ProviderVal:         cloud.ProviderVastai,
		DestroyInstanceFunc: func(string) error { return destroyErr },
	}
	action := InstanceAction{
		Kind:              ActionSpendCapReached,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonSpendCapReached,
		StallMessage:      "spend $0.60 reached the declared $0.60 ceiling",
		DestroyProvider:   true,
		AttemptOutcome:    db.AttemptOutcomeFailed,
	}

	reconciled, terminated := ExecuteAction(database, client, ci, action)
	if reconciled || terminated {
		t.Fatalf("ExecuteAction = (%v, %v), want deferred", reconciled, terminated)
	}

	after, err := db.GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch after destroy failure: %v", err)
	}
	if after.TerminationIntent == nil {
		t.Fatal("termination intent missing after destroy failure")
	}
	if after.TerminationIntent.LastError != destroyErr.Error() {
		t.Fatalf("intent last error = %q, want %q", after.TerminationIntent.LastError, destroyErr.Error())
	}
	if after.TerminationIntent.DestroySucceededAtUnix != 0 {
		t.Fatalf("destroy succeeded at = %d, want unset on failure", after.TerminationIntent.DestroySucceededAtUnix)
	}
	if after.TerminationIntent.DestroyAttempts != 1 {
		t.Fatalf("destroy attempts = %d, want 1", after.TerminationIntent.DestroyAttempts)
	}

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		LaunchID: launchID,
		Kind:     db.EventReconcileDestroyFailed,
	})
	if err != nil {
		t.Fatalf("ListLifecycleEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no %s event recorded", db.EventReconcileDestroyFailed)
	}
	if !strings.Contains(events[0].ErrorText, destroyErr.Error()) {
		t.Fatalf("destroy-failed event error = %q, want %q", events[0].ErrorText, destroyErr.Error())
	}
	if !strings.Contains(events[0].Detail, "spend $0.60") {
		t.Fatalf("destroy-failed event detail = %q, want the stall message", events[0].Detail)
	}
}
