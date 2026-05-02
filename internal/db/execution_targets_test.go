package db

import (
	"database/sql"
	"testing"
)

func TestExecutionTargetsBackfillInventoryAndRental(t *testing.T) {
	database := SetupTestDB(t)

	if err := SaveCachedHostInfo(database, &CachedHostInfo{Name: "cool30", LastUpdated: 123}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUClass: "a100",
		GPUMemGB: 80,
		NumGPUs:  1,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	targets, err := ListExecutionTargets(database)
	if err != nil {
		t.Fatalf("ListExecutionTargets: %v", err)
	}

	var sawHost, sawRental bool
	for _, target := range targets {
		switch {
		case target.Kind == ExecutionTargetInventoryHost && target.Host == "cool30":
			sawHost = true
			if target.Status != ExecutionTargetReady {
				t.Fatalf("inventory status = %q, want %q", target.Status, ExecutionTargetReady)
			}
		case target.Kind == ExecutionTargetRentalInstance && target.LaunchID != nil && *target.LaunchID == launchID:
			sawRental = true
			if target.Status != ExecutionTargetRunning {
				t.Fatalf("rental status = %q, want %q", target.Status, ExecutionTargetRunning)
			}
			if target.GPUClass != "a100" || target.GPUMemGB != 80 || target.NumGPUs != 1 {
				t.Fatalf("rental capacity = class=%q mem=%d gpus=%d", target.GPUClass, target.GPUMemGB, target.NumGPUs)
			}
		}
	}
	var targetID sql.NullInt64
	if err := database.QueryRow(`SELECT target_id FROM launches WHERE id = ?`, launchID).Scan(&targetID); err != nil {
		t.Fatalf("query launch target_id: %v", err)
	}
	if !targetID.Valid || targetID.Int64 == 0 {
		t.Fatal("launch target_id was not backfilled")
	}
	if !sawHost {
		t.Fatal("missing inventory execution target")
	}
	if !sawRental {
		t.Fatal("missing rental execution target")
	}
}

func TestSetLaunchCordonedMirrorsExecutionTarget(t *testing.T) {
	database := SetupTestDB(t)

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetLaunchCordoned(database, launchID, true, "maintenance"); err != nil {
		t.Fatalf("SetLaunchCordoned: %v", err)
	}

	targets, err := ListExecutionTargets(database)
	if err != nil {
		t.Fatalf("ListExecutionTargets: %v", err)
	}
	for _, target := range targets {
		if target.Kind == ExecutionTargetRentalInstance && target.LaunchID != nil && *target.LaunchID == launchID {
			if !target.Cordoned || target.CordonReason != "maintenance" {
				t.Fatalf("target cordon = %v %q, want true maintenance", target.Cordoned, target.CordonReason)
			}
			return
		}
	}
	t.Fatal("missing rental execution target")
}

func TestAssignJobHostSkipsCordonedInventoryTarget(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetInventoryExecutionTargetCordoned(database, "cool30", true, "driver upgrade"); err != nil {
		t.Fatalf("SetInventoryExecutionTargetCordoned: %v", err)
	}

	assigned, err := AssignJobHost(database, jobID, "cool30")
	if err != nil {
		t.Fatalf("AssignJobHost: %v", err)
	}
	if assigned {
		t.Fatal("AssignJobHost assigned to a cordoned inventory target")
	}
}
