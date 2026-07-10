CREATE VIEW authoritative_job_attempts AS
SELECT ja.* FROM job_attempts ja
WHERE ja.abandoned_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM move_intents mi
       WHERE mi.state = 'open'
         AND mi.id = ja.move_intent_id
  );

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
