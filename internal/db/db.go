package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Job represents a remote job record
type Job struct {
	ID           int64
	Host         string
	SessionName  string // Deprecated: kept for backward compatibility with old jobs
	WorkingDir   string
	Command      string
	Description  string
	ErrorMessage string
	QueueName    string // Name of the queue this job belongs to (empty for non-queued jobs)
	StartTime    int64
	EndTime      *int64
	ExitCode     *int
	Status       string
}

// StatusStarting indicates a job is being set up
const StatusStarting = "starting"

// StatusRunning indicates a job is currently running
const StatusRunning = "running"

// StatusCompleted indicates a job finished (check exit code)
const StatusCompleted = "completed"

// StatusDead indicates a job terminated unexpectedly
const StatusDead = "dead"

// StatusPending indicates a job queued but not started (for --queue flag)
const StatusPending = "pending"

// StatusQueued indicates a job queued for sequential execution
const StatusQueued = "queued"

// StatusFailed indicates a job failed to start
const StatusFailed = "failed"

var dbPath string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	dbPath = filepath.Join(home, ".config", "remote-jobs", "jobs.db")
}

// Open opens the database, creating it if necessary
func Open() (*sql.DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return db, nil
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		session_name TEXT,
		working_dir TEXT NOT NULL,
		command TEXT NOT NULL,
		description TEXT,
		start_time INTEGER,
		end_time INTEGER,
		exit_code INTEGER,
		status TEXT NOT NULL DEFAULT 'running'
	);
	CREATE INDEX IF NOT EXISTS idx_jobs_host ON jobs(host);
	CREATE INDEX IF NOT EXISTS idx_jobs_session ON jobs(session_name);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_start ON jobs(start_time DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Migration: add error_message column if it doesn't exist
	// SQLite supports ALTER TABLE ADD COLUMN
	_, _ = db.Exec(`ALTER TABLE jobs ADD COLUMN error_message TEXT`)
	// Ignore error - column may already exist

	// Migration: add queue_name column for queued jobs
	_, _ = db.Exec(`ALTER TABLE jobs ADD COLUMN queue_name TEXT`)
	// Ignore error - column may already exist

	// Migration: make start_time nullable for queued jobs
	// SQLite doesn't support ALTER COLUMN, so we need to recreate the table
	if err := migrateStartTimeNullable(db); err != nil {
		return err
	}

	// Create hosts table for caching static host information
	hostsSchema := `
	CREATE TABLE IF NOT EXISTS hosts (
		name TEXT PRIMARY KEY,
		arch TEXT,
		os_version TEXT,
		model TEXT,
		cpu_count INTEGER,
		cpu_model TEXT,
		cpu_freq TEXT,
		mem_total TEXT,
		gpus_json TEXT,
		last_updated INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(hostsSchema); err != nil {
		return err
	}

	// Create deferred_operations table for operations pending on unreachable hosts
	deferredOpsSchema := `
	CREATE TABLE IF NOT EXISTS deferred_operations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		operation TEXT NOT NULL,
		job_id INTEGER NOT NULL,
		queue_name TEXT,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_deferred_ops_host ON deferred_operations(host);
	CREATE INDEX IF NOT EXISTS idx_deferred_ops_job ON deferred_operations(job_id);
	`
	if _, err := db.Exec(deferredOpsSchema); err != nil {
		return err
	}

	return nil
}

// checkStartTimeNotNull checks if the start_time column has a NOT NULL constraint.
// Returns true if migration is needed (column is NOT NULL).
func checkStartTimeNotNull(db *sql.DB) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(jobs)")
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typeName string
		var notNull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == "start_time" && notNull == 1 {
			// Close rows before returning to release any locks
			rows.Close()
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateStartTimeNullable migrates the jobs table to allow NULL start_time.
// This is needed for queued jobs that haven't started yet.
func migrateStartTimeNullable(db *sql.DB) error {
	// Check if start_time column is NOT NULL
	needsMigration, err := checkStartTimeNotNull(db)
	if err != nil {
		return err
	}

	if !needsMigration {
		return nil
	}

	// SQLite doesn't support ALTER COLUMN, so recreate the table
	// Execute each statement separately (SQLite auto-commits DDL)
	statements := []string{
		`CREATE TABLE jobs_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			host TEXT NOT NULL,
			session_name TEXT,
			working_dir TEXT NOT NULL,
			command TEXT NOT NULL,
			description TEXT,
			start_time INTEGER,
			end_time INTEGER,
			exit_code INTEGER,
			status TEXT NOT NULL DEFAULT 'running',
			error_message TEXT,
			queue_name TEXT
		)`,
		`INSERT INTO jobs_new SELECT id, host, session_name, working_dir, command, description,
			start_time, end_time, exit_code, status, error_message, queue_name FROM jobs`,
		`DROP TABLE jobs`,
		`ALTER TABLE jobs_new RENAME TO jobs`,
		`CREATE INDEX idx_jobs_host ON jobs(host)`,
		`CREATE INDEX idx_jobs_session ON jobs(session_name)`,
		`CREATE INDEX idx_jobs_status ON jobs(status)`,
		`CREATE INDEX idx_jobs_start ON jobs(start_time DESC)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate start_time: %w", err)
		}
	}

	return nil
}

// RecordStart records a new job start and returns its ID
// Deprecated: Use RecordJobStarting + UpdateJobRunning for new jobs
func RecordStart(db *sql.DB, host, sessionName, workingDir, command string, startTime int64, description string) (int64, error) {
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		host, sessionName, workingDir, command, description, startTime, StatusRunning,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// RecordJobStarting creates a new job with status="starting" and returns its ID
// This allows getting the job ID before starting the tmux session
func RecordJobStarting(db *sql.DB, host, workingDir, command, description string) (int64, error) {
	startTime := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status)
		 VALUES (?, NULL, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, startTime, StatusStarting,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// UpdateJobRunning transitions a starting job to running
func UpdateJobRunning(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusStarting,
	)
	return err
}

// UpdateJobFailed marks a starting job as failed
func UpdateJobFailed(db *sql.DB, id int64, errorMsg string) error {
	endTime := time.Now().Unix()
	// Store error in error_message column (not description) for debugging
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, end_time = ?, error_message = ? WHERE id = ? AND status = ?`,
		StatusFailed, endTime, errorMsg, id, StatusStarting,
	)
	return err
}

// UpdateJobPending converts a starting job to pending status (for --queue-on-fail)
func UpdateJobPending(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusPending, id, StatusStarting,
	)
	return err
}

// UpdateJobDescription updates the description for a job
func UpdateJobDescription(db *sql.DB, id int64, description string) error {
	_, err := db.Exec(
		`UPDATE jobs SET description = ? WHERE id = ?`,
		description, id,
	)
	return err
}

// UpdateJobHost updates the host for a job (only for queued jobs)
func UpdateJobHost(db *sql.DB, id int64, newHost string) error {
	_, err := db.Exec(
		`UPDATE jobs SET host = ? WHERE id = ? AND status = ?`,
		newHost, id, StatusQueued,
	)
	return err
}

// RecordCompletionByID updates a job by ID with its exit code and end time
func RecordCompletionByID(db *sql.DB, id int64, exitCode int, endTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?
		 WHERE id = ? AND status IN (?, ?)`,
		exitCode, endTime, StatusCompleted, id, StatusRunning, StatusQueued,
	)
	return err
}

// MarkDeadByID marks a running or queued job as dead by ID
func MarkDeadByID(db *sql.DB, id int64) error {
	endTime := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET end_time = ?, status = ?
		 WHERE id = ? AND status IN (?, ?)`,
		endTime, StatusDead, id, StatusRunning, StatusQueued,
	)
	return err
}

// RecordPending records a pending job and returns its ID
func RecordPending(db *sql.DB, host, workingDir, command, description string) (int64, error) {
	startTime := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status)
		 VALUES (?, NULL, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, startTime, StatusPending,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// RecordQueued records a queued job for sequential execution and returns its ID
// Note: start_time is NULL until the job actually starts running (set by UpdateQueuedToRunning)
func RecordQueued(db *sql.DB, host, workingDir, command, description, queueName string) (int64, error) {
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status, queue_name)
		 VALUES (?, NULL, ?, ?, ?, NULL, ?, ?)`,
		host, workingDir, command, description, StatusQueued, queueName,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ListQueued returns queued jobs for a host and queue name
func ListQueued(db *sql.DB, host, queueName string) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE status = ? AND host = ? AND queue_name = ? ORDER BY id ASC`,
		StatusQueued, host, queueName,
	)
}

// UpdateQueuedToRunning transitions a queued job to running
func UpdateQueuedToRunning(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, start_time = ? WHERE id = ? AND status = ?`,
		StatusRunning, time.Now().Unix(), id, StatusQueued,
	)
	return err
}

// RecordCompletion updates a job with its exit code and end time
func RecordCompletion(db *sql.DB, host, sessionName string, exitCode int, endTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?
		 WHERE host = ? AND session_name = ? AND status = ?`,
		exitCode, endTime, StatusCompleted, host, sessionName, StatusRunning,
	)
	return err
}

// MarkDead marks a running job as dead
func MarkDead(db *sql.DB, host, sessionName string) error {
	endTime := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET end_time = ?, status = ?
		 WHERE host = ? AND session_name = ? AND status = ?`,
		endTime, StatusDead, host, sessionName, StatusRunning,
	)
	return err
}

// MarkStarted transitions a pending job to running
func MarkStarted(db *sql.DB, id int64, startTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET start_time = ?, status = ? WHERE id = ? AND status = ?`,
		startTime, StatusRunning, id, StatusPending,
	)
	return err
}

// UpdateStartTime updates the start_time for a job (for jobs where start_time was initially null/0)
func UpdateStartTime(db *sql.DB, id int64, startTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET start_time = ? WHERE id = ? AND (start_time IS NULL OR start_time = 0)`,
		startTime, id,
	)
	return err
}

// DeletePending deletes a pending job
func DeletePending(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM jobs WHERE id = ? AND status = ?`, id, StatusPending)
	return err
}

// DeleteJob removes a job from the database without touching remote files
func DeleteJob(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// GetJob retrieves a job by host and session name (most recent)
func GetJob(db *sql.DB, host, sessionName string) (*Job, error) {
	row := db.QueryRow(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE host = ? AND session_name = ? ORDER BY start_time DESC LIMIT 1`,
		host, sessionName,
	)
	return scanJob(row)
}

// GetJobByID retrieves a job by ID
func GetJobByID(db *sql.DB, id int64) (*Job, error) {
	row := db.QueryRow(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE id = ?`,
		id,
	)
	return scanJob(row)
}

// GetPendingJob retrieves a pending job by ID
func GetPendingJob(db *sql.DB, id int64) (*Job, error) {
	row := db.QueryRow(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE id = ? AND status = ?`,
		id, StatusPending,
	)
	return scanJob(row)
}

// GetRunningJobsByHost retrieves all running jobs for a specific host
func GetRunningJobsByHost(db *sql.DB, host string) ([]*Job, error) {
	rows, err := db.Query(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE host = ? AND status = ? ORDER BY start_time DESC`,
		host, StatusRunning,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var sessionName sql.NullString
	var desc sql.NullString
	var errorMsg sql.NullString
	var queueName sql.NullString
	var startTime sql.NullInt64
	var endTime sql.NullInt64
	var exitCode sql.NullInt64

	err := row.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &queueName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if sessionName.Valid {
		j.SessionName = sessionName.String
	}
	if desc.Valid {
		j.Description = desc.String
	}
	if errorMsg.Valid {
		j.ErrorMessage = errorMsg.String
	}
	if queueName.Valid {
		j.QueueName = queueName.String
	}
	if startTime.Valid {
		j.StartTime = startTime.Int64
	}
	if endTime.Valid {
		j.EndTime = &endTime.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		j.ExitCode = &code
	}

	return &j, nil
}

// scanJobs scans multiple job rows
func scanJobs(rows *sql.Rows) ([]*Job, error) {
	var jobs []*Job
	for rows.Next() {
		var j Job
		var sessionName sql.NullString
		var desc sql.NullString
		var errorMsg sql.NullString
		var queueName sql.NullString
		var startTime sql.NullInt64
		var endTime sql.NullInt64
		var exitCode sql.NullInt64

		err := rows.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &queueName)
		if err != nil {
			return nil, err
		}

		if sessionName.Valid {
			j.SessionName = sessionName.String
		}
		if desc.Valid {
			j.Description = desc.String
		}
		if errorMsg.Valid {
			j.ErrorMessage = errorMsg.String
		}
		if startTime.Valid {
			j.StartTime = startTime.Int64
		}
		if queueName.Valid {
			j.QueueName = queueName.String
		}
		if endTime.Valid {
			j.EndTime = &endTime.Int64
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			j.ExitCode = &code
		}

		jobs = append(jobs, &j)
	}

	return jobs, rows.Err()
}

// ListJobs returns jobs matching the given filters
func ListJobs(db *sql.DB, status, host string, limit int) ([]*Job, error) {
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name FROM jobs WHERE 1=1`
	args := []interface{}{}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}

	// Order by job ID descending so newest jobs appear first
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	return queryJobs(db, query, args...)
}

// ListPending returns pending jobs, optionally filtered by host
func ListPending(db *sql.DB, host string) ([]*Job, error) {
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name FROM jobs WHERE status = ?`
	args := []interface{}{StatusPending}

	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}

	query += ` ORDER BY start_time DESC`
	return queryJobs(db, query, args...)
}

// ListRunning returns running jobs for a host
func ListRunning(db *sql.DB, host string) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE status = ? AND host = ? ORDER BY start_time DESC`,
		StatusRunning, host,
	)
}

// ListAllRunning returns all running jobs across all hosts
func ListAllRunning(db *sql.DB) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE status = ? ORDER BY start_time DESC`,
		StatusRunning,
	)
}

// ListUniqueRunningHosts returns all unique hosts with running jobs
func ListUniqueRunningHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status = ?`, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// ListUniqueActiveHosts returns unique hosts with running or queued jobs
func ListUniqueActiveHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status IN (?, ?)`, StatusRunning, StatusQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// ListHostsWithQueuedJobs returns unique hosts that have queued jobs
func ListHostsWithQueuedJobs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status = ?`, StatusQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// ListActiveJobs returns all running and queued jobs for a host
func ListActiveJobs(db *sql.DB, host string) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE host = ? AND status IN (?, ?) ORDER BY start_time ASC`,
		host, StatusRunning, StatusQueued,
	)
}

// ListAllQueued returns all queued jobs across all hosts
func ListAllQueued(db *sql.DB) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE status = ? ORDER BY start_time ASC`,
		StatusQueued,
	)
}

// ListRecentDeadQueueJobs returns recently-dead jobs that were queue runner jobs
// These should be re-checked in case they were incorrectly marked as dead
func ListRecentDeadQueueJobs(db *sql.DB, since int64) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE status = ? AND session_name IS NULL AND end_time > ? ORDER BY start_time ASC`,
		StatusDead, since,
	)
}

// ReviveDeadJob changes a dead job back to running (for incorrectly marked jobs)
func ReviveDeadJob(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, end_time = NULL WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusDead,
	)
	return err
}

// ListUniqueHosts returns all unique hosts from all jobs
func ListUniqueHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

// SearchJobs searches jobs by description or command
func SearchJobs(db *sql.DB, query string, limit int) ([]*Job, error) {
	pattern := "%" + query + "%"
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name
		 FROM jobs WHERE description LIKE ? OR command LIKE ? ORDER BY start_time DESC LIMIT ?`,
		pattern, pattern, limit,
	)
}

// CleanupOld deletes completed/dead jobs older than the given number of days
func CleanupOld(db *sql.DB, days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	result, err := db.Exec(
		`DELETE FROM jobs WHERE status IN (?, ?) AND start_time < ?`,
		StatusCompleted, StatusDead, cutoff,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneJobs deletes completed and/or dead jobs, optionally filtered by age
func PruneJobs(db *sql.DB, deadOnly bool, olderThan *time.Time) (int64, error) {
	var result sql.Result
	var err error

	if deadOnly {
		if olderThan != nil {
			result, err = db.Exec(
				`DELETE FROM jobs WHERE status = ? AND start_time < ?`,
				StatusDead, olderThan.Unix(),
			)
		} else {
			result, err = db.Exec(
				`DELETE FROM jobs WHERE status = ?`,
				StatusDead,
			)
		}
	} else {
		if olderThan != nil {
			result, err = db.Exec(
				`DELETE FROM jobs WHERE status IN (?, ?) AND start_time < ?`,
				StatusCompleted, StatusDead, olderThan.Unix(),
			)
		} else {
			result, err = db.Exec(
				`DELETE FROM jobs WHERE status IN (?, ?)`,
				StatusCompleted, StatusDead,
			)
		}
	}
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListJobsForPrune returns jobs that would be deleted by prune
func ListJobsForPrune(db *sql.DB, deadOnly bool, olderThan *time.Time) ([]*Job, error) {
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status, error_message, queue_name FROM jobs WHERE `
	var args []interface{}

	if deadOnly {
		query += `status = ?`
		args = append(args, StatusDead)
	} else {
		query += `status IN (?, ?)`
		args = append(args, StatusCompleted, StatusDead)
	}

	if olderThan != nil {
		query += ` AND start_time < ?`
		args = append(args, olderThan.Unix())
	}

	query += ` ORDER BY start_time DESC`
	return queryJobs(db, query, args...)
}

func queryJobs(db *sql.DB, query string, args ...interface{}) ([]*Job, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var j Job
		var sessionName sql.NullString
		var desc sql.NullString
		var errorMsg sql.NullString
		var queueName sql.NullString
		var startTime sql.NullInt64
		var endTime sql.NullInt64
		var exitCode sql.NullInt64

		err := rows.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &queueName)
		if err != nil {
			return nil, err
		}

		if sessionName.Valid {
			j.SessionName = sessionName.String
		}
		if desc.Valid {
			j.Description = desc.String
		}
		if errorMsg.Valid {
			j.ErrorMessage = errorMsg.String
		}
		if queueName.Valid {
			j.QueueName = queueName.String
		}
		if startTime.Valid {
			j.StartTime = startTime.Int64
		}
		if endTime.Valid {
			j.EndTime = &endTime.Int64
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			j.ExitCode = &code
		}

		jobs = append(jobs, &j)
	}

	return jobs, rows.Err()
}

// EffectiveWorkingDir returns the actual working directory for display.
// If the command starts with "cd <dir> &&", returns that directory instead.
func (j *Job) EffectiveWorkingDir() string {
	_, dir := j.ParseCdCommand()
	if dir != "" {
		return dir
	}
	return j.WorkingDir
}

// EffectiveCommand returns the actual command for display.
// If the command starts with "cd <dir> &&", returns the command after "&&".
// Also strips "export VAR=... && " prefixes for cleaner display.
func (j *Job) EffectiveCommand() string {
	cmd, _ := j.ParseCdCommand()
	if cmd == "" {
		cmd = j.Command
	}
	return stripExportPrefix(cmd)
}

// stripExportPrefix removes "export VAR=... && " prefixes from commands.
// Handles multiple consecutive exports: "export A=1 && export B=2 && cmd" -> "cmd"
func stripExportPrefix(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for strings.HasPrefix(cmd, "export ") {
		// Find the " && " separator
		andIdx := strings.Index(cmd, " && ")
		if andIdx == -1 {
			break
		}
		cmd = strings.TrimSpace(cmd[andIdx+4:])
	}
	return cmd
}

// ParseExportVars extracts environment variable assignments from the command.
// Returns a slice of "VAR=value" strings from "export VAR=value && " prefixes.
// Processes the command after stripping "cd dir && " if present.
func (j *Job) ParseExportVars() []string {
	// Get the command after any cd prefix
	cmd, _ := j.ParseCdCommand()
	if cmd == "" {
		cmd = j.Command
	}
	cmd = strings.TrimSpace(cmd)

	var envVars []string
	for strings.HasPrefix(cmd, "export ") {
		// Find the " && " separator
		andIdx := strings.Index(cmd, " && ")
		if andIdx == -1 {
			break
		}
		// Extract the VAR=value part (skip "export ")
		exportPart := strings.TrimSpace(cmd[7:andIdx])
		if exportPart != "" {
			envVars = append(envVars, exportPart)
		}
		cmd = strings.TrimSpace(cmd[andIdx+4:])
	}
	return envVars
}

// ParseCdCommand checks if the command starts with "cd <dir> &&" pattern.
// Returns (command_after_and, cd_directory) if pattern matches, or ("", "") if not.
func (j *Job) ParseCdCommand() (command, dir string) {
	cmd := strings.TrimSpace(j.Command)

	// Check for "cd " prefix
	if !strings.HasPrefix(cmd, "cd ") {
		return "", ""
	}

	// Find the " && " separator
	andIdx := strings.Index(cmd, " && ")
	if andIdx == -1 {
		return "", ""
	}

	// Extract the directory from "cd <dir>"
	cdPart := cmd[3:andIdx] // Skip "cd "
	dir = strings.TrimSpace(cdPart)

	// Handle quoted directories
	if (strings.HasPrefix(dir, "'") && strings.HasSuffix(dir, "'")) ||
		(strings.HasPrefix(dir, "\"") && strings.HasSuffix(dir, "\"")) {
		dir = dir[1 : len(dir)-1]
	}

	// Extract the command after " && "
	command = strings.TrimSpace(cmd[andIdx+4:])

	return command, dir
}

// CachedHostInfo represents cached static information about a host
type CachedHostInfo struct {
	Name        string
	Arch        string
	OSVersion   string
	Model       string
	CPUCount    int
	CPUModel    string
	CPUFreq     string
	MemTotal    string
	GPUsJSON    string // JSON array of GPU info
	LastUpdated int64  // Unix timestamp
}

// SaveCachedHostInfo saves or updates cached host information
func SaveCachedHostInfo(db *sql.DB, info *CachedHostInfo) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO hosts (name, arch, os_version, model, cpu_count, cpu_model, cpu_freq, mem_total, gpus_json, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		info.Name, info.Arch, info.OSVersion, info.Model, info.CPUCount, info.CPUModel, info.CPUFreq, info.MemTotal, info.GPUsJSON, info.LastUpdated,
	)
	return err
}

// LoadCachedHostInfo retrieves cached host information by name
func LoadCachedHostInfo(db *sql.DB, name string) (*CachedHostInfo, error) {
	row := db.QueryRow(`
		SELECT name, arch, os_version, model, cpu_count, cpu_model, cpu_freq, mem_total, gpus_json, last_updated
		FROM hosts WHERE name = ?`, name)

	var info CachedHostInfo
	var arch, osVersion, model, cpuModel, cpuFreq, memTotal, gpusJSON sql.NullString
	var cpuCount sql.NullInt64

	err := row.Scan(&info.Name, &arch, &osVersion, &model, &cpuCount, &cpuModel, &cpuFreq, &memTotal, &gpusJSON, &info.LastUpdated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if arch.Valid {
		info.Arch = arch.String
	}
	if osVersion.Valid {
		info.OSVersion = osVersion.String
	}
	if model.Valid {
		info.Model = model.String
	}
	if cpuCount.Valid {
		info.CPUCount = int(cpuCount.Int64)
	}
	if cpuModel.Valid {
		info.CPUModel = cpuModel.String
	}
	if cpuFreq.Valid {
		info.CPUFreq = cpuFreq.String
	}
	if memTotal.Valid {
		info.MemTotal = memTotal.String
	}
	if gpusJSON.Valid {
		info.GPUsJSON = gpusJSON.String
	}

	return &info, nil
}

// LoadAllCachedHosts retrieves all cached host information
func LoadAllCachedHosts(db *sql.DB) ([]*CachedHostInfo, error) {
	rows, err := db.Query(`
		SELECT name, arch, os_version, model, cpu_count, cpu_model, cpu_freq, mem_total, gpus_json, last_updated
		FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*CachedHostInfo
	for rows.Next() {
		var info CachedHostInfo
		var arch, osVersion, model, cpuModel, cpuFreq, memTotal, gpusJSON sql.NullString
		var cpuCount sql.NullInt64

		err := rows.Scan(&info.Name, &arch, &osVersion, &model, &cpuCount, &cpuModel, &cpuFreq, &memTotal, &gpusJSON, &info.LastUpdated)
		if err != nil {
			return nil, err
		}

		if arch.Valid {
			info.Arch = arch.String
		}
		if osVersion.Valid {
			info.OSVersion = osVersion.String
		}
		if model.Valid {
			info.Model = model.String
		}
		if cpuCount.Valid {
			info.CPUCount = int(cpuCount.Int64)
		}
		if cpuModel.Valid {
			info.CPUModel = cpuModel.String
		}
		if cpuFreq.Valid {
			info.CPUFreq = cpuFreq.String
		}
		if memTotal.Valid {
			info.MemTotal = memTotal.String
		}
		if gpusJSON.Valid {
			info.GPUsJSON = gpusJSON.String
		}

		hosts = append(hosts, &info)
	}

	return hosts, rows.Err()
}

// FormatDuration formats a duration in human-readable form
func FormatDuration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, " ")
}

// DeferredOperation represents an operation pending on an unreachable host
type DeferredOperation struct {
	ID        int64
	Host      string
	Operation string
	JobID     int64
	QueueName string
	CreatedAt int64
}

// Operation types for deferred operations
const (
	OpKillJob       = "kill_job"
	OpRemoveQueued  = "remove_queued"
	OpMoveFromQueue = "move_from_queue"
)

// AddDeferredOperation adds an operation to execute when host becomes reachable
func AddDeferredOperation(db *sql.DB, host, operation string, jobID int64, queueName string) error {
	createdAt := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO deferred_operations (host, operation, job_id, queue_name, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		host, operation, jobID, queueName, createdAt,
	)
	return err
}

// GetDeferredOperations returns all deferred operations for a host
func GetDeferredOperations(db *sql.DB, host string) ([]*DeferredOperation, error) {
	rows, err := db.Query(
		`SELECT id, host, operation, job_id, queue_name, created_at
		 FROM deferred_operations
		 WHERE host = ?
		 ORDER BY created_at ASC`,
		host,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []*DeferredOperation
	for rows.Next() {
		op := &DeferredOperation{}
		var queueName sql.NullString
		if err := rows.Scan(&op.ID, &op.Host, &op.Operation, &op.JobID, &queueName, &op.CreatedAt); err != nil {
			return nil, err
		}
		if queueName.Valid {
			op.QueueName = queueName.String
		}
		ops = append(ops, op)
	}

	return ops, rows.Err()
}

// DeleteDeferredOperation removes a deferred operation after execution
func DeleteDeferredOperation(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM deferred_operations WHERE id = ?`, id)
	return err
}
