package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// dbExecer is the common interface for *sql.DB and *sql.Tx.
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// createJobAttemptsTableSQL returns the DDL for the job_attempts table.
func createJobAttemptsTableSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS job_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL REFERENCES jobs(id),
		attempt_number INTEGER NOT NULL,

		-- Placement
		host TEXT NOT NULL DEFAULT '',
		cloud_instance_id INTEGER REFERENCES cloud_instances(id),

		-- Execution state
		status TEXT NOT NULL DEFAULT 'queued',
		queued_at INTEGER,
		start_time INTEGER,
		end_time INTEGER,
		exit_code INTEGER,
		error_message TEXT,
		failure_reason TEXT,
		error_diagnosis TEXT,

		-- Backend-specific
		session_name TEXT,
		remote_id TEXT,
		remote_state TEXT,
		backend TEXT,

		-- Three-way merge for sync reconciliation
		last_synced_status TEXT,
		pending_status TEXT,
		pending_at INTEGER,

		-- Telemetry and cost
		cost REAL,
		vastai_instance_id INTEGER,
		placement_meta TEXT,
		job_metadata TEXT,
		observed_inputs TEXT,

		-- Cloud instance outcome (replaces job_cloud_attempts table)
		cloud_outcome TEXT,

		UNIQUE(job_id, attempt_number),
		CONSTRAINT job_attempts_status_check CHECK (%s)
	)`, statusCheckConstraintSQL("status", jobStatusValues(), false))
}

// initJobAttemptsSchema creates the job_attempts table and indexes.
func initJobAttemptsSchema(db *sql.DB) error {
	if _, err := db.Exec(createJobAttemptsTableSQL()); err != nil {
		return fmt.Errorf("create job_attempts table: %w", err)
	}
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_job ON job_attempts(job_id, attempt_number DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_status ON job_attempts(status)`,
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_cloud_instance ON job_attempts(cloud_instance_id)`,
	}
	for _, idx := range indexes {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("create job_attempts index: %w", err)
		}
	}
	// Migration: add cloud_outcome column
	if err := addColumnIfMissing(db, `ALTER TABLE job_attempts ADD COLUMN cloud_outcome TEXT`); err != nil {
		return err
	}
	return nil
}

// backfillJobAttempts populates the job_attempts table from existing jobs and
// job_runs data. This migration runs once; subsequent calls are no-ops because
// the INSERT uses a NOT EXISTS guard.
func backfillJobAttempts(db *sql.DB) error {
	// Step 1: Backfill historical attempts from job_runs (older archived attempts).
	// Give each archived run an attempt_number based on archived_at ordering.
	if _, err := db.Exec(`
		INSERT INTO job_attempts (
			job_id, attempt_number, host, cloud_instance_id, status, queued_at,
			start_time, end_time, exit_code, error_message, failure_reason,
			error_diagnosis, session_name, remote_id, remote_state, backend,
			cost, vastai_instance_id, placement_meta, job_metadata, observed_inputs
		)
		SELECT
			jr.job_id,
			jr.rn,
			jr.host,
			jr.cloud_instance_id,
			jr.status,
			NULL,
			jr.start_time,
			jr.end_time,
			jr.exit_code,
			jr.error_message,
			jr.failure_reason,
			jr.error_diagnosis,
			jr.session_name,
			jr.remote_id,
			jr.remote_state,
			jr.backend,
			jr.cost,
			jr.vastai_instance_id,
			jr.placement_meta,
			jr.job_metadata,
			NULL
		FROM (
			SELECT jr2.*,
			       ROW_NUMBER() OVER (PARTITION BY jr2.job_id ORDER BY jr2.archived_at ASC) AS rn
			FROM job_runs jr2
		) jr
		WHERE NOT EXISTS (
			SELECT 1 FROM job_attempts ja WHERE ja.job_id = jr.job_id
		)
	`); err != nil {
		return fmt.Errorf("backfill job_attempts from job_runs: %w", err)
	}

	// Step 2: Backfill the current attempt from the jobs table itself.
	// Use attempt_number = MAX(existing) + 1, or 1 if no prior attempts.
	if _, err := db.Exec(`
		INSERT INTO job_attempts (
			job_id, attempt_number, host, cloud_instance_id, status, queued_at,
			start_time, end_time, exit_code, error_message, failure_reason,
			error_diagnosis, session_name, remote_id, remote_state, backend,
			last_synced_status, pending_status, pending_at,
			cost, vastai_instance_id, placement_meta, job_metadata, observed_inputs
		)
		SELECT
			j.id,
			COALESCE((SELECT MAX(ja.attempt_number) FROM job_attempts ja WHERE ja.job_id = j.id), 0) + 1,
			j.host,
			j.cloud_instance_id,
			j.status,
			j.queued_at,
			j.start_time,
			j.end_time,
			j.exit_code,
			j.error_message,
			j.failure_reason,
			j.error_diagnosis,
			j.session_name,
			j.remote_id,
			j.remote_state,
			j.backend,
			j.last_synced_status,
			j.pending_status,
			j.pending_at,
			j.cost,
			j.vastai_instance_id,
			j.placement_meta,
			j.job_metadata,
			j.observed_inputs
		FROM jobs j
		WHERE NOT EXISTS (
			SELECT 1 FROM job_attempts ja WHERE ja.job_id = j.id
		)
		AND (j.status != 'draft'
		  OR j.start_time IS NOT NULL
		  OR j.host != '')
	`); err != nil {
		return fmt.Errorf("backfill job_attempts from jobs: %w", err)
	}

	return nil
}

// cleanupStaleAttempts fixes data corruption from earlier bugs:
//  1. Jobs with multiple open attempts — keeps only the latest, closes the rest.
//  2. Jobs with cloud_instance_id on their latest attempt pointing to a terminated
//     instance — closes that attempt and creates a fresh unplaced one.
func cleanupStaleAttempts(db *sql.DB) error {
	now := time.Now().Unix()

	// Step 1: Close duplicate open attempts. For each job with multiple open
	// attempts, keep only the one with the highest attempt_number.
	if _, err := db.Exec(`
		UPDATE job_attempts SET end_time = ?, status = 'canceled'
		WHERE end_time IS NULL
		  AND id NOT IN (
			SELECT MAX(id) FROM job_attempts
			WHERE end_time IS NULL
			GROUP BY job_id
		  )`, now); err != nil {
		return fmt.Errorf("close duplicate open attempts: %w", err)
	}

	// Step 2: For jobs whose latest open attempt references a terminated cloud
	// instance, close the attempt and create a fresh unplaced one.
	rows, err := db.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		JOIN cloud_instances ci ON ci.id = ja.cloud_instance_id
		WHERE ja.end_time IS NULL
		  AND ci.status IN ('failed', 'completed', 'canceled')`)
	if err != nil {
		return fmt.Errorf("find orphaned cloud attempts: %w", err)
	}
	var orphaned []struct{ jobID, attemptID int64 }
	for rows.Next() {
		var jobID, attemptID int64
		if err := rows.Scan(&jobID, &attemptID); err != nil {
			rows.Close()
			return err
		}
		orphaned = append(orphaned, struct{ jobID, attemptID int64 }{jobID, attemptID})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, o := range orphaned {
		if _, err := db.Exec(`
			UPDATE job_attempts SET status = 'canceled', end_time = ?
			WHERE id = ?`, now, o.attemptID); err != nil {
			return fmt.Errorf("close orphaned attempt %d: %w", o.attemptID, err)
		}
		if _, err := createAttemptTx(db, o.jobID, "", nil, StatusQueued); err != nil {
			return fmt.Errorf("create fresh attempt for job %d: %w", o.jobID, err)
		}
	}

	return nil
}

// isNoSuchTable checks if an error is a "no such table" SQLite error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "no such table: ")
}

// createJobStatusView creates the job_status view that joins jobs with their
// latest attempt to provide backward-compatible columns for the Job struct.
func createJobStatusView(db *sql.DB) error {
	if _, err := db.Exec(`DROP VIEW IF EXISTS job_status`); err != nil {
		return err
	}

	// The view must produce exactly the columns in jobSelectColumns so that
	// the existing scanJob function works unchanged.
	_, err := db.Exec(`
		CREATE VIEW job_status AS
		WITH latest_attempt AS (
			SELECT ja.*,
			       ROW_NUMBER() OVER (PARTITION BY ja.job_id
			                          ORDER BY ja.attempt_number DESC) AS rn
			FROM job_attempts ja
		)
		SELECT
			j.id,
			COALESCE(la.host, '') AS host,
			la.session_name,
			j.working_dir,
			j.command,
			j.description,
			j.generated_description,
			j.generation_hash,
			j.created_at,
			la.queued_at,
			la.start_time,
			la.end_time,
			la.exit_code,
			-- Raw status from attempt (preserves pending vs actual distinction)
			CASE
				WHEN j.requested_status = 'canceled' THEN 'canceled'
				WHEN j.requested_status = 'queued'
				     AND la.status IN ('completed','failed','dead','killed','canceled')
				     THEN 'queued'
				WHEN la.id IS NULL THEN
					CASE WHEN j.requested_status IS NOT NULL THEN j.requested_status
					     ELSE 'draft'
					END
				ELSE la.status
			END AS status,
			la.error_message,
			COALESCE(la.backend, j.backend) AS backend,
			la.remote_id,
			la.remote_state,
			la.failure_reason,
			j.queue_name,
			j.gpu,
			j.gpu_class,
			j.cpu_allotment,
			j.gpu_mem_gb,
			j.env_vars,
			j.tags,
			j.dep_spec,
			j.inputs,
			la.observed_inputs,
			j.outputs,
			j.output_dirs,
			j.produces,
			j.needs,
			j.project,
			j.tombstoned,
			CASE WHEN la.id IS NOT NULL THEN la.last_synced_status ELSE j.last_synced_status END AS last_synced_status,
			CASE WHEN la.id IS NOT NULL THEN la.pending_status ELSE j.pending_status END AS pending_status,
			CASE WHEN la.id IS NOT NULL THEN la.pending_at ELSE j.pending_at END AS pending_at,
			la.job_metadata,
			la.cost,
			la.vastai_instance_id,
			la.error_diagnosis,
			-- retry_count = number of prior attempts (attempt_count - 1), or 0
			COALESCE(la.attempt_number - 1, 0) AS retry_count,
			la.placement_meta,
			j.placement_reasons,
			la.cloud_instance_id,
			j.campaign_job_index,
			la.id AS latest_run_id,
			-- Target kind for placement queries
			CASE
				WHEN la.cloud_instance_id IS NOT NULL THEN 'rental_instance'
				WHEN COALESCE(la.host, '') != ''
				     AND COALESCE(la.host, '') NOT LIKE 'vastai:%%'
				     AND COALESCE(la.host, '') NOT LIKE 'runpod:%%'
				THEN 'inventory_host'
				WHEN COALESCE(la.host, '') LIKE 'vastai:%%' OR COALESCE(la.host, '') LIKE 'runpod:%%'
				THEN 'rental_instance'
				ELSE 'unplaced'
			END AS effective_target_kind
		FROM jobs j
		LEFT JOIN latest_attempt la ON la.job_id = j.id AND la.rn = 1
	`)
	return err
}

// createJobsToAttemptsSync creates a trigger that mirrors execution-state
// columns from jobs to job_attempts on every UPDATE. This ensures backward
// compatibility: code and tests that update jobs directly automatically
// propagate to attempts, which is where the job_status view reads from.
// createJobsToAttemptsSyncTrigger drops the legacy sync trigger. Execution-state
// writes now go directly to job_attempts; no trigger needed.
func createJobsToAttemptsSyncTrigger(db *sql.DB) error {
	_, err := db.Exec(`DROP TRIGGER IF EXISTS jobs_sync_exec_to_attempts`)
	return err
}

// createJobsInsertToAttemptsTrigger creates a trigger that auto-creates an
// attempt row when a new job is inserted (if the job isn't a draft).
func createJobsInsertToAttemptsTrigger(db *sql.DB) error {
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS jobs_insert_create_attempt`); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE TRIGGER jobs_insert_create_attempt
		AFTER INSERT ON jobs
		FOR EACH ROW
		WHEN NEW.status != 'draft'
		BEGIN
			INSERT INTO job_attempts (
				job_id, attempt_number, host, cloud_instance_id, status, queued_at,
				start_time, end_time, exit_code, error_message, failure_reason,
				error_diagnosis, session_name, remote_id, remote_state, backend,
				last_synced_status, pending_status, pending_at, cost,
				vastai_instance_id, placement_meta, job_metadata, observed_inputs
			) VALUES (
				NEW.id,
				COALESCE((SELECT MAX(attempt_number) FROM job_attempts WHERE job_id = NEW.id), 0) + 1,
				NEW.host, NEW.cloud_instance_id, NEW.status, NEW.queued_at,
				NEW.start_time, NEW.end_time, NEW.exit_code, NEW.error_message, NEW.failure_reason,
				NEW.error_diagnosis, NEW.session_name, NEW.remote_id, NEW.remote_state, NEW.backend,
				NEW.last_synced_status, NEW.pending_status, NEW.pending_at, NEW.cost,
				NEW.vastai_instance_id, NEW.placement_meta, NEW.job_metadata, NEW.observed_inputs
			);
		END
	`)
	return err
}

// createTrainingExamplesView creates the job_run_training_examples view using
// job_attempts joined with jobs for spec columns.
func createTrainingExamplesView(db *sql.DB) error {
	if _, err := db.Exec(`DROP VIEW IF EXISTS job_run_training_examples`); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE VIEW job_run_training_examples AS
		SELECT
			ja.id AS run_id,
			ja.job_id,
			ja.end_time AS archived_at,
			'' AS archive_reason,
			ja.status,
			ja.host,
			j.working_dir,
			j.command,
			j.description,
			j.gpu,
			j.gpu_class,
			j.cpu_allotment,
			j.gpu_mem_gb,
			j.env_vars,
			j.tags,
			j.dep_spec,
			j.inputs,
			j.outputs,
			j.output_dirs,
			j.produces,
			j.needs,
			j.project,
			COALESCE(ja.backend, j.backend, 'queue-runner') AS backend,
			CASE WHEN COALESCE(ja.backend, j.backend, 'queue-runner') = 'vastai' THEN 'single' ELSE 'multi' END AS tenant,
			ja.start_time,
			ja.end_time,
			CASE
				WHEN ja.start_time IS NOT NULL AND ja.end_time IS NOT NULL THEN ja.end_time - ja.start_time
				ELSE NULL
			END AS duration_s,
			ja.exit_code,
			ja.job_metadata,
			ja.placement_meta,
			ja.cost,
			ja.vastai_instance_id,
			ja.attempt_number - 1 AS retry_count,
			ja.cloud_instance_id,
			ja.error_message,
			ja.failure_reason,
			ja.error_diagnosis
		FROM job_attempts ja
		JOIN jobs j ON j.id = ja.job_id
		WHERE ja.start_time IS NOT NULL AND ja.end_time IS NOT NULL
	`)
	return err
}

// --- Attempt helper functions ---
//
// SQLite doesn't support UPDATE ... ORDER BY ... LIMIT without SQLITE_ENABLE_UPDATE_DELETE_LIMIT.
// All UPDATE helpers use a subquery: WHERE id = (SELECT id ... ORDER BY ... LIMIT 1).

// latestOpenAttemptSubquery returns a SQL subquery that selects the id of the
// latest open (end_time IS NULL) attempt for the given job_id placeholder.
const latestOpenAttemptSubquery = `(SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL ORDER BY attempt_number DESC LIMIT 1)`

// latestAttemptSubquery returns a SQL subquery for the latest attempt (open or closed).
const latestAttemptSubquery = `(SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`

// closeOpenAttempts closes all open attempts for a job.
func closeOpenAttempts(execer dbExecer, jobID int64, now int64) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET end_time = COALESCE(end_time, ?), pending_status = NULL
		WHERE job_id = ? AND end_time IS NULL`,
		now, jobID,
	)
	return err
}

// CreateAttempt inserts a new attempt for a job and returns its ID.
func CreateAttempt(db *sql.DB, jobID int64, host string, cloudInstanceID *int64, status string) (int64, error) {
	return createAttemptTx(db, jobID, host, cloudInstanceID, status)
}

func createAttemptTx(execer dbExecer, jobID int64, host string, cloudInstanceID *int64, status string) (int64, error) {
	now := time.Now().Unix()
	result, err := execer.Exec(`
		INSERT INTO job_attempts (job_id, attempt_number, host, cloud_instance_id, status, queued_at)
		VALUES (?,
			COALESCE((SELECT MAX(attempt_number) FROM job_attempts WHERE job_id = ?), 0) + 1,
			?, ?, ?, ?)`,
		jobID, jobID, host, cloudInstanceID, status, now,
	)
	if err != nil {
		return 0, fmt.Errorf("create attempt for job %d: %w", jobID, err)
	}
	return result.LastInsertId()
}

// CloseAttempt marks the current open attempt for a job as ended.
func CloseAttempt(db *sql.DB, jobID int64, status string, exitCode *int, endTime int64) error {
	_, err := db.Exec(`
		UPDATE job_attempts
		SET status = ?, exit_code = ?, end_time = ?, pending_status = NULL, session_name = NULL
		WHERE id = `+latestOpenAttemptSubquery,
		status, exitCode, endTime, jobID,
	)
	return err
}

// GetLatestAttemptID returns the ID of the latest attempt for a job, or 0 if none.
func GetLatestAttemptID(db *sql.DB, jobID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`
		SELECT id FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// UpdateAttemptRunning marks the latest open attempt as running with a start time.
func UpdateAttemptRunning(execer dbExecer, jobID int64) error {
	now := time.Now().Unix()
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?, start_time = COALESCE(start_time, ?)
		WHERE id = `+latestOpenAttemptSubquery,
		StatusRunning, StatusRunning, now, jobID,
	)
	return err
}

// UpdateAttemptCompletion marks the latest attempt as completed.
// Uses latestAttemptSubquery (not just open attempts) because completion is
// authoritative — it can override a previous "failed" status from a race
// condition (e.g., job was marked dead locally but status file shows completion).
func UpdateAttemptCompletion(execer dbExecer, jobID int64, exitCode int, endTime int64) error {
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, exit_code = ?, end_time = ?,
		    last_synced_status = ?, pending_status = NULL, session_name = NULL
		WHERE id = `+latestAttemptSubquery,
		StatusCompleted, exitCode, endTime, StatusCompleted, jobID,
	)
	return err
}

// UpdateAttemptDead marks the latest open attempt as failed (unexpected termination).
func UpdateAttemptDead(execer dbExecer, jobID int64) error {
	endTime := time.Now().Unix()
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, end_time = ?,
		    last_synced_status = ?, pending_status = NULL, session_name = NULL
		WHERE id = `+latestOpenAttemptSubquery,
		StatusFailed, endTime, StatusFailed, jobID,
	)
	return err
}

// SetAttemptPendingStatus sets pending_status on the latest attempt (open or closed).
// Pending status can be set on a terminal attempt (e.g., retry intent on a failed job).
func SetAttemptPendingStatus(db *sql.DB, jobID int64, status string) error {
	now := time.Now().Unix()
	_, err := db.Exec(`
		UPDATE job_attempts SET pending_status = ?, pending_at = ?
		WHERE id = `+latestAttemptSubquery,
		status, now, jobID,
	)
	return err
}

// ClearAttemptPendingStatus clears pending_status on the latest attempt (open or closed).
func ClearAttemptPendingStatus(db *sql.DB, jobID int64) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET pending_status = NULL, pending_at = NULL
		WHERE id = `+latestAttemptSubquery,
		jobID,
	)
	return err
}

// SetAttemptCloudInstanceID updates the cloud_instance_id on the latest open attempt.
func SetAttemptCloudInstanceID(execer dbExecer, jobID, instanceID int64) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET cloud_instance_id = ?
		WHERE id = `+latestOpenAttemptSubquery,
		instanceID, jobID,
	)
	return err
}

// SetAttemptVastaiInstance stores the Vast.ai instance ID on the latest open attempt.
func SetAttemptVastaiInstance(db *sql.DB, jobID int64, instanceID int) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET vastai_instance_id = ?, backend = ?
		WHERE id = `+latestOpenAttemptSubquery,
		instanceID, BackendVastai, jobID,
	)
	return err
}

// SetAttemptCost updates the cost on the latest open attempt.
func SetAttemptCost(db *sql.DB, jobID int64, cost float64) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET cost = ?
		WHERE id = `+latestOpenAttemptSubquery,
		cost, jobID,
	)
	return err
}

// SetAttemptPlacementMeta stores placement telemetry on the latest open attempt.
func SetAttemptPlacementMeta(db *sql.DB, jobID int64, meta *PlacementMeta) error {
	encoded, err := encodePlacementMeta(meta)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		UPDATE job_attempts SET placement_meta = ?
		WHERE id = `+latestOpenAttemptSubquery,
		encoded, jobID,
	)
	return err
}

// SetAttemptSessionName updates the session_name on the latest open attempt.
func SetAttemptSessionName(execer dbExecer, jobID int64, sessionName string) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET session_name = ?
		WHERE id = `+latestOpenAttemptSubquery,
		sessionName, jobID,
	)
	return err
}

// SetAttemptErrorDiagnosis updates error diagnosis on the latest attempt (open or not).
func SetAttemptErrorDiagnosis(db *sql.DB, jobID int64, diagnosis string) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET error_diagnosis = ?
		WHERE id = `+latestAttemptSubquery,
		diagnosis, jobID,
	)
	return err
}

// UpdateAttemptLastSyncedStatus updates last_synced_status on the latest open attempt.
func UpdateAttemptLastSyncedStatus(db *sql.DB, jobID int64, status string) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET last_synced_status = ?
		WHERE id = `+latestOpenAttemptSubquery,
		status, jobID,
	)
	return err
}

// UpdateAttemptStatusAndLastSynced updates both status and last_synced_status
// on the latest open attempt.
func UpdateAttemptStatusAndLastSynced(execer dbExecer, jobID int64, status string) error {
	_, err := execer.Exec(`
		UPDATE job_attempts SET status = ?, last_synced_status = ?
		WHERE id = `+latestOpenAttemptSubquery,
		status, status, jobID,
	)
	return err
}

// ClearAttemptPendingAndUpdateStatus reconciles pending_status by clearing it
// and updating status + last_synced_status on the latest open attempt.
func ClearAttemptPendingAndUpdateStatus(db *sql.DB, jobID int64, status string) error {
	// Use latestAttemptSubquery because this can reset a closed (terminal)
	// attempt back to queued/draft status.
	if status == StatusQueued || status == StatusDraft {
		// When going back to queued/draft, clear all execution fields
		_, err := db.Exec(`
			UPDATE job_attempts
			SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL,
			    start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL,
			    session_name = NULL, failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
			WHERE id = `+latestAttemptSubquery,
			status, status, jobID,
		)
		return err
	}
	if IsTerminalStatus(status) {
		// For terminal states, set end_time if missing and clear session
		now := time.Now().Unix()
		_, err := db.Exec(`
			UPDATE job_attempts
			SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL,
			    session_name = NULL,
			    end_time = CASE WHEN end_time IS NULL OR end_time = 0 THEN ? ELSE end_time END
			WHERE id = `+latestAttemptSubquery,
			status, status, now, jobID,
		)
		return err
	}
	_, err := db.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL
		WHERE id = `+latestAttemptSubquery,
		status, status, jobID,
	)
	return err
}

// MarkAttemptQueuedByID resets the latest open attempt back to queued status
// (e.g., when sync finds the job is still in queue after it was thought to be running).
func MarkAttemptQueuedByID(execer dbExecer, jobID int64) error {
	// Use latestAttemptSubquery (not just open) because the attempt may have
	// end_time set from a previous status. This resets all execution fields.
	_, err := execer.Exec(`
		UPDATE job_attempts
		SET status = ?, last_synced_status = ?,
		    start_time = NULL, end_time = NULL, exit_code = NULL,
		    error_message = NULL, session_name = NULL, failure_reason = NULL,
		    error_diagnosis = NULL, remote_state = NULL
		WHERE id = `+latestAttemptSubquery,
		StatusQueued, StatusQueued, jobID,
	)
	return err
}
