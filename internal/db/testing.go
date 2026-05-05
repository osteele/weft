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
	result, err := db.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at)
		 VALUES (?, 1, ?, ?, ?, ?)`,
		id, o.host, cloudInstanceIDPtr, status, now,
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

// setupStatsTestDB creates an in-memory SQLite database with the launches,
// jobs, job_attempts, and job_phase_timings tables needed by both
// overhead_stats and bootstrap_stats tests.
func setupStatsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}

	for _, ddl := range []string{
		`CREATE TABLE campaigns (
			id INTEGER PRIMARY KEY,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			ended_at INTEGER
		)`,
		`CREATE TABLE launches (
			id INTEGER PRIMARY KEY,
			campaign_id INTEGER,
			host_id INTEGER,
			status TEXT NOT NULL,
			provider TEXT,
			gpu_class TEXT,
			data_center TEXT,
			dl_perf REAL,
			inet_down_mbps REAL,
			inet_up_mbps REAL,
			reliability REAL,
			gpu_spec TEXT,
			gpu_mem_gb INTEGER,
			max_spend_cents INTEGER,
			max_time_seconds INTEGER,
			actual_spend_cents INTEGER,
			created_at INTEGER NOT NULL DEFAULT 0,
			ready_at INTEGER,
			launched_at INTEGER,
			ended_at INTEGER,
			resolved_gpu_name TEXT,
			cost_per_hour_cents INTEGER,
			num_gpus INTEGER,
			cuda_version REAL,
			cpu_cores_effective INTEGER,
			cpu_name TEXT,
			ram_gb INTEGER,
			provider_instance_id TEXT,
			provider_running_at INTEGER,
			instance_role TEXT DEFAULT 'worker',
			docker_image TEXT
		)`,
		createJobsTableSQL("jobs", false),
		`CREATE TABLE job_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL,
			attempt_number INTEGER NOT NULL DEFAULT 1,
			host TEXT DEFAULT '',
			launch_id INTEGER,
			status TEXT DEFAULT 'queued',
			queued_at INTEGER,
			start_time INTEGER,
			end_time INTEGER,
			exit_code INTEGER,
			error_message TEXT,
			failure_reason TEXT,
			error_diagnosis TEXT,
			session_name TEXT,
			remote_id TEXT,
			remote_state TEXT,
			backend TEXT,
			last_synced_status TEXT,
			pending_status TEXT,
			pending_at INTEGER,
			cost REAL,
			placement_meta TEXT,
			job_metadata TEXT,
			observed_inputs TEXT,
			cloud_outcome TEXT
		)`,
		`CREATE TABLE job_phase_timings (
			job_id INTEGER PRIMARY KEY,
			wrapper_start INTEGER,
			setup_start INTEGER,
			setup_end INTEGER,
			run_start INTEGER,
			run_end INTEGER,
			upload_start INTEGER,
			upload_end INTEGER,
			upload_results_bytes INTEGER,
			upload_workspace_bytes INTEGER,
			cache_hf_bytes INTEGER,
			cache_uv_bytes INTEGER,
			cache_uv_post_bytes INTEGER,
			cache_hf_post_bytes INTEGER,
			uv_sync_seconds INTEGER,
			disk_used_bytes INTEGER,
			disk_total_bytes INTEGER,
			peak_gpu_mem_mib INTEGER,
			mean_gpu_util INTEGER,
			peak_gpu_util INTEGER
		)`,
		`CREATE TABLE bootstrap_transitions (
			launch_id INTEGER NOT NULL,
			stage TEXT NOT NULL,
			entered_at INTEGER NOT NULL,
			PRIMARY KEY (launch_id, stage, entered_at)
		)`,
	} {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

// suppress unused warnings — these are for future test use
var _ = withStartTime
var _ = withEndTime
var _ = withExitCode
var _ = withErrorMessage
var _ = withFailureReason
var _ = time.Now
