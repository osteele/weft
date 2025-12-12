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
	ID          int64
	Host        string
	SessionName string // Deprecated: kept for backward compatibility with old jobs
	WorkingDir  string
	Command     string
	Description string
	StartTime   int64
	EndTime     *int64
	ExitCode    *int
	Status      string
}

// StatusStarting indicates a job is being set up
const StatusStarting = "starting"

// StatusRunning indicates a job is currently running
const StatusRunning = "running"

// StatusCompleted indicates a job finished (check exit code)
const StatusCompleted = "completed"

// StatusDead indicates a job terminated unexpectedly
const StatusDead = "dead"

// StatusPending indicates a job queued but not started
const StatusPending = "pending"

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
		start_time INTEGER NOT NULL,
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

	// Migration: make session_name nullable for existing tables
	// SQLite doesn't support ALTER COLUMN, but we can add new jobs with NULL session_name
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
	// Store error in description if there isn't one already
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, end_time = ?,
		 description = CASE WHEN description IS NULL OR description = '' THEN ? ELSE description END
		 WHERE id = ? AND status = ?`,
		StatusFailed, endTime, errorMsg, id, StatusStarting,
	)
	return err
}

// RecordCompletionByID updates a job by ID with its exit code and end time
func RecordCompletionByID(db *sql.DB, id int64, exitCode int, endTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?
		 WHERE id = ? AND status = ?`,
		exitCode, endTime, StatusCompleted, id, StatusRunning,
	)
	return err
}

// MarkDeadByID marks a running job as dead by ID
func MarkDeadByID(db *sql.DB, id int64) error {
	endTime := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET end_time = ?, status = ?
		 WHERE id = ? AND status = ?`,
		endTime, StatusDead, id, StatusRunning,
	)
	return err
}

// RecordPending records a pending job and returns its ID
// Note: session_name is deprecated; new pending jobs use NULL
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
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
		 FROM jobs WHERE host = ? AND session_name = ? ORDER BY start_time DESC LIMIT 1`,
		host, sessionName,
	)
	return scanJob(row)
}

// GetJobByID retrieves a job by ID
func GetJobByID(db *sql.DB, id int64) (*Job, error) {
	row := db.QueryRow(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
		 FROM jobs WHERE id = ?`,
		id,
	)
	return scanJob(row)
}

// GetPendingJob retrieves a pending job by ID
func GetPendingJob(db *sql.DB, id int64) (*Job, error) {
	row := db.QueryRow(
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
		 FROM jobs WHERE id = ? AND status = ?`,
		id, StatusPending,
	)
	return scanJob(row)
}

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var sessionName sql.NullString
	var desc sql.NullString
	var endTime sql.NullInt64
	var exitCode sql.NullInt64

	err := row.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &j.StartTime, &endTime, &exitCode, &j.Status)
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
	if endTime.Valid {
		j.EndTime = &endTime.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		j.ExitCode = &code
	}

	return &j, nil
}

// ListJobs returns jobs matching the given filters
func ListJobs(db *sql.DB, status, host string, limit int) ([]*Job, error) {
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status FROM jobs WHERE 1=1`
	args := []interface{}{}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}

	query += ` ORDER BY start_time DESC LIMIT ?`
	args = append(args, limit)

	return queryJobs(db, query, args...)
}

// ListPending returns pending jobs, optionally filtered by host
func ListPending(db *sql.DB, host string) ([]*Job, error) {
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status FROM jobs WHERE status = ?`
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
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
		 FROM jobs WHERE status = ? AND host = ? ORDER BY start_time DESC`,
		StatusRunning, host,
	)
}

// ListAllRunning returns all running jobs across all hosts
func ListAllRunning(db *sql.DB) ([]*Job, error) {
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
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

// SearchJobs searches jobs by description or command
func SearchJobs(db *sql.DB, query string, limit int) ([]*Job, error) {
	pattern := "%" + query + "%"
	return queryJobs(db,
		`SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status
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
	query := `SELECT id, host, session_name, working_dir, command, description, start_time, end_time, exit_code, status FROM jobs WHERE `
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
		var endTime sql.NullInt64
		var exitCode sql.NullInt64

		err := rows.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &j.StartTime, &endTime, &exitCode, &j.Status)
		if err != nil {
			return nil, err
		}

		if sessionName.Valid {
			j.SessionName = sessionName.String
		}
		if desc.Valid {
			j.Description = desc.String
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
