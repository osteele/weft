package db

import (
	"database/sql"
	"testing"
	"time"
)

func TestOpenAttemptStateTracksAttemptLifecycle(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := createOpenAttemptTestJob(database, "echo ok")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	firstID, err := CreateAttempt(database, jobID, "cool30", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	assertOpenAttempt(t, database, jobID, firstID)

	if err := CloseAttempt(database, jobID, StatusCompleted, intPtr(0), time.Now().Unix()); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	assertNoOpenAttempt(t, database, jobID)

	secondID, err := CreateAttempt(database, jobID, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt retry: %v", err)
	}
	assertOpenAttempt(t, database, jobID, secondID)
}

func TestOpenAttemptStateRejectsInvalidPointers(t *testing.T) {
	database := setupTestDB(t)

	firstJob, err := createOpenAttemptTestJob(database, "echo first")
	if err != nil {
		t.Fatalf("create first job: %v", err)
	}
	secondJob, err := createOpenAttemptTestJob(database, "echo second")
	if err != nil {
		t.Fatalf("create second job: %v", err)
	}

	firstAttempt, err := CreateAttempt(database, firstJob, "cool30", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt first: %v", err)
	}
	secondAttempt, err := CreateAttempt(database, secondJob, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateAttempt second: %v", err)
	}

	if _, err := database.Exec(
		`UPDATE job_open_attempts SET attempt_id = ? WHERE job_id = ?`,
		secondAttempt, firstJob,
	); err == nil {
		t.Fatal("expected cross-job open attempt pointer update to fail")
	}

	if _, err := database.Exec(
		`UPDATE job_attempts SET end_time = ? WHERE id = ?`,
		time.Now().Unix(), firstAttempt,
	); err != nil {
		t.Fatalf("close first attempt: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO job_open_attempts(job_id, attempt_id) VALUES (?, ?)`,
		firstJob, firstAttempt,
	); err == nil {
		t.Fatal("expected closed attempt pointer insert to fail")
	}
}

func createOpenAttemptTestJob(database *sql.DB, command string) (int64, error) {
	result, err := database.Exec(
		`INSERT INTO jobs (working_dir, command, tombstoned) VALUES (?, ?, 0)`,
		"/tmp", command,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func assertOpenAttempt(t *testing.T, database *sql.DB, jobID, wantAttemptID int64) {
	t.Helper()
	var got int64
	if err := database.QueryRow(
		`SELECT attempt_id FROM job_open_attempts WHERE job_id = ?`,
		jobID,
	).Scan(&got); err != nil {
		t.Fatalf("read open attempt: %v", err)
	}
	if got != wantAttemptID {
		t.Fatalf("open attempt = %d, want %d", got, wantAttemptID)
	}
}

func assertNoOpenAttempt(t *testing.T, database *sql.DB, jobID int64) {
	t.Helper()
	var got int64
	err := database.QueryRow(
		`SELECT attempt_id FROM job_open_attempts WHERE job_id = ?`,
		jobID,
	).Scan(&got)
	if err != sql.ErrNoRows {
		t.Fatalf("open attempt query err = %v, want sql.ErrNoRows", err)
	}
}
