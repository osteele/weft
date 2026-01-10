package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Job represents a remote job record
type Job struct {
	ID                   int64
	Host                 string
	SessionName          string // Deprecated: kept for backward compatibility with old jobs
	WorkingDir           string
	Command              string
	Description          string
	GeneratedDescription string // LLM-generated description for jobs without user descriptions
	GenerationHash       string // Hash of model+prompt+settings used to generate the description
	ErrorMessage         string
	QueueName            string // Name of the queue this job belongs to (empty for non-queued jobs)
	GPU                  string // CUDA_VISIBLE_DEVICES value (e.g., "0", "0,1")
	EnvVars              []string
	DepSpec              string // Dependency specification (e.g., "42" or "42+" for after-any)
	CreatedAt            int64  // When the job was created/queued (0 for legacy jobs)
	QueuedAt             int64  // When job was added to remote queue (for queue ordering)
	StartTime            int64
	EndTime              *int64
	ExitCode             *int
	Status               string
	Tombstoned           bool

	// Three-way merge state for reconciliation
	LastSyncedStatus string  // Base: what remote was at last successful sync
	PendingStatus    *string // Local: what user wants (nil = no pending change)
	PendingAt        *int64  // When pending state was set
}

const jobSelectColumns = `id, host, session_name, working_dir, command, description, generated_description, generation_hash, created_at, queued_at, start_time, end_time, exit_code, status, error_message, queue_name, gpu, env_vars, dep_spec, tombstoned, last_synced_status, pending_status, pending_at`

// StatusStarting indicates a job is being set up
const StatusStarting = "starting"

// StatusRunning indicates a job is currently running
const StatusRunning = "running"

// StatusCompleted indicates a job finished (check exit code)
const StatusCompleted = "completed"

// StatusDead indicates a job failed to start
const StatusDead = "dead"

// StatusQueued indicates a job queued for sequential execution
const StatusQueued = "queued"

// StatusFailed indicates a job terminated unexpectedly after starting
const StatusFailed = "failed"

// StatusKilled indicates a job was terminated by an explicit user action
const StatusKilled = "killed"

// StatusCanceled indicates a queued job was explicitly removed from the queue
const StatusCanceled = "canceled"

// StatusDraft indicates a job that exists locally but should not run remotely
const StatusDraft = "draft"

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

	// Use busy_timeout to wait up to 5 seconds for locks to be released
	// This allows multiple concurrent processes to access the database
	connStr := fmt.Sprintf("file:%s?_busy_timeout=5000", dbPath)
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// Enable WAL mode for better concurrent access (TUI + sync + CLI)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL mode: %w", err)
	}

	return db, nil
}

// Path returns the location of the database file on disk.
func Path() string {
	return dbPath
}

func SetDBPath(path string) func() {
	original := dbPath
	dbPath = path
	return func() {
		dbPath = original
	}
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
	status TEXT NOT NULL DEFAULT 'running',
	tombstoned INTEGER NOT NULL DEFAULT 0
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
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN error_message TEXT`); err != nil {
		return err
	}

	// Migration: add queue_name column for queued jobs
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN queue_name TEXT`); err != nil {
		return err
	}

	// Migration: add tombstoned column for soft-deleted jobs
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN tombstoned INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}

	// Migration: add created_at column to track when jobs were queued
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN created_at INTEGER`); err != nil {
		return err
	}

	// Migration: add gpu column for CUDA_VISIBLE_DEVICES
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN gpu TEXT`); err != nil {
		return err
	}

	// Migration: add env vars column for storing job environment
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN env_vars TEXT`); err != nil {
		return err
	}

	// Migration: add generated_description column for LLM-generated descriptions
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN generated_description TEXT`); err != nil {
		return err
	}

	// Migration: add generation_hash column for tracking LLM generation settings
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN generation_hash TEXT`); err != nil {
		return err
	}

	// Migration: populate gpu column from existing commands
	if err := migrateGPUFromCommands(db); err != nil {
		return err
	}

	// Migration: make start_time nullable for queued jobs
	// SQLite doesn't support ALTER COLUMN, so we need to recreate the table
	if err := migrateStartTimeNullable(db); err != nil {
		return err
	}

	// Migration: add three-way merge columns for reconciliation
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN last_synced_status TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN pending_status TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN pending_at INTEGER`); err != nil {
		return err
	}

	// Migration: add dep_spec column for job dependencies (e.g., "42" or "42+" for after-any)
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN dep_spec TEXT`); err != nil {
		return err
	}

	// Migration: add queued_at column for queue ordering (independent of job ID)
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN queued_at INTEGER`); err != nil {
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
		payload TEXT,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_deferred_ops_host ON deferred_operations(host);
	CREATE INDEX IF NOT EXISTS idx_deferred_ops_job ON deferred_operations(job_id);
	`
	if _, err := db.Exec(deferredOpsSchema); err != nil {
		return err
	}

	// Ensure payload column exists (older versions may lack it)
	if err := addColumnIfMissing(db, `ALTER TABLE deferred_operations ADD COLUMN payload TEXT`); err != nil {
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
			queue_name TEXT,
			tombstoned INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO jobs_new SELECT id, host, session_name, working_dir, command, description,
			start_time, end_time, exit_code, status, error_message, queue_name, tombstoned FROM jobs`,
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

func addColumnIfMissing(db *sql.DB, stmt string) error {
	if _, err := db.Exec(stmt); err != nil {
		if isDuplicateColumnError(err) {
			return nil
		}
		return err
	}
	return nil
}

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// migrateGPUFromCommands populates the gpu column from existing command strings
func migrateGPUFromCommands(db *sql.DB) error {
	// Only migrate jobs that have CUDA_VISIBLE_DEVICES in their command but no gpu set
	rows, err := db.Query(`SELECT id, command FROM jobs WHERE gpu IS NULL OR gpu = ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type jobUpdate struct {
		id  int64
		gpu string
	}
	var updates []jobUpdate

	for rows.Next() {
		var id int64
		var command string
		if err := rows.Scan(&id, &command); err != nil {
			return err
		}

		// Parse GPU from command using a temporary Job struct
		job := &Job{Command: command}
		if gpu := job.parseGPUFromCommand(); gpu != "" {
			updates = append(updates, jobUpdate{id: id, gpu: gpu})
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	// Apply updates
	for _, u := range updates {
		if _, err := db.Exec(`UPDATE jobs SET gpu = ? WHERE id = ?`, u.gpu, u.id); err != nil {
			return err
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
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, start_time, status)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, now, now, StatusStarting,
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

// UpdateJobFailed marks a starting job as failed to start
func UpdateJobFailed(db *sql.DB, id int64, errorMsg string) error {
	endTime := time.Now().Unix()
	// Store error in error_message column (not description) for debugging
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, end_time = ?, error_message = ? WHERE id = ? AND status = ?`,
		StatusDead, endTime, errorMsg, id, StatusStarting,
	)
	return err
}

// UpdateJobStartingToQueued transitions a starting job to queued state and assigns a queue name.
func UpdateJobStartingToQueued(db *sql.DB, id int64, queueName string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, queue_name = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL WHERE id = ? AND status = ?`,
		StatusQueued, queueName, id, StatusStarting,
	)
	return err
}

// UpdateJobRunningToQueued transitions a running job back to queued state.
func UpdateJobRunningToQueued(db *sql.DB, id int64, queueName string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, queue_name = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL WHERE id = ? AND status = ?`,
		StatusQueued, queueName, id, StatusRunning,
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

// UpdateJobGeneratedDescription updates the LLM-generated description for a job
func UpdateJobGeneratedDescription(db *sql.DB, id int64, generatedDesc, generationHash string) error {
	_, err := db.Exec(
		`UPDATE jobs SET generated_description = ?, generation_hash = ? WHERE id = ?`,
		generatedDesc, generationHash, id,
	)
	return err
}

// GetJobsNeedingDescriptions returns jobs without user or generated descriptions
func GetJobsNeedingDescriptions(db *sql.DB, limit int) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs
		WHERE tombstoned = 0
		AND (description IS NULL OR description = '')
		AND (generated_description IS NULL OR generated_description = '')
		ORDER BY id DESC LIMIT ?`, jobSelectColumns)
	return queryJobs(db, query, limit)
}

// UpdateJobWorkingDir updates the working directory for a queued job
func UpdateJobWorkingDir(db *sql.DB, id int64, workingDir string) error {
	_, err := db.Exec(
		`UPDATE jobs SET working_dir = ? WHERE id = ? AND status = ?`,
		workingDir, id, StatusQueued,
	)
	return err
}

// UpdateJobCommand updates the command for a queued job
func UpdateJobCommand(db *sql.DB, id int64, command string) error {
	_, err := db.Exec(
		`UPDATE jobs SET command = ? WHERE id = ? AND status = ?`,
		command, id, StatusQueued,
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

// RecordCompletionByID updates a job by ID with its exit code and end time.
// Clears session_name per spec: SessionImpliesRunning (session => status = running).
func RecordCompletionByID(db *sql.DB, id int64, exitCode int, endTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?, pending_status = NULL, session_name = NULL
		 WHERE id = ? AND status IN (?, ?)`,
		exitCode, endTime, StatusCompleted, id, StatusRunning, StatusQueued,
	)
	return err
}

// MarkDeadByID marks a running or queued job as failed (unexpected termination) by ID.
// Clears session_name per spec: SessionImpliesRunning (session => status = running).
func MarkDeadByID(db *sql.DB, id int64) error {
	endTime := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET end_time = ?, status = ?, pending_status = NULL, session_name = NULL
		 WHERE id = ? AND status IN (?, ?, ?)`,
		endTime, StatusFailed, id, StatusRunning, StatusStarting, StatusQueued,
	)
	return err
}

// MarkJobDraftPending updates a job to draft status locally and records pending cleanup.
func MarkJobDraftPending(db *sql.DB, id int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, pending_status = ?, pending_at = ? WHERE id = ?`,
		StatusDraft, StatusDraft, now, id,
	)
	return err
}

// MarkRunningByID transitions a job from starting to running
func MarkRunningByID(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusStarting,
	)
	return err
}

// MarkQueuedJobRunning transitions a queued job to running without touching start_time.
func MarkQueuedJobRunning(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusQueued,
	)
	return err
}

// MarkQueuedByID resets a job back to queued status (e.g., when sync finds it's still in queue)
func MarkQueuedByID(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL WHERE id = ?`,
		StatusQueued, id,
	)
	return err
}

// ClearQueueAssignment removes the queue association from a job so its queue
// metadata can be rebuilt (e.g., when re-queuing via deferred operations).
func ClearQueueAssignment(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET queue_name = NULL WHERE id = ?`,
		id,
	)
	return err
}

// SetPendingStatus sets the pending (target) status for a job.
// This represents what the user wants the job state to become.
func SetPendingStatus(db *sql.DB, jobID int64, status string) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET pending_status = ?, pending_at = ? WHERE id = ?`,
		status, now, jobID,
	)
	return err
}

// ClearPendingStatus clears the pending status after reconciliation succeeds.
func ClearPendingStatus(db *sql.DB, jobID int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET pending_status = NULL, pending_at = NULL WHERE id = ?`,
		jobID,
	)
	return err
}

// UpdateLastSyncedStatus updates the base status (what remote was at last sync).
func UpdateLastSyncedStatus(db *sql.DB, jobID int64, status string) error {
	_, err := db.Exec(
		`UPDATE jobs SET last_synced_status = ? WHERE id = ?`,
		status, jobID,
	)
	return err
}

// UpdateStatusAndLastSynced updates both the current status and last synced status together.
// Used when sync confirms the remote state.
func UpdateStatusAndLastSynced(db *sql.DB, jobID int64, status string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ? WHERE id = ?`,
		status, status, jobID,
	)
	return err
}

// ClearPendingAndUpdateStatus clears pending status and updates both status fields.
// Used when reconciliation succeeds or when accepting remote state.
// Clears session_name for non-running states per spec: SessionImpliesRunning.
func ClearPendingAndUpdateStatus(db *sql.DB, jobID int64, status string) error {
	if status == StatusQueued || status == StatusDraft {
		// Reset all execution-related fields when going back to queued/draft
		_, err := db.Exec(
			`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL WHERE id = ?`,
			status, status, jobID,
		)
		return err
	}
	if IsTerminalStatus(status) {
		// Clear session_name for terminal states per spec: SessionImpliesRunning
		_, err := db.Exec(
			`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL, session_name = NULL WHERE id = ?`,
			status, status, jobID,
		)
		return err
	}
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL WHERE id = ?`,
		status, status, jobID,
	)
	return err
}

// SetQueuedAtNow sets queued_at to the current time if it's not already set.
// Called when a job is successfully added to the remote queue.
func SetQueuedAtNow(db *sql.DB, jobID int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET queued_at = ? WHERE id = ? AND (queued_at IS NULL OR queued_at = 0)`,
		time.Now().Unix(), jobID,
	)
	return err
}

// SetQueuedAtBefore sets queued_at to one second before the current minimum for the host.
// Used for "move to front" operations to ensure this job runs first.
func SetQueuedAtBefore(db *sql.DB, jobID int64, host string) error {
	// Get the minimum queued_at for queued jobs on this host
	var minQueuedAt sql.NullInt64
	err := db.QueryRow(
		`SELECT MIN(queued_at) FROM jobs WHERE host = ? AND status = ? AND queued_at > 0 AND id != ? AND tombstoned = 0`,
		host, StatusQueued, jobID,
	).Scan(&minQueuedAt)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	var newQueuedAt int64
	if minQueuedAt.Valid && minQueuedAt.Int64 > 0 {
		newQueuedAt = minQueuedAt.Int64 - 1
	} else {
		// No other queued jobs, use current time
		newQueuedAt = time.Now().Unix()
	}

	_, err = db.Exec(`UPDATE jobs SET queued_at = ? WHERE id = ?`, newQueuedAt, jobID)
	return err
}

// IsTerminalStatus returns true if the status represents a terminal state.
func IsTerminalStatus(status string) bool {
	return status == StatusCompleted || status == StatusDead || status == StatusFailed || status == StatusKilled || status == StatusCanceled || status == StatusDraft
}

// CountQueuedByHost returns the number of queued jobs for a host
func CountQueuedByHost(db *sql.DB, host string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM jobs WHERE host = ? AND status = ? AND tombstoned = 0`,
		host, StatusQueued,
	).Scan(&count)
	return count, err
}

// RecordQueued records a queued job for sequential execution and returns its ID
// Note: start_time is NULL until the job actually starts running (set by UpdateQueuedToRunning)
func RecordQueued(db *sql.DB, host, workingDir, command, description, queueName string) (int64, error) {
	return RecordQueuedWithGPU(db, host, workingDir, command, description, queueName, "")
}

// RecordQueuedWithGPU records a queued job with GPU specification
func RecordQueuedWithGPU(db *sql.DB, host, workingDir, command, description, queueName, gpu string) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, queued_at, start_time, status, queue_name, gpu)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
		host, workingDir, command, description, now, now, StatusQueued, queueName, gpu,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// RecordDraftJobWithGPU records a job that should remain in draft locally.
func RecordDraftJobWithGPU(db *sql.DB, host, workingDir, command, description, queueName, gpu string) (int64, error) {
	return RecordDraftJob(db, host, workingDir, command, description, queueName, gpu, "")
}

// RecordDraftJob records a job that should remain in draft locally, with optional dependency.
func RecordDraftJob(db *sql.DB, host, workingDir, command, description, queueName, gpu, depSpec string) (int64, error) {
	createdAt := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, start_time, status, queue_name, gpu, dep_spec, last_synced_status)
		 VALUES (?, NULL, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, createdAt, StatusDraft, queueName, gpu, depSpec, StatusDraft,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// SetJobGPU updates the GPU field for a job
func SetJobGPU(db *sql.DB, jobID int64, gpu string) error {
	_, err := db.Exec(`UPDATE jobs SET gpu = ? WHERE id = ?`, gpu, jobID)
	return err
}

// SetJobEnvVars updates the stored environment variables for a job.
// The values are stored as a JSON array; passing nil or an empty slice clears the field.
func SetJobEnvVars(db *sql.DB, jobID int64, envVars []string) error {
	var value interface{}
	if len(envVars) > 0 {
		data, err := json.Marshal(envVars)
		if err != nil {
			return fmt.Errorf("encode env vars: %w", err)
		}
		value = string(data)
	}
	_, err := db.Exec(`UPDATE jobs SET env_vars = ? WHERE id = ?`, value, jobID)
	return err
}

// SetJobDepSpec stores the dependency specification for a job (e.g., "42" or "42:any").
// Passing an empty string clears the dependency field.
func SetJobDepSpec(db *sql.DB, jobID int64, depSpec string) error {
	if depSpec == "" {
		_, err := db.Exec(`UPDATE jobs SET dep_spec = NULL WHERE id = ?`, jobID)
		return err
	}
	_, err := db.Exec(`UPDATE jobs SET dep_spec = ? WHERE id = ?`, depSpec, jobID)
	return err
}

// ListQueuedJobsWithDependency returns queued jobs whose dependency list references depID.
func ListQueuedJobsWithDependency(db *sql.DB, depID int64) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND dep_spec IS NOT NULL AND dep_spec != '' AND tombstoned = 0`, jobSelectColumns)
	jobs, err := queryJobs(db, query, StatusQueued)
	if err != nil {
		return nil, err
	}
	var filtered []*Job
	for _, job := range jobs {
		if depSpecContains(job.DepSpec, depID) {
			filtered = append(filtered, job)
		}
	}
	return filtered, nil
}

func depSpecContains(spec string, depID int64) bool {
	if spec == "" || depID <= 0 {
		return false
	}
	target := strconv.FormatInt(depID, 10)
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, ":"); idx != -1 {
			part = part[:idx]
		}
		if part == target {
			return true
		}
	}
	return false
}

// ReplaceDepSpecID rewrites a dependency spec, replacing occurrences of oldID with newID.
// Returns the new spec string and whether a replacement was made.
func ReplaceDepSpecID(spec string, oldID, newID int64) (string, bool) {
	if strings.TrimSpace(spec) == "" || oldID <= 0 || newID <= 0 {
		return spec, false
	}
	oldStr := strconv.FormatInt(oldID, 10)
	newStr := strconv.FormatInt(newID, 10)
	parts := strings.Split(spec, ",")
	changed := false
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		mode := ""
		if idx := strings.Index(part, ":"); idx != -1 {
			mode = part[idx:]
			part = part[:idx]
		}
		if part == oldStr {
			part = newStr
			changed = true
		}
		parts[i] = part + mode
	}
	if !changed {
		return spec, false
	}
	return strings.Join(parts, ","), true
}

// ListQueued returns queued jobs for a host and queue name
func ListQueued(db *sql.DB, host, queueName string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND host = ? AND queue_name = ? AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued, host, queueName)
}

// UpdateQueuedToRunning transitions a queued job to running
func UpdateQueuedToRunning(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, start_time = ? WHERE id = ? AND status = ?`,
		StatusRunning, time.Now().Unix(), id, StatusQueued,
	)
	return err
}

// UpdateQueuedToRunningWithSession transitions a queued job to running and sets session_name.
// Used when starting a queued job directly via tmux (not through queue runner).
func UpdateQueuedToRunningWithSession(db *sql.DB, id int64, sessionName string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, start_time = ?, session_name = ? WHERE id = ? AND status = ?`,
		StatusRunning, time.Now().Unix(), sessionName, id, StatusQueued,
	)
	return err
}

// RecordCompletion updates a job with its exit code and end time
func RecordCompletion(db *sql.DB, host, sessionName string, exitCode int, endTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?, pending_status = NULL
		 WHERE host = ? AND session_name = ? AND status = ?`,
		exitCode, endTime, StatusCompleted, host, sessionName, StatusRunning,
	)
	return err
}

// MarkDead marks a running job as failed (unexpected termination)
func MarkDead(db *sql.DB, host, sessionName string) error {
	endTime := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE jobs SET end_time = ?, status = ?, pending_status = NULL
		 WHERE host = ? AND session_name = ? AND status = ?`,
		endTime, StatusFailed, host, sessionName, StatusRunning,
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

// DeleteJob removes a job from the database without touching remote files
func DeleteJob(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// TombstoneJob marks a job as tombstoned (soft-deleted) so it no longer appears in listings
func TombstoneJob(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE jobs SET tombstoned = 1 WHERE id = ?`, id)
	return err
}

// GetTombstonedActiveJobs returns jobs that are tombstoned but still marked as running/queued/starting
// These need to be killed on remote hosts during sync
func GetTombstonedActiveJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 1 AND status IN (?, ?, ?)`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning, StatusQueued, StatusStarting)
}

// GetJob retrieves a job by host and session name (most recent)
func GetJob(db *sql.DB, host, sessionName string) (*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND session_name = ? ORDER BY start_time DESC LIMIT 1`, jobSelectColumns)
	row := db.QueryRow(query, host, sessionName)
	return scanJob(row)
}

// GetJobByID retrieves a job by ID
func GetJobByID(db *sql.DB, id int64) (*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns)
	row := db.QueryRow(query, id)
	return scanJob(row)
}

// GetRunningJobsByHost retrieves all running jobs for a specific host
func GetRunningJobsByHost(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? ORDER BY start_time DESC`, jobSelectColumns)
	rows, err := db.Query(query, host, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

// GetJobsByHostAndStatus retrieves jobs for a specific host with a specific status
func GetJobsByHostAndStatus(db *sql.DB, host, status string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? AND tombstoned = 0 ORDER BY created_at ASC`, jobSelectColumns)
	rows, err := db.Query(query, host, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

// GetJobsByHost retrieves all jobs for a specific host
func GetJobsByHost(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? ORDER BY id DESC`, jobSelectColumns)
	rows, err := db.Query(query, host)
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
	var generatedDesc sql.NullString
	var generationHash sql.NullString
	var errorMsg sql.NullString
	var queueName sql.NullString
	var gpu sql.NullString
	var envVars sql.NullString
	var depSpec sql.NullString
	var createdAt sql.NullInt64
	var queuedAt sql.NullInt64
	var startTime sql.NullInt64
	var endTime sql.NullInt64
	var exitCode sql.NullInt64
	var tombstoned sql.NullInt64
	var lastSyncedStatus sql.NullString
	var pendingStatus sql.NullString
	var pendingAt sql.NullInt64

	err := row.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &generatedDesc, &generationHash, &createdAt, &queuedAt, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &queueName, &gpu, &envVars, &depSpec, &tombstoned, &lastSyncedStatus, &pendingStatus, &pendingAt)
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
	if generatedDesc.Valid {
		j.GeneratedDescription = generatedDesc.String
	}
	if generationHash.Valid {
		j.GenerationHash = generationHash.String
	}
	if errorMsg.Valid {
		j.ErrorMessage = errorMsg.String
	}
	if queueName.Valid {
		j.QueueName = queueName.String
	}
	if gpu.Valid {
		j.GPU = gpu.String
	}
	j.EnvVars = decodeEnvVars(envVars)
	if depSpec.Valid {
		j.DepSpec = depSpec.String
	}
	if createdAt.Valid {
		j.CreatedAt = createdAt.Int64
	}
	if queuedAt.Valid {
		j.QueuedAt = queuedAt.Int64
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
	if tombstoned.Valid {
		j.Tombstoned = tombstoned.Int64 != 0
	}
	if lastSyncedStatus.Valid {
		j.LastSyncedStatus = lastSyncedStatus.String
	}
	if pendingStatus.Valid {
		j.PendingStatus = &pendingStatus.String
	}
	if pendingAt.Valid {
		j.PendingAt = &pendingAt.Int64
	}

	return &j, nil
}

// decodeEnvVars converts the stored env_vars JSON (or legacy newline-separated values) into a slice.
func decodeEnvVars(value sql.NullString) []string {
	if !value.Valid {
		return nil
	}
	raw := strings.TrimSpace(value.String)
	if raw == "" {
		return nil
	}

	var envVars []string
	if err := json.Unmarshal([]byte(raw), &envVars); err == nil {
		return envVars
	}

	// Fallback: legacy newline-delimited storage
	parts := strings.Split(raw, "\n")
	envVars = envVars[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			envVars = append(envVars, part)
		}
	}
	if len(envVars) == 0 {
		return nil
	}
	return envVars
}

// scanJobs scans multiple job rows
func scanJobs(rows *sql.Rows) ([]*Job, error) {
	var jobs []*Job
	for rows.Next() {
		var j Job
		var sessionName sql.NullString
		var desc sql.NullString
		var generatedDesc sql.NullString
		var generationHash sql.NullString
		var errorMsg sql.NullString
		var queueName sql.NullString
		var gpu sql.NullString
		var envVars sql.NullString
		var depSpec sql.NullString
		var createdAt sql.NullInt64
		var queuedAt sql.NullInt64
		var startTime sql.NullInt64
		var endTime sql.NullInt64
		var exitCode sql.NullInt64
		var tombstoned sql.NullInt64
		var lastSyncedStatus sql.NullString
		var pendingStatus sql.NullString
		var pendingAt sql.NullInt64

		err := rows.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &generatedDesc, &generationHash, &createdAt, &queuedAt, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &queueName, &gpu, &envVars, &depSpec, &tombstoned, &lastSyncedStatus, &pendingStatus, &pendingAt)
		if err != nil {
			return nil, err
		}

		if sessionName.Valid {
			j.SessionName = sessionName.String
		}
		if desc.Valid {
			j.Description = desc.String
		}
		if generatedDesc.Valid {
			j.GeneratedDescription = generatedDesc.String
		}
		if generationHash.Valid {
			j.GenerationHash = generationHash.String
		}
		if errorMsg.Valid {
			j.ErrorMessage = errorMsg.String
		}
		if queueName.Valid {
			j.QueueName = queueName.String
		}
		if gpu.Valid {
			j.GPU = gpu.String
		}
		j.EnvVars = decodeEnvVars(envVars)
		if depSpec.Valid {
			j.DepSpec = depSpec.String
		}
		if createdAt.Valid {
			j.CreatedAt = createdAt.Int64
		}
		if queuedAt.Valid {
			j.QueuedAt = queuedAt.Int64
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
		if tombstoned.Valid {
			j.Tombstoned = tombstoned.Int64 != 0
		}
		if lastSyncedStatus.Valid {
			j.LastSyncedStatus = lastSyncedStatus.String
		}
		if pendingStatus.Valid {
			j.PendingStatus = &pendingStatus.String
		}
		if pendingAt.Valid {
			j.PendingAt = &pendingAt.Int64
		}

		jobs = append(jobs, &j)
	}

	return jobs, rows.Err()
}

// ListJobs returns jobs matching the given filters
func ListJobs(db *sql.DB, status, host string, limit int) ([]*Job, error) {
	return ListJobsWithMaxAge(db, status, host, limit, 0)
}

// ListJobsWithMaxAge returns jobs, optionally filtered by status, host, and age.
// maxAgeDays of 0 means no age limit.
func ListJobsWithMaxAge(db *sql.DB, status, host string, limit, maxAgeDays int) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 0`, jobSelectColumns)
	args := []interface{}{}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}
	if maxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -maxAgeDays).Unix()
		query += ` AND start_time > ?`
		args = append(args, cutoff)
	}

	// Order by running jobs first, then by job ID descending
	query += ` ORDER BY CASE WHEN status IN ('running', 'starting') THEN 0 ELSE 1 END, id DESC LIMIT ?`
	args = append(args, limit)

	return queryJobs(db, query, args...)
}

// ListRunning returns running jobs for a host
func ListRunning(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND host = ? AND tombstoned = 0 ORDER BY start_time DESC`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning, host)
}

// ListAllRunning returns all running jobs across all hosts
func ListAllRunning(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND tombstoned = 0 ORDER BY start_time DESC`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning)
}

// ListRecentFailed returns recently failed jobs (last 24 hours)
// Includes: completed with non-zero exit code, status=failed, status=dead
func ListRecentFailed(db *sql.DB, limit int) ([]*Job, error) {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	query := fmt.Sprintf(`SELECT %s FROM jobs
		WHERE tombstoned = 0
		AND end_time > ?
		AND (
			(status = ? AND exit_code IS NOT NULL AND exit_code != 0)
			OR status = ?
			OR status = ?
		)
		ORDER BY end_time DESC
		LIMIT ?`, jobSelectColumns)
	return queryJobs(db, query, cutoff, StatusCompleted, StatusFailed, StatusDead, limit)
}

// ListUniqueRunningHosts returns all unique hosts with running jobs
func ListUniqueRunningHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status = ? AND tombstoned = 0`, StatusRunning)
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

// ListUniqueActiveHosts returns unique hosts with running, queued, or pending draft jobs
func ListUniqueActiveHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs
		WHERE tombstoned = 0
		AND (
			status IN (?, ?)
			OR (status = ? AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?))
		)`,
		StatusRunning, StatusQueued, StatusDraft, StatusDraft, StatusDraft)
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
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status = ? AND tombstoned = 0`, StatusQueued)
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

// ListHostsWithDraftsPending returns hosts that have draft jobs needing remote cleanup.
func ListHostsWithDraftsPending(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status = ? AND tombstoned = 0 AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?)`,
		StatusDraft, StatusDraft, StatusDraft)
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
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status IN (?, ?, ?) AND tombstoned = 0 ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusRunning, StatusStarting, StatusQueued)
}

// ListDraftJobsPendingSync returns draft jobs that still need remote cleanup.
func ListDraftJobsPendingSync(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? AND tombstoned = 0 AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?) ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusDraft, StatusDraft, StatusDraft)
}

// ListAllQueued returns all queued jobs across all hosts
func ListAllQueued(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND tombstoned = 0 ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued)
}

// ListUniqueHosts returns all unique hosts from all jobs
func ListUniqueHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE tombstoned = 0 ORDER BY host`)
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
	stmt := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 0 AND (description LIKE ? OR command LIKE ?) ORDER BY start_time DESC LIMIT ?`, jobSelectColumns)
	return queryJobs(db, stmt, pattern, pattern, limit)
}

// CleanupOld deletes terminal jobs older than the given number of days
func CleanupOld(db *sql.DB, days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	result, err := db.Exec(
		`DELETE FROM jobs WHERE status IN (?, ?, ?, ?, ?) AND start_time < ?`,
		StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, cutoff,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneJobs tombstones terminal jobs so they no longer appear in listings.
func PruneJobs(db *sql.DB, deadOnly bool, olderThan *time.Time) (int64, error) {
	query := `UPDATE jobs SET tombstoned = 1 WHERE tombstoned = 0`
	args := []interface{}{}

	if deadOnly {
		query += ` AND status = ?`
		args = append(args, StatusDead)
	} else {
		query += ` AND status IN (?, ?, ?, ?, ?)`
		args = append(args, StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled)
	}

	if olderThan != nil {
		query += ` AND start_time < ?`
		args = append(args, olderThan.Unix())
	}

	result, err := db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListJobsForPrune returns jobs that would be deleted by prune
func ListJobsForPrune(db *sql.DB, deadOnly bool, olderThan *time.Time) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 0 AND `, jobSelectColumns)
	var args []interface{}

	if deadOnly {
		query += `status = ?`
		args = append(args, StatusDead)
	} else {
		query += `status IN (?, ?, ?, ?, ?)`
		args = append(args, StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled)
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

	return scanJobs(rows)
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

// DisplayWorkingDir returns a user-friendly directory string, falling back to
// the remote home when no explicit directory is set.
func (j *Job) DisplayWorkingDir() string {
	dir := strings.TrimSpace(j.EffectiveWorkingDir())
	if dir == "" {
		return "~ (remote home)"
	}
	return dir
}

// EffectiveStatus returns the status to use for UI decisions.
// Returns PendingStatus if set (the desired/target state), otherwise Status.
func (j *Job) EffectiveStatus() string {
	if j.PendingStatus != nil {
		return *j.PendingStatus
	}
	return j.Status
}

// EffectiveDescription returns the best description for display.
// Priority: user description > generated description > effective command.
func (j *Job) EffectiveDescription() string {
	if j.Description != "" {
		return j.Description
	}
	if j.GeneratedDescription != "" {
		return j.GeneratedDescription
	}
	return j.EffectiveCommand()
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

// GetGPU returns the GPU (CUDA_VISIBLE_DEVICES) value for this job.
// First checks the database GPU field, then falls back to parsing the command.
func (j *Job) GetGPU() string {
	// Prefer the database field if set
	if j.GPU != "" {
		return j.GPU
	}

	// Fall back to parsing from command for backwards compatibility
	return j.parseGPUFromCommand()
}

// parseGPUFromCommand extracts CUDA_VISIBLE_DEVICES from the job's command.
// Returns empty string if not found.
func (j *Job) parseGPUFromCommand() string {
	// Get command after cd prefix if present
	cmd := j.Command
	if afterCd, _ := j.ParseCdCommand(); afterCd != "" {
		cmd = afterCd
	}

	// First check for env prefix: "env CUDA_VISIBLE_DEVICES=0 ..."
	_, envVars := ParseEnvPrefix(cmd)
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			return strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
		}
	}

	// Then check exports (handles cd prefix first)
	for _, ev := range j.ParseExportVars() {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			return strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
		}
	}

	// Check in command itself for inline assignment: "CUDA_VISIBLE_DEVICES=0 python ..."
	parts := strings.Fields(cmd)
	for _, part := range parts {
		if strings.HasPrefix(part, "CUDA_VISIBLE_DEVICES=") {
			return strings.TrimPrefix(part, "CUDA_VISIBLE_DEVICES=")
		}
		// Stop at first non-assignment
		if !strings.Contains(part, "=") {
			break
		}
	}

	return ""
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
	return ParseCdPrefix(j.Command)
}

// ParseCdPrefix extracts "cd <dir> && " prefix from a command string.
// Returns (command_after_and, cd_directory) if pattern matches, or ("", "") if not.
func ParseCdPrefix(cmd string) (command, dir string) {
	cmd = strings.TrimSpace(cmd)

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

// ParseEnvPrefix extracts "env VAR=value ... " prefix from a command string.
// Returns (command_after_env, env_vars) where env_vars is a slice of "VAR=value" strings.
// If no env prefix is found, returns (original_cmd, nil).
func ParseEnvPrefix(cmd string) (command string, envVars []string) {
	cmd = strings.TrimSpace(cmd)

	// Check for "env " prefix
	if !strings.HasPrefix(cmd, "env ") {
		return cmd, nil
	}

	// Skip "env "
	rest := cmd[4:]

	// Parse VAR=value pairs until we hit a command (doesn't contain '=')
	parts := strings.Fields(rest)
	var envParts []string
	cmdStartIdx := 0

	for i, part := range parts {
		if strings.Contains(part, "=") {
			envParts = append(envParts, part)
			cmdStartIdx = i + 1
		} else {
			// This is the start of the actual command
			break
		}
	}

	if len(envParts) == 0 {
		return cmd, nil
	}

	// Reconstruct the command from remaining parts
	if cmdStartIdx < len(parts) {
		command = strings.Join(parts[cmdStartIdx:], " ")
	}

	return command, envParts
}

// ParseExportPrefix extracts "export VAR=value && " prefixes from a command string.
// Returns (command_after_exports, env_vars) where env_vars is a slice of "VAR=value" strings.
// Handles multiple consecutive exports: "export A=1 && export B=2 && cmd" -> ("cmd", ["A=1", "B=2"])
func ParseExportPrefix(cmd string) (command string, envVars []string) {
	cmd = strings.TrimSpace(cmd)

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

	return cmd, envVars
}

// NormalizeCommand parses a command and extracts working directory and environment variables.
// This is used when creating new jobs to split embedded cd/env prefixes into their proper fields.
// Returns (working_dir, command, env_vars).
// If workingDir is empty, no cd prefix was found.
func NormalizeCommand(cmd string) (workingDir, command string, envVars []string) {
	// First check for cd prefix
	remainder, dir := ParseCdPrefix(cmd)
	if dir != "" {
		workingDir = dir
		cmd = remainder
	}

	// Then check for env prefix
	cmd, envFromEnv := ParseEnvPrefix(cmd)
	envVars = append(envVars, envFromEnv...)

	// Then check for export prefix
	cmd, envFromExport := ParseExportPrefix(cmd)
	envVars = append(envVars, envFromExport...)

	return workingDir, cmd, envVars
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

// DeleteCachedHost removes a host from the hosts cache
func DeleteCachedHost(db *sql.DB, name string) error {
	_, err := db.Exec(`DELETE FROM hosts WHERE name = ?`, name)
	return err
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
	Payload   string
	CreatedAt int64
}

// Operation types for deferred operations
const (
	OpKillJob         = "kill_job"
	OpRemoveQueued    = "remove_queued"
	OpMoveFromQueue   = "move_from_queue"
	OpQueueJob        = "queue_job"
	OpStartQueuedJob  = "start_queued_job"
	OpUpdateQueuedJob = "update_queued_job"
	OpRunJob          = "run_job"     // Create and run a new job
	OpRestartJob      = "restart_job" // Restart a completed/dead job
)

// AddDeferredOperation adds an operation to execute when host becomes reachable
func AddDeferredOperation(db *sql.DB, host, operation string, jobID int64, queueName string, payload string) error {
	_, err := AddDeferredOperationReturningID(db, host, operation, jobID, queueName, payload)
	return err
}

// AddDeferredOperationReturningID adds an operation and returns its ID
func AddDeferredOperationReturningID(db *sql.DB, host, operation string, jobID int64, queueName string, payload string) (int64, error) {
	createdAt := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO deferred_operations (host, operation, job_id, queue_name, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		host, operation, jobID, queueName, payload, createdAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetDeferredOperations returns all deferred operations for a host
func GetDeferredOperations(db *sql.DB, host string) ([]*DeferredOperation, error) {
	rows, err := db.Query(
		`SELECT id, host, operation, job_id, queue_name, payload, created_at
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
		var payload sql.NullString
		if err := rows.Scan(&op.ID, &op.Host, &op.Operation, &op.JobID, &queueName, &payload, &op.CreatedAt); err != nil {
			return nil, err
		}
		if queueName.Valid {
			op.QueueName = queueName.String
		}
		if payload.Valid {
			op.Payload = payload.String
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

// DeletePendingOperationsForJob removes pending deferred operations of specific types for a job.
// This is used to remove incompatible operations, e.g., removing pending start operations
// when a kill is requested before the start was executed.
func DeletePendingOperationsForJob(db *sql.DB, jobID int64, operations ...string) (int64, error) {
	if len(operations) == 0 {
		return 0, nil
	}
	// Build placeholders for the IN clause
	placeholders := make([]string, len(operations))
	args := make([]interface{}, len(operations)+1)
	args[0] = jobID
	for i, op := range operations {
		placeholders[i] = "?"
		args[i+1] = op
	}
	query := `DELETE FROM deferred_operations WHERE job_id = ? AND operation IN (` + strings.Join(placeholders, ",") + `)`
	result, err := db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// HasPendingDeferredOperationForJob checks if there's a pending deferred operation for a job ID
func HasPendingDeferredOperationForJob(db *sql.DB, jobID int64) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM deferred_operations WHERE job_id = ?`,
		jobID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetJobIDsWithPendingOperations returns a map of job IDs that have pending deferred operations
func GetJobIDsWithPendingOperations(db *sql.DB) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT DISTINCT job_id FROM deferred_operations WHERE job_id > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]bool)
	for rows.Next() {
		var jobID int64
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		result[jobID] = true
	}
	return result, rows.Err()
}

// HasPendingOperation checks if a specific operation type already exists for a job
func HasPendingOperation(db *sql.DB, jobID int64, operation string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM deferred_operations WHERE job_id = ? AND operation = ?`,
		jobID, operation,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// DeletePendingOperation deletes a deferred operation by job ID and operation type
func DeletePendingOperation(db *sql.DB, jobID int64, operation string) error {
	_, err := db.Exec(
		`DELETE FROM deferred_operations WHERE job_id = ? AND operation = ?`,
		jobID, operation,
	)
	return err
}

// UpdatePendingOperationPayload updates the payload of an existing deferred operation
func UpdatePendingOperationPayload(db *sql.DB, jobID int64, operation string, payload string) error {
	_, err := db.Exec(
		`UPDATE deferred_operations SET payload = ? WHERE job_id = ? AND operation = ?`,
		payload, jobID, operation,
	)
	return err
}

// DeleteDuplicateDeferredOperations removes duplicate deferred operations, keeping only the oldest
func DeleteDuplicateDeferredOperations(db *sql.DB) (int64, error) {
	result, err := db.Exec(`
		DELETE FROM deferred_operations
		WHERE id NOT IN (
			SELECT MIN(id) FROM deferred_operations
			GROUP BY job_id, operation
		)
	`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// GetDeferredOperationPayload returns the payload for a job's deferred operation
func GetDeferredOperationPayload(db *sql.DB, jobID int64, operation string) (string, error) {
	var payload sql.NullString
	err := db.QueryRow(
		`SELECT payload FROM deferred_operations WHERE job_id = ? AND operation = ? LIMIT 1`,
		jobID, operation,
	).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return payload.String, nil
}

// GetJobDependencyInfo returns dependency info for jobs with pending operations
// Returns a map of jobID -> dep_spec (e.g., "930" or "930+" for after-any)
func GetJobDependencyInfo(db *sql.DB) (map[int64]string, error) {
	rows, err := db.Query(`
		SELECT job_id, payload FROM deferred_operations
		WHERE job_id > 0 AND operation = 'queue_job' AND payload != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]string)
	for rows.Next() {
		var jobID int64
		var payload string
		if err := rows.Scan(&jobID, &payload); err != nil {
			return nil, err
		}
		// Extract dep_spec from JSON payload
		// Simple extraction without full JSON parsing
		if depStart := strings.Index(payload, `"dep_spec":"`); depStart != -1 {
			depStart += len(`"dep_spec":"`)
			if depEnd := strings.Index(payload[depStart:], `"`); depEnd != -1 {
				depSpec := payload[depStart : depStart+depEnd]
				if depSpec != "" {
					result[jobID] = depSpec
				}
			}
		}
	}
	return result, rows.Err()
}
