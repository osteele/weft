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

func TestJobStatusUsesExecutionTargetWhenPlacementShadowsAreMissing(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "", "/tmp/project", "echo rental", "rental")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET launch_id = NULL WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("clear launch shadow: %v", err)
	}

	var kind string
	var gotLaunchID sql.NullInt64
	if err := database.QueryRow(`SELECT effective_target_kind, launch_id FROM job_status WHERE id = ?`, jobID).Scan(&kind, &gotLaunchID); err != nil {
		t.Fatalf("query job_status: %v", err)
	}
	if kind != string(JobTargetRentalInstance) {
		t.Fatalf("effective_target_kind = %q, want %q", kind, JobTargetRentalInstance)
	}
	if !gotLaunchID.Valid || gotLaunchID.Int64 != launchID {
		t.Fatalf("launch_id = %v, want %d from execution target", gotLaunchID, launchID)
	}
}

func TestJobStatusUsesExecutionTargetWhenPlacementShadowsAreStale(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "echo host", "host")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	for _, trigger := range []string{
		"job_attempts_reject_target_shadow_mismatch_insert",
		"job_attempts_reject_target_shadow_mismatch_update",
	} {
		if _, err := database.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
			t.Fatalf("drop trigger %s: %v", trigger, err)
		}
	}
	if _, err := database.Exec(`UPDATE job_attempts SET host = '' WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("clear host shadow: %v", err)
	}

	var kind string
	if err := database.QueryRow(`SELECT effective_target_kind FROM job_status WHERE id = ?`, jobID).Scan(&kind); err != nil {
		t.Fatalf("query job_status: %v", err)
	}
	if kind != string(JobTargetInventoryHost) {
		t.Fatalf("effective_target_kind = %q, want %q from target_id", kind, JobTargetInventoryHost)
	}
}

func TestEstimatorCompatibilityColumnsRemain(t *testing.T) {
	database := SetupTestDB(t)

	for _, table := range []string{"job_attempts", "training_examples", "job_run_training_examples"} {
		if ok, err := relationExists(database, table); err != nil || !ok {
			t.Fatalf("relationExists(%s) = %v, %v", table, ok, err)
		}
	}
	for _, col := range []string{"host", "launch_id", "target_id"} {
		if !columnExistsForTest(t, database, "job_attempts", col) {
			t.Fatalf("missing job_attempts.%s compatibility column", col)
		}
	}
}

func columnExistsForTest(t *testing.T, database *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := database.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	return false
}

func TestJobAttemptsRejectTargetShadowMismatch(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "cool30", "/tmp/project", "echo host", "host")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	var targetID int64
	if err := database.QueryRow(`SELECT target_id FROM job_attempts WHERE job_id = ?`, jobID).Scan(&targetID); err != nil {
		t.Fatalf("query target_id: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET host = 'cool100' WHERE job_id = ?`, jobID); err == nil {
		t.Fatal("inventory target host mismatch succeeded; want trigger failure")
	}

	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	rentalJobID, err := RecordQueued(database, "", "/tmp/project", "echo rental", "rental")
	if err != nil {
		t.Fatalf("RecordQueued rental: %v", err)
	}
	if err := SetJobLaunchID(database, rentalJobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET host = 'cool30' WHERE job_id = ?`, rentalJobID); err == nil {
		t.Fatal("rental target inventory host shadow succeeded; want trigger failure")
	}

	var stillTargetID int64
	if err := database.QueryRow(`SELECT target_id FROM job_attempts WHERE job_id = ?`, jobID).Scan(&stillTargetID); err != nil {
		t.Fatalf("query final target_id: %v", err)
	}
	if stillTargetID != targetID {
		t.Fatalf("target_id changed after rejected update: got %d want %d", stillTargetID, targetID)
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
