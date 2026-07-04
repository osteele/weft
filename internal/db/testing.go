package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db/migrations"
)

var (
	testDBTemplateOnce  sync.Once
	testDBTemplateBytes []byte
	testDBTemplateErr   error
)

// SetupTestDB creates an isolated database from a pre-migrated schema template.
func SetupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	// Create temp file for test database
	tmpFile, err := os.CreateTemp("", "weft-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()
	templateBytes := testDBTemplate(t)
	if err := os.WriteFile(tmpFile.Name(), templateBytes, 0o600); err != nil {
		t.Fatalf("Failed to seed test database: %v", err)
	}

	// Override db path for code paths that call db.Path or OpenForReading.
	cleanup := SetDBPath(tmpFile.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmpFile.Name())
	})

	// These databases are per-test throwaways. Keep FK checks, but skip
	// durability work that only matters for production database files.
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=foreign_keys(ON)&_txlock=immediate", tmpFile.Name())
	database, err := sql.Open("sqlite", connStr)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}

// SetupTestDBWithOpen creates a test database through the production Open path.
// Use this only for tests that intentionally exercise migrations, startup
// repair, WAL configuration, or Open/OpenForReading behavior.
func SetupTestDBWithOpen(t *testing.T) *sql.DB {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "weft-db-open-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

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

func testDBTemplate(t *testing.T) []byte {
	t.Helper()
	testDBTemplateOnce.Do(func() {
		testDBTemplateBytes, testDBTemplateErr = buildTestDBTemplate()
	})
	if testDBTemplateErr != nil {
		t.Fatalf("build test database template: %v", testDBTemplateErr)
	}
	return testDBTemplateBytes
}

func buildTestDBTemplate() ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "weft-test-template-*.db")
	if err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=foreign_keys(ON)&_txlock=immediate", tmpFile.Name())
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open template database: %w", err)
	}
	if err := migrations.Up(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize template database: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close template database: %w", err)
	}
	bytes, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		return nil, fmt.Errorf("read template database: %w", err)
	}
	return bytes, nil
}

// SetupTestBugDB creates a temporary standalone bug database for testing.
func SetupTestBugDB(t *testing.T) *sql.DB {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "weft-bug-db-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp bug database file: %v", err)
	}
	tmpFile.Close()

	cleanup := SetBugDBPath(tmpFile.Name())
	legacyImportWasEnabled := legacyBugImportEnabled
	legacyBugImportEnabled = false
	t.Cleanup(func() {
		legacyBugImportEnabled = legacyImportWasEnabled
		cleanup()
		os.Remove(tmpFile.Name())
	})

	database, err := OpenBugDB()
	if err != nil {
		t.Fatalf("Failed to open test bug database: %v", err)
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

	// Determine requested_status. The job_status view uses requested_status
	// as the primary override for canceled/killed/draft status.
	var requestedStatus *string
	switch status {
	case StatusCanceled, StatusKilled, StatusDraft:
		requestedStatus = &status
	}

	// Insert into jobs (spec columns only).
	if id > 0 {
		if _, err := db.Exec(
			`INSERT INTO jobs (id, working_dir, command, tombstoned, requested_status) VALUES (?, ?, ?, 0, ?)`,
			id, workingDir, command, requestedStatus,
		); err != nil {
			t.Fatalf("insertTestJob: insert jobs: %v", err)
		}
	} else {
		result, err := db.Exec(
			`INSERT INTO jobs (working_dir, command, tombstoned, requested_status) VALUES (?, ?, 0, ?)`,
			workingDir, command, requestedStatus,
		)
		if err != nil {
			t.Fatalf("insertTestJob: insert jobs: %v", err)
		}
		newID, _ := result.LastInsertId()
		id = newID
	}

	// Explicitly create an attempt for this job.
	var cloudInstanceIDPtr *int64
	if o.cloudInstanceID > 0 {
		cloudInstanceIDPtr = &o.cloudInstanceID
	}
	now := time.Now().Unix()
	// Stamp end_time at INSERT time for terminal-status attempts so the
	// TerminalJobsHaveEndTime invariant (enforced by triggers in 00006)
	// isn't violated mid-INSERT. The subsequent UPDATE below may override
	// end_time with an explicit value from opts.
	var initialEndTime any
	if IsTerminalStatus(status) {
		initialEndTime = now
	}
	result, err := db.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at, end_time)
		 VALUES (?, 1, ?, ?, ?, ?, ?)`,
		id, o.host, cloudInstanceIDPtr, status, now, initialEndTime,
	)
	if err != nil {
		t.Fatalf("insertTestJob: create attempt: %v", err)
	}
	attemptID, _ := result.LastInsertId()

	// Auto-set start_time/end_time based on status when not explicitly provided,
	// so the job_status view can derive status from attempt facts for cloud jobs.
	if o.startTime == 0 && (status == StatusRunning || IsTerminalStatus(status)) {
		o.startTime = now - 10
	}
	if o.endTime == 0 && IsTerminalStatus(status) {
		o.endTime = now
	}

	// Apply additional execution-state overrides to the attempt.
	setClauses := []string{}
	args := []any{}
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
	if len(setClauses) > 0 {
		args = append(args, attemptID)
		query := fmt.Sprintf("UPDATE job_attempts SET %s WHERE id = ?",
			joinStrings(setClauses, ", "))
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("insertTestJob: update attempt: %v", err)
		}
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

func withLaunch(id int64) testJobOpt {
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
