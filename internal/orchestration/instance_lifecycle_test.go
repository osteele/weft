package orchestration

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

// User-initiated terminate must stamp termination_requested_at
// (UserTerminatesInstance in specs/campaign-lifecycle.allium) alongside the
// canceled status.
func TestTerminateInstancesParallel_StampsTerminationRequestedAt(t *testing.T) {
	database := db.SetupTestDB(t)

	// No provider ID, so no provider destroy call is attempted.
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	terminated, errs := TerminateInstancesParallel(database, []int64{instanceID})
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
