-- +goose Up
CREATE TABLE IF NOT EXISTS external_job_bindings (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	attempt_id INTEGER REFERENCES job_attempts(id),
	executor TEXT NOT NULL,
	external_job_id TEXT NOT NULL,
	external_task_id TEXT NOT NULL DEFAULT '',
	external_cluster_id TEXT,
	external_cluster_name TEXT,
	raw_status TEXT,
	raw_status_message TEXT,
	normalized_status TEXT,
	submitted_from_working_dir TEXT,
	submitted_from_project TEXT,
	dashboard_url TEXT,
	created_at INTEGER NOT NULL,
	last_observed_at INTEGER,
	UNIQUE(executor, external_job_id, external_task_id)
);

CREATE INDEX IF NOT EXISTS idx_external_job_bindings_job ON external_job_bindings(job_id);
CREATE INDEX IF NOT EXISTS idx_external_job_bindings_executor ON external_job_bindings(executor);

DROP VIEW IF EXISTS launch_job_membership;
DROP VIEW IF EXISTS job_status;

CREATE VIEW job_status AS
WITH latest_attempt AS (
	SELECT ja.*,
	       ROW_NUMBER() OVER (PARTITION BY ja.job_id
	                          ORDER BY ja.attempt_number DESC) AS rn
	FROM authoritative_job_attempts ja
)
SELECT
	j.id,
	CASE WHEN COALESCE(la.backend, j.backend) = 'skypilot' THEN ''
	     WHEN et.kind = 'inventory_host' THEN et.host
	     WHEN et.kind = 'rental_instance' THEN ''
	     WHEN la.launch_id IS NOT NULL THEN ''
	     ELSE COALESCE(la.host, '')
	END AS host,
	la.session_name,
	j.working_dir,
	j.command,
	j.description,
	j.generated_description,
	j.generation_hash,
	COALESCE(j.priority, 0) AS priority,
	j.created_at,
	la.queued_at,
	la.start_time,
	la.end_time,
	la.exit_code,
	CASE
		WHEN j.requested_status = 'canceled' THEN 'canceled'
		WHEN j.requested_status = 'killed' THEN 'killed'
		WHEN j.requested_status = 'draft' THEN 'draft'
		WHEN la.id IS NULL THEN
			CASE WHEN j.requested_status = 'queued' THEN 'queued'
			     WHEN j.requested_status IS NOT NULL THEN j.requested_status
			     ELSE 'draft'
			END
		WHEN la.end_time IS NOT NULL THEN
			CASE
				WHEN j.requested_status = 'queued'
				     AND (
				          COALESCE(la.cloud_outcome, '') IN ('orphaned', 'canceled')
				          OR (
				             la.start_time IS NULL
				             AND la.exit_code IS NULL
				             AND COALESCE(la.failure_reason, '') != ''
				          )
				     )
				     THEN 'queued'
				WHEN la.exit_code = 0 THEN 'completed'
				WHEN la.exit_code IS NOT NULL THEN 'failed'
				WHEN la.status = 'completed' THEN 'completed'
				WHEN la.status = 'failed' THEN 'failed'
				WHEN l.termination_reason = 'job_failure' THEN 'failed'
				WHEN l.status IN ('failed','canceled') THEN 'orphaned'
				WHEN la.status = 'dead' THEN 'dead'
				WHEN la.status = 'killed' THEN 'killed'
				WHEN la.status = 'canceled' THEN 'canceled'
				ELSE 'dead'
			END
		WHEN COALESCE(et.launch_id, la.launch_id) IS NOT NULL THEN
			CASE
				WHEN la.start_time IS NOT NULL THEN
					CASE
						WHEN la.status = 'starting' THEN 'starting'
						WHEN l.status IN ('failed','canceled') THEN 'orphaned'
						ELSE 'running'
					END
				WHEN l.status IN ('failed','canceled') THEN 'orphaned'
				ELSE 'queued'
			END
		ELSE la.status
	END AS status,
	la.error_message,
	COALESCE(la.backend, j.backend) AS backend,
	la.remote_id,
	la.remote_state,
	la.failure_reason,
	j.gpu,
	j.gpu_class,
	j.cpu_allotment,
	j.gpu_mem_gb,
	j.gpu_mem_max_gb,
	j.max_compute_cap,
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
	COALESCE(la.job_metadata, j.job_metadata) AS job_metadata,
	la.cost,
	la.error_diagnosis,
	COALESCE(la.attempt_number - 1, 0) AS retry_count,
	la.placement_meta,
	j.placement_reasons,
	j.cli_overrides,
	j.placement_blocked,
	CASE WHEN la.end_time IS NOT NULL
	          AND COALESCE(la.cloud_outcome, '') IN ('orphaned', 'canceled')
	     THEN NULL ELSE COALESCE(et.launch_id, la.launch_id) END AS launch_id,
	j.campaign_job_index,
	la.id AS latest_run_id,
	CASE
		WHEN COALESCE(la.backend, j.backend) = 'skypilot'
		THEN 'external_executor'
		WHEN la.end_time IS NOT NULL
		     AND COALESCE(la.cloud_outcome, '') IN ('orphaned', 'canceled')
		THEN 'unplaced'
		WHEN et.kind = 'rental_instance'
		     AND l.status IN ('launching', 'running', 'grace', 'completed')
		THEN 'rental_instance'
		WHEN et.kind = 'inventory_host'
		THEN 'inventory_host'
		WHEN la.launch_id IS NOT NULL
		     AND l.status IN ('launching', 'running', 'grace', 'completed')
		THEN 'rental_instance'
		WHEN COALESCE(la.host, '') != ''
		     AND COALESCE(la.host, '') NOT LIKE 'vastai:%'
		     AND COALESCE(la.host, '') NOT LIKE 'runpod:%'
		THEN 'inventory_host'
		WHEN COALESCE(la.host, '') LIKE 'vastai:%' OR COALESCE(la.host, '') LIKE 'runpod:%'
		THEN 'rental_instance'
		ELSE 'unplaced'
	END AS effective_target_kind
FROM jobs j
LEFT JOIN latest_attempt la ON la.job_id = j.id AND la.rn = 1
LEFT JOIN execution_targets et ON et.id = la.target_id
LEFT JOIN launches l ON l.id = COALESCE(et.launch_id, la.launch_id);

CREATE VIEW launch_job_membership AS
WITH current_memberships AS (
	SELECT DISTINCT
	       js.id AS job_id,
	       js.launch_id AS membership_launch_id
	FROM job_status js
	WHERE js.launch_id IS NOT NULL
),
historical_memberships AS (
	SELECT job_id, membership_launch_id, attempt_id
	FROM (
		SELECT ja.job_id AS job_id,
		       ja.launch_id AS membership_launch_id,
		       ja.id AS attempt_id,
		       ROW_NUMBER() OVER (
			       PARTITION BY ja.job_id, ja.launch_id
			       ORDER BY ja.attempt_number DESC
		       ) AS rn
		FROM job_attempts ja
		JOIN job_status js ON js.id = ja.job_id
		WHERE ja.launch_id IS NOT NULL
		  AND ja.end_time IS NOT NULL
		  AND (js.launch_id IS NULL OR ja.launch_id != js.launch_id)
	)
	WHERE rn = 1
)
SELECT js.id, js.host, js.session_name, js.working_dir, js.command, js.description, js.generated_description, js.generation_hash, js.priority, js.created_at, js.queued_at, js.start_time, js.end_time, js.exit_code, js.status, js.error_message, js.backend, js.remote_id, js.remote_state, js.failure_reason, js.gpu, js.gpu_class, js.cpu_allotment, js.gpu_mem_gb, js.gpu_mem_max_gb, js.max_compute_cap, js.env_vars, js.tags, js.dep_spec, js.inputs, js.observed_inputs, js.outputs, js.output_dirs, js.produces, js.needs, js.project, js.tombstoned, js.last_synced_status, js.pending_status, js.pending_at, js.job_metadata, js.cost, js.error_diagnosis, js.retry_count, js.placement_meta, js.placement_reasons, js.cli_overrides, current_memberships.membership_launch_id AS launch_id, js.campaign_job_index, js.latest_run_id, js.placement_blocked,
       current_memberships.membership_launch_id,
       'current' AS membership_kind,
       0 AS membership_rank
FROM job_status js
JOIN current_memberships ON current_memberships.job_id = js.id
UNION ALL
SELECT js.id, js.host, js.session_name, js.working_dir, js.command, js.description, js.generated_description, js.generation_hash, js.priority, js.created_at, ja.queued_at AS queued_at, ja.start_time AS start_time, ja.end_time AS end_time, ja.exit_code AS exit_code, CASE
	WHEN COALESCE(ja.cloud_outcome, '') IN ('orphaned', 'canceled')
	     THEN 'queued'
	WHEN ja.exit_code = 0 THEN 'completed'
	WHEN ja.exit_code IS NOT NULL THEN 'failed'
	ELSE ja.status
END AS status, ja.error_message AS error_message, COALESCE(ja.backend, js.backend) AS backend, ja.remote_id AS remote_id, ja.remote_state AS remote_state, ja.failure_reason AS failure_reason, js.gpu, js.gpu_class, js.cpu_allotment, js.gpu_mem_gb, js.gpu_mem_max_gb, js.max_compute_cap, js.env_vars, js.tags, js.dep_spec, js.inputs, ja.observed_inputs AS observed_inputs, js.outputs, js.output_dirs, js.produces, js.needs, js.project, js.tombstoned, ja.last_synced_status AS last_synced_status, ja.pending_status AS pending_status, ja.pending_at AS pending_at, ja.job_metadata AS job_metadata, ja.cost AS cost, ja.error_diagnosis AS error_diagnosis, js.retry_count, ja.placement_meta AS placement_meta, js.placement_reasons, js.cli_overrides, historical_memberships.membership_launch_id AS launch_id, js.campaign_job_index, js.latest_run_id, js.placement_blocked,
       historical_memberships.membership_launch_id,
       'historical' AS membership_kind,
       1 AS membership_rank
FROM job_status js
JOIN historical_memberships ON historical_memberships.job_id = js.id
JOIN job_attempts ja ON ja.id = historical_memberships.attempt_id;

-- +goose Down
DROP VIEW IF EXISTS launch_job_membership;
DROP VIEW IF EXISTS job_status;
DROP INDEX IF EXISTS idx_external_job_bindings_executor;
DROP INDEX IF EXISTS idx_external_job_bindings_job;
DROP TABLE IF EXISTS external_job_bindings;
