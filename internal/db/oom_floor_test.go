package db

import (
	"testing"
)

func TestOOMFloor(t *testing.T) {
	db := setupTestDB(t)

	// No OOM history → 0
	floor, err := OOMFloor(db, "uv run python train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 0 {
		t.Errorf("expected 0, got %d", floor)
	}

	// Insert a job and set OOM diagnosis on its attempt (24GB GPU)
	jobID, err := RecordQueuedWithGPU(db, "cool30", "/tmp/test", "uv run python train.py", "test", "")
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE job_attempts SET error_diagnosis = ?, exit_code = 1, end_time = 2000
		 WHERE job_id = ? AND end_time IS NULL`,
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory","gpu_capacity_gb":24}`,
		jobID,
	); err != nil {
		t.Fatalf("set diagnosis: %v", err)
	}

	floor, err = OOMFloor(db, "uv run python train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 25 {
		t.Errorf("expected 25 (24+1), got %d", floor)
	}

	// Different command → no match
	floor, err = OOMFloor(db, "uv run python other.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 0 {
		t.Errorf("expected 0 for different command, got %d", floor)
	}

	// Add a second OOM on a larger GPU → floor should increase
	jobID2, err := RecordQueuedWithGPU(db, "cool100", "/tmp/test", "uv run python train.py", "test", "")
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE job_attempts SET error_diagnosis = ?, exit_code = 1, end_time = 2000
		 WHERE job_id = ? AND end_time IS NULL`,
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory","gpu_capacity_gb":80}`,
		jobID2,
	); err != nil {
		t.Fatalf("set diagnosis: %v", err)
	}

	floor, err = OOMFloor(db, "uv run python train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 81 {
		t.Errorf("expected 81 (80+1), got %d", floor)
	}

	// OOM without gpu_capacity_gb → should not affect floor
	jobID3, err := RecordQueuedWithGPU(db, "cool30", "/tmp/test", "uv run python nocap.py", "test", "")
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE job_attempts SET error_diagnosis = ?, exit_code = 1, end_time = 2000
		 WHERE job_id = ? AND end_time IS NULL`,
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory"}`,
		jobID3,
	); err != nil {
		t.Fatalf("set diagnosis: %v", err)
	}

	floor, err = OOMFloor(db, "uv run python nocap.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 0 {
		t.Errorf("expected 0 for OOM without gpu_capacity_gb, got %d", floor)
	}
}
