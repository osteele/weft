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

	// Insert a job with a gpu_oom diagnosis on a 24GB GPU
	res, err := db.Exec(
		`INSERT INTO jobs (command, host, working_dir, status, tombstoned) VALUES (?, ?, '/tmp/test', 'completed', 0)`,
		"uv run python train.py", "cool30",
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID, _ := res.LastInsertId()

	_, err = db.Exec(
		`INSERT INTO job_runs (job_id, host, command, working_dir, status, exit_code, error_diagnosis, start_time, end_time, archived_at, archive_reason)
		 VALUES (?, ?, ?, '/tmp/test', 'completed', 1, ?, 1000, 2000, 0, '')`,
		jobID, "cool30", "uv run python train.py",
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory","gpu_capacity_gb":24}`,
	)
	if err != nil {
		t.Fatalf("insert job run: %v", err)
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
	res, err = db.Exec(
		`INSERT INTO jobs (command, host, working_dir, status, tombstoned) VALUES (?, ?, '/tmp/test', 'completed', 0)`,
		"uv run python train.py", "cool100",
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID2, _ := res.LastInsertId()

	_, err = db.Exec(
		`INSERT INTO job_runs (job_id, host, command, working_dir, status, exit_code, error_diagnosis, start_time, end_time, archived_at, archive_reason)
		 VALUES (?, ?, ?, '/tmp/test', 'completed', 1, ?, 1000, 2000, 0, '')`,
		jobID2, "cool100", "uv run python train.py",
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory","gpu_capacity_gb":80}`,
	)
	if err != nil {
		t.Fatalf("insert job run: %v", err)
	}

	floor, err = OOMFloor(db, "uv run python train.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 81 {
		t.Errorf("expected 81 (80+1), got %d", floor)
	}

	// OOM without gpu_capacity_gb → should not affect floor
	res, err = db.Exec(
		`INSERT INTO jobs (command, host, working_dir, status, tombstoned) VALUES (?, ?, '/tmp/test', 'completed', 0)`,
		"uv run python nocap.py", "cool30",
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID3, _ := res.LastInsertId()

	_, err = db.Exec(
		`INSERT INTO job_runs (job_id, host, command, working_dir, status, exit_code, error_diagnosis, start_time, end_time, archived_at, archive_reason)
		 VALUES (?, ?, ?, '/tmp/test', 'completed', 1, ?, 1000, 2000, 0, '')`,
		jobID3, "cool30", "uv run python nocap.py",
		`{"pattern":"gpu_oom","category":"environment","message":"GPU out of memory"}`,
	)
	if err != nil {
		t.Fatalf("insert job run: %v", err)
	}

	floor, err = OOMFloor(db, "uv run python nocap.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if floor != 0 {
		t.Errorf("expected 0 for OOM without gpu_capacity_gb, got %d", floor)
	}
}
