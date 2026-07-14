-- +goose Up
DROP VIEW IF EXISTS launch_job_membership;

CREATE VIEW launch_job_membership AS
WITH current_memberships AS (
	SELECT DISTINCT
	       js.id AS job_id,
	       js.launch_id AS membership_launch_id
	FROM job_status js
	WHERE js.launch_id IS NOT NULL
),
move_target_memberships AS (
	SELECT mi.job_id AS job_id,
	       mi.target_launch_id AS membership_launch_id,
	       mi.target_attempt_id AS attempt_id
	FROM move_intents mi
	JOIN job_attempts ja ON ja.id = mi.target_attempt_id
	WHERE mi.state = 'open'
	  AND mi.target_launch_id IS NOT NULL
	  AND mi.target_attempt_id IS NOT NULL
	  AND ja.launch_id = mi.target_launch_id
	  AND ja.abandoned_at IS NULL
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
END AS status, ja.error_message AS error_message, COALESCE(ja.backend, js.backend) AS backend, ja.remote_id AS remote_id, ja.remote_state AS remote_state, ja.failure_reason AS failure_reason, js.gpu, js.gpu_class, js.cpu_allotment, js.gpu_mem_gb, js.gpu_mem_max_gb, js.max_compute_cap, js.env_vars, js.tags, js.dep_spec, js.inputs, ja.observed_inputs AS observed_inputs, js.outputs, js.output_dirs, js.produces, js.needs, js.project, js.tombstoned, ja.last_synced_status AS last_synced_status, ja.pending_status AS pending_status, ja.pending_at AS pending_at, ja.job_metadata AS job_metadata, ja.cost AS cost, ja.error_diagnosis AS error_diagnosis, js.retry_count, ja.placement_meta AS placement_meta, js.placement_reasons, js.cli_overrides, move_target_memberships.membership_launch_id AS launch_id, js.campaign_job_index, ja.id AS latest_run_id, js.placement_blocked,
       move_target_memberships.membership_launch_id,
       'move_target' AS membership_kind,
       1 AS membership_rank
FROM job_status js
JOIN move_target_memberships ON move_target_memberships.job_id = js.id
JOIN job_attempts ja ON ja.id = move_target_memberships.attempt_id
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
       2 AS membership_rank
FROM job_status js
JOIN historical_memberships ON historical_memberships.job_id = js.id
JOIN job_attempts ja ON ja.id = historical_memberships.attempt_id;

-- +goose Down
DROP VIEW IF EXISTS launch_job_membership;

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
