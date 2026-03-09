package scheduler

import (
	"context"
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
)

func TestLocalScheduler_PlacedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	sched := &LocalScheduler{
		db: database,
		placeFn: func(_ *sql.DB, _ placement.Constraints, _ placement.JobPredictor) (*placement.PlacementResult, error) {
			return &placement.PlacementResult{Host: "mock-host"}, nil
		},
	}

	result, err := sched.Submit(context.Background(), &SubmitRequest{
		Command:    "echo hello",
		WorkingDir: "/tmp/test",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.Host != "mock-host" {
		t.Errorf("Host = %q, want %q", result.Host, "mock-host")
	}
	if result.JobID == 0 {
		t.Error("expected non-zero JobID")
	}
	if result.PlacementResult == nil {
		t.Error("expected non-nil PlacementResult")
	}

	job, err := db.GetJobByID(database, result.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, db.StatusQueued)
	}
	if job.Host != "mock-host" {
		t.Errorf("Host = %q, want %q", job.Host, "mock-host")
	}
}

func TestLocalScheduler_UnplacedJob(t *testing.T) {
	database := db.SetupTestDB(t)
	sched := &LocalScheduler{
		db: database,
		placeFn: func(_ *sql.DB, _ placement.Constraints, _ placement.JobPredictor) (*placement.PlacementResult, error) {
			return nil, placement.ErrNoEligibleHost
		},
	}

	result, err := sched.Submit(context.Background(), &SubmitRequest{
		Command:    "python train.py",
		WorkingDir: "/tmp/test",
		GPUClass:   "H100",
		GPUMemGB:   80,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.Host != "" {
		t.Errorf("Host = %q, want empty (unplaced)", result.Host)
	}
	if result.JobID == 0 {
		t.Error("expected non-zero JobID")
	}
	if result.PlacementResult != nil {
		t.Error("expected nil PlacementResult for unplaced job")
	}

	job, err := db.GetJobByID(database, result.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, db.StatusQueued)
	}
	if job.Host != "" {
		t.Errorf("Host = %q, want empty", job.Host)
	}
	if job.Command != "python train.py" {
		t.Errorf("Command = %q, want %q", job.Command, "python train.py")
	}
	if job.GPUClass != "H100" {
		t.Errorf("GPUClass = %q, want %q", job.GPUClass, "H100")
	}
}

func TestLocalScheduler_ExplicitHost_SkipsPlacement(t *testing.T) {
	database := db.SetupTestDB(t)
	placeCalled := false
	sched := &LocalScheduler{
		db: database,
		placeFn: func(_ *sql.DB, _ placement.Constraints, _ placement.JobPredictor) (*placement.PlacementResult, error) {
			placeCalled = true
			return nil, placement.ErrNoEligibleHost
		},
	}

	result, err := sched.Submit(context.Background(), &SubmitRequest{
		Host:       "explicit-host",
		Command:    "echo hello",
		WorkingDir: "/tmp/test",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if placeCalled {
		t.Error("placement should not be called when host is explicit")
	}
	if result.Host != "explicit-host" {
		t.Errorf("Host = %q, want %q", result.Host, "explicit-host")
	}
}
