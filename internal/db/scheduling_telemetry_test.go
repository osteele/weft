package db

import (
	"database/sql"
	"testing"
)

func insertTelemetryTestJob(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	res, err := database.Exec(`INSERT INTO jobs (working_dir, command, tombstoned) VALUES ('/tmp/p', 'python train.py', 0)`)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func TestLatestPlacementDecisionForJob_RoundTrip(t *testing.T) {
	database := setupTestDB(t)
	jobID := insertTelemetryTestJob(t, database)

	if _, err := RecordPlacementDecision(database, PlacementDecision{
		JobID:                  &jobID,
		DecisionKind:           "acted",
		Operation:              "autopilot_reuse",
		PlacementPolicyVersion: "weft-placement-v1",
		SelectedKind:           "reuse-instance",
		SelectedTarget:         "wi4424",
		Details: PlacementDecisionDetails{
			Instance:        "wi4424",
			GPU:             "NVIDIA 48GB",
			ColocatedBehind: 1,
			Why:             "reused running instance (queued behind 1 job(s) on its GPU)",
			Rejected:        []string{"wi4421: gpu too small"},
		},
	}, nil); err != nil {
		t.Fatalf("record reuse decision: %v", err)
	}

	got, err := LatestPlacementDecisionForJob(database, jobID)
	if err != nil {
		t.Fatalf("LatestPlacementDecisionForJob: %v", err)
	}
	if got == nil {
		t.Fatal("expected a decision, got nil")
	}
	if got.Operation != "autopilot_reuse" || got.SelectedKind != "reuse-instance" || got.SelectedTarget != "wi4424" {
		t.Fatalf("decision identity = %+v", got)
	}
	if got.Details.ColocatedBehind != 1 || got.Details.GPU != "NVIDIA 48GB" {
		t.Fatalf("decision details = %+v", got.Details)
	}
	if len(got.Details.Rejected) != 1 || got.Details.Rejected[0] != "wi4421: gpu too small" {
		t.Fatalf("rejected = %v", got.Details.Rejected)
	}
}

func TestLatestPlacementDecisionForJob_SkipsRunStub(t *testing.T) {
	database := setupTestDB(t)
	jobID := insertTelemetryTestJob(t, database)

	// The submission-time stub has no selected target.
	if _, err := RecordPlacementDecision(database, PlacementDecision{
		JobID:        &jobID,
		DecisionKind: "acted",
		Operation:    "run",
		Objective:    "fast",
	}, nil); err != nil {
		t.Fatalf("record run stub: %v", err)
	}

	// A job whose only decision is the stub has no actionable placement.
	if got, err := LatestPlacementDecisionForJob(database, jobID); err != nil {
		t.Fatalf("LatestPlacementDecisionForJob (stub only): %v", err)
	} else if got != nil {
		t.Fatalf("expected nil for stub-only job, got %+v", got)
	}

	// Once a real placement lands, it is returned even though the later-id
	// stub-style rows would otherwise win on ORDER BY id DESC.
	if _, err := RecordPlacementDecision(database, PlacementDecision{
		JobID:          &jobID,
		DecisionKind:   "acted",
		Operation:      "autopilot_launch",
		SelectedKind:   "launch-instance",
		SelectedTarget: "wi4428",
		Details:        PlacementDecisionDetails{Instance: "wi4428", GPU: "NVIDIA 48GB", CostPerHour: "$0.42/hr", Why: "launched a new instance"},
	}, nil); err != nil {
		t.Fatalf("record launch decision: %v", err)
	}
	got, err := LatestPlacementDecisionForJob(database, jobID)
	if err != nil {
		t.Fatalf("LatestPlacementDecisionForJob (after launch): %v", err)
	}
	if got == nil || got.SelectedTarget != "wi4428" || got.Operation != "autopilot_launch" {
		t.Fatalf("expected launch decision, got %+v", got)
	}
}

func TestLatestPlacementDecisionForJob_NilDB(t *testing.T) {
	if got, err := LatestPlacementDecisionForJob(nil, 1); err != nil || got != nil {
		t.Fatalf("nil db: got=%+v err=%v, want nil/nil", got, err)
	}
}
