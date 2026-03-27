package db

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

// SetupTestDB creates a temporary database for testing
func SetupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "weft-db-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

	// Override db path for testing
	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	database, err := Open()
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}

// insertTestJob inserts a job row with an attempt for testing.
// It accepts spec-level columns on jobs plus execution-state overrides on the attempt.
func insertTestJob(t *testing.T, db *sql.DB, id int64, command, workingDir, status string, opts ...testJobOpt) {
	t.Helper()
	o := testJobOptions{status: status}
	for _, fn := range opts {
		fn(&o)
	}
	if workingDir == "" {
		workingDir = "/tmp"
	}
	if command == "" {
		command = "echo test"
	}

	// Insert into jobs (spec columns only). The trigger creates an attempt.
	if id > 0 {
		if _, err := db.Exec(
			`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, ?, ?, 0)`,
			id, workingDir, command,
		); err != nil {
			t.Fatalf("insertTestJob: insert jobs: %v", err)
		}
	} else {
		result, err := db.Exec(
			`INSERT INTO jobs (working_dir, command, tombstoned) VALUES (?, ?, 0)`,
			workingDir, command,
		)
		if err != nil {
			t.Fatalf("insertTestJob: insert jobs: %v", err)
		}
		newID, _ := result.LastInsertId()
		id = newID
	}

	// Update the auto-created attempt with execution state
	setClauses := []string{fmt.Sprintf("status = '%s'", status)}
	args := []any{}
	if o.host != "" {
		setClauses = append(setClauses, "host = ?")
		args = append(args, o.host)
	}
	if o.cloudInstanceID > 0 {
		setClauses = append(setClauses, "cloud_instance_id = ?")
		args = append(args, o.cloudInstanceID)
	}
	if o.startTime > 0 {
		setClauses = append(setClauses, "start_time = ?")
		args = append(args, o.startTime)
	}
	if o.endTime > 0 {
		setClauses = append(setClauses, "end_time = ?")
		args = append(args, o.endTime)
	}
	if o.exitCode != nil {
		setClauses = append(setClauses, "exit_code = ?")
		args = append(args, *o.exitCode)
	}
	if o.errorMessage != "" {
		setClauses = append(setClauses, "error_message = ?")
		args = append(args, o.errorMessage)
	}
	if o.failureReason != "" {
		setClauses = append(setClauses, "failure_reason = ?")
		args = append(args, o.failureReason)
	}
	args = append(args, id)
	query := fmt.Sprintf("UPDATE job_attempts SET %s WHERE job_id = ? AND end_time IS NULL",
		joinStrings(setClauses, ", "))
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("insertTestJob: update attempt: %v", err)
	}
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

type testJobOptions struct {
	status          string
	host            string
	cloudInstanceID int64
	startTime       int64
	endTime         int64
	exitCode        *int
	errorMessage    string
	failureReason   string
}

type testJobOpt func(*testJobOptions)

func withHost(host string) testJobOpt {
	return func(o *testJobOptions) { o.host = host }
}

func withCloudInstance(id int64) testJobOpt {
	return func(o *testJobOptions) { o.cloudInstanceID = id }
}

func withStartTime(t int64) testJobOpt {
	return func(o *testJobOptions) { o.startTime = t }
}

func withEndTime(t int64) testJobOpt {
	return func(o *testJobOptions) { o.endTime = t }
}

func withExitCode(code int) testJobOpt {
	return func(o *testJobOptions) { o.exitCode = &code }
}

func withErrorMessage(msg string) testJobOpt {
	return func(o *testJobOptions) { o.errorMessage = msg }
}

func withFailureReason(reason string) testJobOpt {
	return func(o *testJobOptions) { o.failureReason = reason }
}

// suppress unused warnings — these are for future test use
var _ = withStartTime
var _ = withEndTime
var _ = withExitCode
var _ = withErrorMessage
var _ = withFailureReason
var _ = time.Now
