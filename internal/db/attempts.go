package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func sqlNormalizeGPUClassExpr(expr string) string {
	cleaned := fmt.Sprintf("lower(trim(coalesce(%s, '')))", expr)
	for _, token := range []string{
		"nvidia", "geforce", "tesla", "amd", "radeon", "instinct",
		" ", "-", "_", ".", "/", "(", ")", "[", "]",
	} {
		cleaned = fmt.Sprintf("replace(%s, '%s', '')", cleaned, token)
	}
	return fmt.Sprintf(`
		CASE
			WHEN NULLIF(%[1]s, '') IS NULL THEN NULL
			WHEN %[1]s LIKE '%%b200%%' THEN 'b200'
			WHEN %[1]s LIKE '%%h200%%' THEN 'h200'
			WHEN %[1]s LIKE '%%h100%%' THEN 'h100'
			WHEN %[1]s LIKE '%%a100%%' THEN 'a100'
			WHEN %[1]s LIKE '%%l40s%%' THEN 'l40s'
			WHEN %[1]s LIKE '%%l40%%' THEN 'l40'
			WHEN %[1]s LIKE '%%a40%%' THEN 'a40'
			WHEN %[1]s LIKE '%%a10g%%' THEN 'a10g'
			WHEN %[1]s LIKE '%%a10%%' THEN 'a10'
			WHEN %[1]s LIKE '%%m2max%%' THEN 'm2max'
			WHEN instr(%[1]s, 'rtx') > 0 THEN substr(%[1]s, instr(%[1]s, 'rtx'))
			WHEN %[1]s GLOB '[0-9][0-9][0-9][0-9]' OR %[1]s GLOB '[0-9][0-9][0-9][0-9]ti' THEN 'rtx' || %[1]s
			ELSE %[1]s
		END`, cleaned)
}

func sqlParseMemoryMiBExpr(expr string) string {
	cleaned := fmt.Sprintf("lower(replace(trim(coalesce(%s, '')), ' ', ''))", expr)
	return fmt.Sprintf(`
		CASE
			WHEN NULLIF(%[1]s, '') IS NULL THEN NULL
			WHEN %[1]s GLOB '*gib' THEN CAST(replace(%[1]s, 'gib', '') AS INTEGER) * 1024
			WHEN %[1]s GLOB '*gb' THEN CAST(replace(%[1]s, 'gb', '') AS INTEGER) * 1024
			WHEN %[1]s GLOB '*mib' THEN CAST(replace(%[1]s, 'mib', '') AS INTEGER)
			WHEN %[1]s GLOB '*mb' THEN CAST(replace(%[1]s, 'mb', '') AS INTEGER)
			ELSE CAST(%[1]s AS INTEGER)
		END`, cleaned)
}

// dbExecer is the common interface for *sql.DB and *sql.Tx.
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// closeAttemptsAndRequeue closes open attempts for a cloud job and sets
// requested_status='queued' so the view derives "queued" without a hostless
// attempt. Used by ResetLaunchJobs, RequeueByID, ResetJobToUnplaced, and
// cleanupStaleAttempts.
func closeAttemptsAndRequeue(db dbExecer, jobID int64, now int64) error {
	if _, err := db.Exec(
		`UPDATE job_attempts SET status = ?, end_time = COALESCE(end_time, ?) WHERE job_id = ? AND end_time IS NULL`,
		StatusCanceled, now, jobID); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusQueued, jobID)
	return err
}

// createJobAttemptsTableSQL returns the DDL for the job_attempts table.
func createJobAttemptsTableSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS job_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL REFERENCES jobs(id),
		attempt_number INTEGER NOT NULL,

		-- Placement
		host TEXT NOT NULL DEFAULT '',
		launch_id INTEGER REFERENCES launches(id),

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
		placement_meta TEXT,
		job_metadata TEXT,
		observed_inputs TEXT,

		-- Cloud instance outcome (replaces job_cloud_attempts table)
		cloud_outcome TEXT,

		UNIQUE(job_id, attempt_number),
		CONSTRAINT job_attempts_status_check CHECK (%s),
		CONSTRAINT job_attempts_cloud_outcome_check CHECK (
			cloud_outcome IS NULL OR cloud_outcome IN ('completed','failed','canceled','orphaned','superseded')
		)
	)`, statusCheckConstraintSQL("status", jobStatusValues(), false))
}

// initJobAttemptsSchema creates the job_attempts table and indexes.
func initJobAttemptsSchema(db *sql.DB) error {
	if _, err := db.Exec(createJobAttemptsTableSQL()); err != nil {
		return fmt.Errorf("create job_attempts table: %w", err)
	}
	// Migrations: column renames must run before index creation.
	if err := addColumnIfMissing(db, `ALTER TABLE job_attempts ADD COLUMN cloud_outcome TEXT`); err != nil {
		return err
	}
	// Drop views and triggers that reference old column names before renaming.
	// These are recreated later in initSchema.
	for _, v := range []string{"launch_job_membership", "job_status", "job_run_training_examples", "training_examples", "job_effective_state", "all_runs"} {
		db.Exec(`DROP VIEW IF EXISTS ` + v)
	}
	for _, t := range []string{
		"artifacts_validate_job_run_ownership_on_insert", "artifacts_validate_job_run_ownership_on_update",
		"job_timeseries_validate_job_run_ownership_on_insert", "job_timeseries_validate_job_run_ownership_on_update",
		"jobs_insert_create_attempt",
	} {
		db.Exec(`DROP TRIGGER IF EXISTS ` + t)
	}
	renameColumnIfExists(db, "job_attempts", "cloud_instance_id", "launch_id")
	// Drop old index that references the renamed column.
	db.Exec(`DROP INDEX IF EXISTS idx_job_attempts_cloud_instance`)

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_job ON job_attempts(job_id, attempt_number DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_status ON job_attempts(status)`,
		`CREATE INDEX IF NOT EXISTS idx_job_attempts_launch ON job_attempts(launch_id)`,
	}
	for _, idx := range indexes {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("create job_attempts index: %w", err)
		}
	}

	// Rebuild if the table has stale columns (vastai_instance_id) or missing
	// constraints (cloud_outcome CHECK). The DDL defines the canonical schema.
	hasStaleCol, _ := tableSchemaContains(db, "job_attempts", "vastai_instance_id")
	hasOutcomeCheck, _ := tableSchemaContains(db, "job_attempts", "job_attempts_cloud_outcome_check")
	if hasStaleCol || !hasOutcomeCheck {
		// Drop views/triggers that reference job_attempts before rebuild.
		for _, v := range []string{"launch_job_membership", "cloud_instance_job_membership", "job_status", "training_examples"} {
			db.Exec(`DROP VIEW IF EXISTS ` + v)
		}
		for _, t := range []string{"jobs_insert_create_attempt"} {
			db.Exec(`DROP TRIGGER IF EXISTS ` + t)
		}
		const cols = `id, job_id, attempt_number, host, launch_id, status, queued_at, start_time, end_time, exit_code, error_message, failure_reason, error_diagnosis, session_name, remote_id, remote_state, backend, last_synced_status, pending_status, pending_at, cost, placement_meta, job_metadata, observed_inputs, cloud_outcome`
		if err := rebuildTable(db,
			strings.Replace(createJobAttemptsTableSQL(), "job_attempts", "job_attempts_new", 1),
			fmt.Sprintf(`INSERT INTO job_attempts_new (%s) SELECT %s FROM job_attempts`, cols, cols),
			`DROP TABLE job_attempts`,
			`ALTER TABLE job_attempts_new RENAME TO job_attempts`,
			`CREATE INDEX IF NOT EXISTS idx_job_attempts_job ON job_attempts(job_id, attempt_number DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_job_attempts_status ON job_attempts(status)`,
			`CREATE INDEX IF NOT EXISTS idx_job_attempts_launch ON job_attempts(launch_id)`,
		); err != nil {
			return fmt.Errorf("rebuild job_attempts: %w", err)
		}
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
			job_id, attempt_number, host, launch_id, status, queued_at,
			start_time, end_time, exit_code, error_message, failure_reason,
			error_diagnosis, session_name, remote_id, remote_state, backend,
			cost, placement_meta, job_metadata, observed_inputs
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
	// Only runs on old schemas that still have execution-state columns on jobs.
	// New schemas store execution state exclusively on job_attempts.
	hasStatusColumn, err := tableSchemaContains(db, "jobs", "status TEXT NOT NULL")
	if err != nil {
		return fmt.Errorf("check jobs schema: %w", err)
	}
	if hasStatusColumn {
		if _, err := db.Exec(`
			INSERT INTO job_attempts (
				job_id, attempt_number, host, launch_id, status, queued_at,
				start_time, end_time, exit_code, error_message, failure_reason,
				error_diagnosis, session_name, remote_id, remote_state, backend,
				last_synced_status, pending_status, pending_at,
				cost, placement_meta, job_metadata, observed_inputs
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

	// Step 2: For jobs whose latest open attempt references a failed or
	// canceled cloud instance, close the attempt and create a fresh unplaced
	// one. Completed instances are intentionally skipped — their jobs should
	// be finalized by the R2 result sync, not reset to queued.
	rows, err := db.Query(`
		SELECT ja.job_id, ja.id
		FROM job_attempts ja
		JOIN launches ci ON ci.id = ja.launch_id
		WHERE ja.end_time IS NULL
		  AND ci.status IN (?, ?)
		  AND NOT EXISTS (
			SELECT 1 FROM job_attempts ja2
			WHERE ja2.job_id = ja.job_id
			  AND ja2.id != ja.id
			  AND ja2.status IN (?, ?, ?, ?)
		  )`,
		LaunchStatusFailed, LaunchStatusCancelled,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled,
	)
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
		if err := closeAttemptsAndRequeue(db, o.jobID, now); err != nil {
			return fmt.Errorf("requeue orphaned job %d: %w", o.jobID, err)
		}
	}

	return nil
}

// repairOrphanedCompletedAttempts fixes completed attempts that lost their
// launch association. This happens when cleanupStaleAttempts creates a blank
// replacement attempt (no host, no launch_id) and R2 sync later completes it
// without propagating the launch_id.
func repairOrphanedCompletedAttempts(database *sql.DB) error {
	rows, err := database.Query(`
		SELECT job_id, id FROM job_attempts
		WHERE status IN (?, ?)
		  AND (host = '' OR host IS NULL)
		  AND launch_id IS NULL
		  AND id = (SELECT MAX(ja3.id) FROM job_attempts ja3 WHERE ja3.job_id = job_attempts.job_id)`,
		StatusCompleted, StatusFailed,
	)
	if err != nil {
		return fmt.Errorf("find orphaned attempts: %w", err)
	}
	type orphan struct{ jobID, attemptID int64 }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.jobID, &o.attemptID); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()

	for _, o := range orphans {
		launchID, err := inferSiblingLaunch(database, o.jobID)
		if err != nil || launchID == 0 {
			continue
		}
		host := LaunchHost(launchID)
		database.Exec(
			`UPDATE job_attempts SET launch_id = ?, host = ? WHERE id = ? AND launch_id IS NULL`,
			launchID, host, o.attemptID,
		)
	}

	return nil
}

// inferSiblingLaunch finds the replacement launch for an orphaned job by
// looking at the job's prior attempt that had a launch_id, then finding a
// sibling launch (same GPU class, completed status) that ran other jobs from
// that same original failed launch. Returns 0 if no match found.
func inferSiblingLaunch(db *sql.DB, jobID int64) (int64, error) {
	var launchID sql.NullInt64
	err := db.QueryRow(inferSiblingLaunchSQL, jobID).Scan(&launchID)
	if err == sql.ErrNoRows || !launchID.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return launchID.Int64, nil
}

// inferSiblingLaunchSQL is used both as a standalone query (via inferSiblingLaunch)
// and as a correlated subquery (in repairOrphanedCompletedAttempts, where ? is
// bound to job_attempts.job_id from the outer UPDATE).
const inferSiblingLaunchSQL = `
	SELECT DISTINCT ja_sibling.launch_id
	FROM job_attempts ja_prior
	JOIN job_attempts ja_sibling ON ja_sibling.launch_id != ja_prior.launch_id
	  AND ja_sibling.status IN ('completed', 'dead')
	  AND ja_sibling.job_id IN (
		SELECT ja_peer.job_id FROM job_attempts ja_peer
		WHERE ja_peer.launch_id = ja_prior.launch_id
	  )
	JOIN launches l_sibling ON l_sibling.id = ja_sibling.launch_id
	  AND l_sibling.status = 'completed'
	JOIN launches l_prior ON l_prior.id = ja_prior.launch_id
	  AND UPPER(l_sibling.gpu_class) = UPPER(l_prior.gpu_class)
	WHERE ja_prior.job_id = ?
	  AND ja_prior.launch_id IS NOT NULL
	ORDER BY ja_sibling.launch_id DESC LIMIT 1`

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
			CASE WHEN j.requested_status = 'queued' AND la.end_time IS NOT NULL
			     THEN '' ELSE COALESCE(la.host, '') END AS host,
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
			CASE
				-- User-level overrides always win
				WHEN j.requested_status = 'canceled' THEN 'canceled'
				WHEN j.requested_status = 'killed' THEN 'killed'
				WHEN j.requested_status = 'draft' THEN 'draft'
				-- No attempt: check if user wants to run or job is new
				WHEN la.id IS NULL THEN
					CASE WHEN j.requested_status = 'queued' THEN 'queued'
					     WHEN j.requested_status IS NOT NULL THEN j.requested_status
					     ELSE 'draft'
					END
				-- Cloud jobs: derive status from attempt facts + instance lifecycle
				WHEN la.launch_id IS NOT NULL THEN
					CASE
						WHEN j.requested_status = 'queued'
						     AND la.end_time IS NOT NULL THEN 'queued'
						WHEN la.end_time IS NOT NULL THEN
							CASE
								WHEN la.exit_code = 0 THEN 'completed'
								WHEN la.exit_code IS NOT NULL THEN 'failed'
								WHEN l.termination_reason = 'job_failure' THEN 'failed'
								WHEN l.status IN ('failed','canceled') THEN 'orphaned'
								ELSE 'dead'
							END
						WHEN la.start_time IS NOT NULL THEN
							CASE
								WHEN l.status IN ('failed','canceled') THEN 'orphaned'
								ELSE 'running'
							END
						-- Placed but not yet started
						WHEN l.status IN ('failed','canceled') THEN 'orphaned'
						ELSE 'queued'
					END
				-- On-prem jobs: existing three-way merge logic
				WHEN j.requested_status = 'queued'
				     AND la.status IN ('completed','failed','dead','killed','canceled')
				     THEN 'queued'
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
			j.gpu_mem_max_gb,
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
			la.last_synced_status,
			la.pending_status,
			la.pending_at,
			la.job_metadata,
			la.cost,
			la.error_diagnosis,
			-- retry_count = number of prior attempts (attempt_count - 1), or 0
			COALESCE(la.attempt_number - 1, 0) AS retry_count,
			la.placement_meta,
			j.placement_reasons,
			CASE WHEN j.requested_status = 'queued' AND la.end_time IS NOT NULL
			     THEN NULL ELSE la.launch_id END AS launch_id,
			j.campaign_job_index,
			la.id AS latest_run_id,
			-- Target kind for placement queries.
			-- Only count a launch as claiming if it is actively progressing;
			-- planned/failed/cancelled launches do not block re-launch.
			CASE
				WHEN j.requested_status = 'queued' AND la.end_time IS NOT NULL
				THEN 'unplaced'
				WHEN la.launch_id IS NOT NULL
				     AND l.status IN ('launching', 'running', 'grace', 'completed')
				THEN 'rental_instance'
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
		LEFT JOIN launches l ON l.id = la.launch_id
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

// dropJobsInsertTrigger removes the legacy trigger that auto-created an
// attempt row on job insert. Attempts are now only created when a job is
// placed on a host/instance.
func dropJobsInsertTrigger(db *sql.DB) error {
	_, err := db.Exec(`DROP TRIGGER IF EXISTS jobs_insert_create_attempt`)
	return err
}

// createTrainingExamplesView creates the training_examples view using
// job_attempts joined with jobs for spec columns.
func createTrainingExamplesView(db *sql.DB) error {
	if _, err := db.Exec(`DROP VIEW IF EXISTS training_examples`); err != nil {
		return err
	}
	if _, err := db.Exec(`DROP VIEW IF EXISTS job_run_training_examples`); err != nil {
		return err
	}

	selectedGPUClassExpr := sqlNormalizeGPUClassExpr(`json_extract(je.value, '$.Name')`)
	launchGPUClassExpr := sqlNormalizeGPUClassExpr(`COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class)`)
	gpuMemExpr := sqlParseMemoryMiBExpr(`json_extract(je.value, '$.MemTotal')`)

	createTrainingExamplesSQL := fmt.Sprintf(`
		CREATE VIEW training_examples AS
		WITH completed_runs AS (
			SELECT
				ja.id AS run_id,
				ja.job_id,
				ja.attempt_number,
				ja.status,
				ja.host,
				ja.start_time,
				ja.end_time,
				ja.exit_code,
				ja.job_metadata,
				ja.placement_meta,
				ja.cost,
				ja.launch_id,
				ja.error_message,
				ja.failure_reason,
				ja.error_diagnosis,
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
				hic.cpu_count,
				hic.cpu_model,
				hic.cpu_freq,
				hic.mem_total,
				hic.gpus_json,
				hic.last_updated AS host_hardware_last_updated,
				l.resolved_gpu_name AS launch_resolved_gpu_name,
				l.gpu_class AS launch_gpu_class,
				l.num_gpus AS launch_gpu_count,
				l.gpu_mem_gb AS launch_gpu_mem_gb,
				NULLIF(REPLACE(COALESCE(json_extract(ja.job_metadata, '$.resource.gpu_devices'), ''), ' ', ''), '') AS assigned_gpu_devices_csv,
				CASE
					WHEN json_type(ja.job_metadata, '$.telemetry.assigned_gpu_indices') = 'array'
					THEN json_extract(ja.job_metadata, '$.telemetry.assigned_gpu_indices')
				END AS assigned_gpu_indices_json
			FROM job_attempts ja
			JOIN jobs j ON j.id = ja.job_id
			LEFT JOIN host_info_cache hic ON hic.name = ja.host
			LEFT JOIN launches l ON l.id = ja.launch_id
			WHERE ja.start_time IS NOT NULL AND ja.end_time IS NOT NULL
		),
		selected_gpu_inventory AS (
			SELECT
				cr.run_id,
				json_extract(je.value, '$.Index') AS gpu_index,
				NULLIF(json_extract(je.value, '$.Name'), '') AS gpu_name,
				%s AS gpu_vram_mib,
				%s AS gpu_class
			FROM completed_runs cr
			JOIN json_each(CASE
				WHEN cr.gpus_json IS NOT NULL AND trim(cr.gpus_json) != '' THEN cr.gpus_json
				ELSE '[]'
			END) je
			WHERE
				(cr.assigned_gpu_devices_csv IS NULL AND cr.assigned_gpu_indices_json IS NULL)
				OR (
					cr.assigned_gpu_devices_csv IS NOT NULL
					AND instr(',' || cr.assigned_gpu_devices_csv || ',', ',' || CAST(json_extract(je.value, '$.Index') AS TEXT) || ',') > 0
				)
				OR (
					cr.assigned_gpu_indices_json IS NOT NULL
					AND EXISTS (
						SELECT 1
						FROM json_each(COALESCE(cr.assigned_gpu_indices_json, '[]')) idx
						WHERE CAST(idx.value AS TEXT) = CAST(json_extract(je.value, '$.Index') AS TEXT)
					)
				)
				OR (
					(cr.assigned_gpu_devices_csv IS NOT NULL OR cr.assigned_gpu_indices_json IS NOT NULL)
					AND json_extract(je.value, '$.Index') IS NULL
				)
		),
		selected_gpu_summary AS (
			SELECT
				sgi.run_id,
				COUNT(*) AS selected_gpu_count,
				COUNT(DISTINCT sgi.gpu_name) AS selected_gpu_unique_name_count,
				MIN(sgi.gpu_name) AS first_gpu_name,
				COALESCE(json_group_array(sgi.gpu_name), '[]') AS gpu_names,
				COUNT(DISTINCT sgi.gpu_class) AS selected_gpu_unique_class_count,
				MIN(sgi.gpu_class) AS first_gpu_class,
				CASE
					WHEN COUNT(DISTINCT sgi.gpu_vram_mib) = 1 THEN MIN(sgi.gpu_vram_mib)
				END AS gpu_vram_per_device_mib,
				SUM(COALESCE(sgi.gpu_vram_mib, 0)) AS gpu_vram_total_mib
			FROM selected_gpu_inventory sgi
			GROUP BY sgi.run_id
		)
		SELECT
			base.run_id,
			base.job_id,
			base.end_time AS archived_at,
			'' AS archive_reason,
			base.status,
			base.host,
			base.working_dir,
			base.command,
			base.description,
			base.gpu,
			base.gpu_class,
			base.gpu AS requested_gpu,
			base.gpu_class AS requested_gpu_class,
			base.cpu_allotment,
			base.gpu_mem_gb,
			base.env_vars,
			base.tags,
			base.dep_spec,
			base.inputs,
			base.outputs,
			base.output_dirs,
			base.produces,
			base.needs,
			base.project,
			base.backend,
			CASE WHEN base.backend = 'vastai' THEN 'single' ELSE 'multi' END AS tenant,
			base.start_time,
			base.end_time,
			CASE
				WHEN base.start_time IS NOT NULL AND base.end_time IS NOT NULL THEN base.end_time - base.start_time
				ELSE NULL
			END AS duration_s,
			base.exit_code,
			base.job_metadata,
			base.placement_meta,
			base.cost,
			base.attempt_number - 1 AS retry_count,
			base.launch_id,
			base.error_message,
			base.failure_reason,
			base.error_diagnosis,
			base.cpu_count,
			base.cpu_model,
			base.cpu_freq,
			base.mem_total,
			CASE
				WHEN gpu.selected_gpu_unique_name_count = 1 THEN gpu.first_gpu_name
				WHEN NULLIF(trim(base.launch_resolved_gpu_name), '') IS NOT NULL THEN base.launch_resolved_gpu_name
			END AS actual_gpu_name,
			CASE
				WHEN gpu.gpu_names IS NOT NULL THEN gpu.gpu_names
				WHEN NULLIF(trim(base.launch_resolved_gpu_name), '') IS NOT NULL THEN json_array(base.launch_resolved_gpu_name)
			END AS gpu_names,
			COALESCE(gpu.selected_gpu_count, base.launch_gpu_count) AS gpu_count,
			COALESCE(
				gpu.gpu_vram_per_device_mib,
				CASE WHEN base.launch_gpu_mem_gb IS NOT NULL THEN base.launch_gpu_mem_gb * 1024 END
			) AS gpu_vram_per_device_mib,
			COALESCE(
				gpu.gpu_vram_total_mib,
				CASE
					WHEN base.launch_gpu_mem_gb IS NOT NULL AND base.launch_gpu_count IS NOT NULL
					THEN base.launch_gpu_mem_gb * 1024 * base.launch_gpu_count
				END
			) AS gpu_vram_total_mib,
			CASE
				WHEN gpu.selected_gpu_unique_class_count = 1 THEN gpu.first_gpu_class
				WHEN NULLIF(%s, '') IS NOT NULL THEN %s
			END AS actual_gpu_class,
			base.host_hardware_last_updated,
			CAST(json_extract(base.job_metadata, '$.resource.peak_rss_kb') AS INTEGER) AS peak_rss_kb,
			CAST(json_extract(base.job_metadata, '$.resource.max_gpu_mem_mib') AS INTEGER) AS max_gpu_mem_mib,
			CAST(json_extract(base.job_metadata, '$.cpu.mean') AS REAL) AS cpu_mean
		FROM completed_runs base
		LEFT JOIN selected_gpu_summary gpu ON gpu.run_id = base.run_id
	`, gpuMemExpr, selectedGPUClassExpr, launchGPUClassExpr, launchGPUClassExpr)

	if _, err := db.Exec(createTrainingExamplesSQL); err != nil {
		return err
	}

	_, err := db.Exec(`CREATE VIEW job_run_training_examples AS SELECT * FROM training_examples`)
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
		INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, queued_at)
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

// SetAttemptLaunch associates the latest open attempt for a job with a launch.
// This re-links an orphaned job (whose attempt was reset without a launch_id)
// back to the launch that is actually running it, as observed from R2 phase.
func SetAttemptLaunch(database *sql.DB, jobID int64, launchID int64) error {
	_, err := database.Exec(`
		UPDATE job_attempts
		SET launch_id = ?, host = ?
		WHERE id = `+latestOpenAttemptSubquery,
		launchID, LaunchHost(launchID), jobID,
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

// SetAttemptVastaiInstance sets the backend to 'vastai' on the latest open attempt.
func SetAttemptVastaiInstance(db *sql.DB, jobID int64, _ int) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET backend = ?
		WHERE id = `+latestOpenAttemptSubquery,
		BackendVastai, jobID,
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

// MarkAttemptQueuedByID closes any open attempt and creates a fresh queued
// attempt, preserving the historical record of the previous attempt.
// Carries over host, pending_status, and pending_at from the old attempt so
// that placement and user intent survive the reset. Sets last_synced_status
// because this is called from the sync path when the remote reports the job
// is still queued.
func MarkAttemptQueuedByID(database *sql.DB, jobID int64) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}

	// Read placement and intent fields from the current attempt before closing.
	var host string
	var pendingStatus sql.NullString
	var pendingAt sql.NullInt64
	var launchID sql.NullInt64
	err = tx.QueryRow(`
		SELECT host, pending_status, pending_at, launch_id FROM job_attempts
		WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`, jobID,
	).Scan(&host, &pendingStatus, &pendingAt, &launchID)
	if err != nil && err != sql.ErrNoRows {
		tx.Rollback()
		return err
	}

	now := time.Now().Unix()
	if err := closeOpenAttempts(tx, jobID, now); err != nil {
		tx.Rollback()
		return err
	}

	var cloudInstanceID *int64
	if launchID.Valid {
		cloudInstanceID = &launchID.Int64
	}
	attemptID, err := createAttemptTx(tx, jobID, host, cloudInstanceID, StatusQueued)
	if err != nil {
		tx.Rollback()
		return err
	}
	if pendingStatus.Valid || pendingAt.Valid {
		if _, err := tx.Exec(`UPDATE job_attempts SET pending_status = ?, pending_at = ? WHERE id = ?`,
			pendingStatus, pendingAt, attemptID); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE job_attempts SET last_synced_status = ? WHERE id = ?`,
		StatusQueued, attemptID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
