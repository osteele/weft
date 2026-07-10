CREATE TABLE IF NOT EXISTS host_syncs (
		name TEXT PRIMARY KEY,
		last_synced INTEGER NOT NULL
	, last_restart_check INTEGER DEFAULT 0);
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
CREATE TABLE IF NOT EXISTS artifacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL,
		name TEXT,
		path TEXT NOT NULL,
		stored_path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT,
		created_at INTEGER NOT NULL
	, attempt_id INTEGER, job_run_id INTEGER);
CREATE INDEX IF NOT EXISTS idx_artifacts_job ON artifacts(job_id);
CREATE TABLE IF NOT EXISTS host_data (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		asset_kind TEXT NOT NULL,
		asset_id TEXT NOT NULL,
		path TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		last_seen INTEGER NOT NULL,
		UNIQUE(host, asset_kind, asset_id)
	);
CREATE INDEX IF NOT EXISTS idx_host_data_host ON host_data(host);
CREATE INDEX IF NOT EXISTS idx_host_data_asset ON host_data(asset_kind, asset_id);
CREATE TABLE IF NOT EXISTS job_timeseries (
		job_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		cpu_pct INTEGER,
		rss_kb INTEGER,
		gpu_mib INTEGER,
		host_rss_kb INTEGER,
		host_mem_total_kb INTEGER,
		gpu_util_pct INTEGER,
		gpu_mem_used_mib INTEGER,
		gpu_mem_total_mib INTEGER,
		tenant TEXT, gpu_temp_c INTEGER, gpu_clock_mhz INTEGER, disk_free_bytes INTEGER, disk_total_bytes INTEGER, attempt_id INTEGER, job_run_id INTEGER,
		PRIMARY KEY (job_id, ts)
	);
CREATE INDEX IF NOT EXISTS idx_job_timeseries_job ON job_timeseries(job_id);
CREATE TABLE IF NOT EXISTS job_phase_timings (
		job_id INTEGER PRIMARY KEY REFERENCES jobs(id),
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
		peak_gpu_mem_mib INTEGER,
		mean_gpu_util INTEGER,
		peak_gpu_util INTEGER
	, uv_sync_seconds INTEGER, cache_uv_post_bytes INTEGER, cache_hf_post_bytes INTEGER, disk_used_bytes INTEGER, disk_total_bytes INTEGER, output_upload_files INTEGER, output_upload_retries INTEGER, output_upload_duration_ms INTEGER, results_upload_files INTEGER, results_upload_retries INTEGER, results_upload_duration_ms INTEGER);
CREATE TABLE IF NOT EXISTS processed_intents (
	intent_id TEXT PRIMARY KEY,
	processed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS transfer_observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			dest_key TEXT NOT NULL,
			source_instance_id TEXT,
			dest_instance_id TEXT,
			bytes_transferred INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			observed_bw_bps REAL NOT NULL,
			created_at INTEGER NOT NULL
		);
CREATE TABLE IF NOT EXISTS data_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		asset_kind TEXT NOT NULL,
		asset_id TEXT NOT NULL,
		revision TEXT NOT NULL DEFAULT 'main',
		status TEXT NOT NULL,
		error_message TEXT DEFAULT '',
		remote_path TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		requested_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER
	);
CREATE INDEX IF NOT EXISTS idx_data_requests_host ON data_requests(host);
CREATE INDEX IF NOT EXISTS idx_data_requests_status ON data_requests(status);
CREATE INDEX IF NOT EXISTS idx_data_requests_asset ON data_requests(asset_kind, asset_id);
CREATE TABLE IF NOT EXISTS processed_relay_requests (
		request_id TEXT PRIMARY KEY,
		op TEXT NOT NULL,
		job_id INTEGER NOT NULL DEFAULT 0,
		processed_at INTEGER NOT NULL
	);
CREATE TABLE IF NOT EXISTS job_telemetry_samples (
		job_id INTEGER NOT NULL,
		attempt_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		elapsed_s REAL,
		proc_cpu_user_s REAL,
		proc_cpu_sys_s REAL,
		proc_rss_kb INTEGER,
		host_cpu_util_pct REAL,
		proc_disk_read_bps REAL,
		proc_disk_write_bps REAL,
		proc_net_rx_bps REAL,
		proc_net_tx_bps REAL,
		PRIMARY KEY (attempt_id, ts)
	);
CREATE INDEX IF NOT EXISTS idx_job_telemetry_samples_job ON job_telemetry_samples(job_id);
CREATE INDEX IF NOT EXISTS idx_job_telemetry_samples_run ON job_telemetry_samples(attempt_id);
CREATE TABLE IF NOT EXISTS job_telemetry_gpus (
		job_id INTEGER NOT NULL,
		attempt_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		gpu_index TEXT NOT NULL,
		gpu_name TEXT,
		gpu_mem_used_mib INTEGER,
		gpu_util_pct REAL,
		gpu_mem_util_pct REAL,
		gpu_power_w REAL,
		gpu_pcie_tx_mib_s REAL,
		gpu_pcie_rx_mib_s REAL,
		gpu_sm_clock_mhz INTEGER,
		gpu_mem_clock_mhz INTEGER,
		PRIMARY KEY (attempt_id, ts, gpu_index)
	);
CREATE INDEX IF NOT EXISTS idx_job_telemetry_gpus_job ON job_telemetry_gpus(job_id);
CREATE INDEX IF NOT EXISTS idx_job_telemetry_gpus_run ON job_telemetry_gpus(attempt_id);
CREATE TABLE IF NOT EXISTS "campaigns" (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		status TEXT NOT NULL DEFAULT 'planned',
		created_at INTEGER NOT NULL,
		ended_at INTEGER,
		estimated_cost_cents INTEGER,
		distinct_machines INTEGER NOT NULL DEFAULT 0,
		avoid_machines TEXT,
		CONSTRAINT campaigns_status_check CHECK (status IN ('planned', 'launching', 'running', 'completed', 'failed', 'canceled'))
	);
CREATE TABLE IF NOT EXISTS provider_status_transitions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			launch_id INTEGER NOT NULL REFERENCES "launches"(id),
			observed_at INTEGER NOT NULL,
			old_status TEXT NOT NULL DEFAULT '',
			new_status TEXT NOT NULL
		);
CREATE INDEX IF NOT EXISTS idx_pst_instance ON provider_status_transitions(launch_id);
CREATE TABLE IF NOT EXISTS "jobs" (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		working_dir TEXT NOT NULL,
		command TEXT NOT NULL,
		description TEXT,
		generated_description TEXT,
		generation_hash TEXT,
		created_at INTEGER,
		backend TEXT DEFAULT 'queue-runner',
		queue_name TEXT,
		gpu TEXT,
		gpu_class TEXT,
		cpu_allotment INTEGER,
		gpu_mem_gb INTEGER,
		env_vars TEXT,
		tags TEXT,
		dep_spec TEXT,
		inputs TEXT,
		outputs TEXT,
		output_dirs TEXT,
		produces TEXT,
		needs TEXT,
		project TEXT,
		tombstoned INTEGER NOT NULL DEFAULT 0,
		placement_host TEXT,
		placement_reasons TEXT,
		campaign_job_index INTEGER,
		requested_status TEXT
	, gpu_mem_max_gb INTEGER, cli_overrides TEXT, max_compute_cap TEXT, priority INTEGER NOT NULL DEFAULT 0, job_metadata TEXT, placement_blocked TEXT);
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
CREATE TABLE IF NOT EXISTS host_info_cache (
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
CREATE TABLE IF NOT EXISTS launch_live_state (
		launch_id         INTEGER PRIMARY KEY REFERENCES launches(id),
		instance_phase    TEXT,
		bootstrap_stage   TEXT,
		heartbeat_json    TEXT,
		heartbeat_ts      INTEGER,
		job_progress_pct  INTEGER,
		job_progress_id   INTEGER,
		agent_version     TEXT,
		updated_at        INTEGER NOT NULL
	, phase_changed_at INTEGER, job_progress_phase INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			occurred_at INTEGER NOT NULL,
			event_kind TEXT NOT NULL,
			launch_id INTEGER,
			campaign_id INTEGER,
			job_id INTEGER,
			gpu_spec TEXT,
			job_count INTEGER,
			detail TEXT,
			error_text TEXT,
			attempt_number INTEGER,
			max_attempts INTEGER,
			disk_gb INTEGER
		);
CREATE INDEX IF NOT EXISTS idx_le_kind ON lifecycle_events(event_kind);
CREATE INDEX IF NOT EXISTS idx_le_launch ON lifecycle_events(launch_id);
CREATE INDEX IF NOT EXISTS idx_le_occurred ON lifecycle_events(occurred_at);
CREATE TABLE IF NOT EXISTS host_contention_obs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			host TEXT NOT NULL,
			gpu_pct INTEGER,
			cpu_pct INTEGER,
			queue_depth INTEGER,
			gpu_jobs_queued INTEGER,
			observed_at INTEGER NOT NULL
		);
CREATE INDEX IF NOT EXISTS idx_host_contention_host ON host_contention_obs(host, observed_at);
CREATE TABLE IF NOT EXISTS auto_leases (
			scope TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
CREATE INDEX IF NOT EXISTS idx_auto_leases_expires ON auto_leases(expires_at);
CREATE TABLE IF NOT EXISTS "job_attempts" (
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

		-- Predecessor in a relaunch chain (e.g. set when a preempted
		-- interruptible launch is replaced and the job is reattached to
		-- a fresh launch). Lets queries sum runtime across the chain.
		predecessor_attempt_id INTEGER REFERENCES job_attempts(id), target_id INTEGER REFERENCES execution_targets(id), error_diagnosis_backfilled INTEGER NOT NULL DEFAULT 0,

		UNIQUE(job_id, attempt_number),
		CONSTRAINT job_attempts_status_check CHECK (status IN ('starting', 'running', 'completed', 'dead', 'queued', 'failed', 'killed', 'canceled', 'paused', 'draft', 'pending_placement')),
		CONSTRAINT job_attempts_cloud_outcome_check CHECK (
			cloud_outcome IS NULL OR cloud_outcome IN ('completed','failed','canceled','orphaned','superseded','preempted')
		)
	);
CREATE INDEX IF NOT EXISTS idx_job_attempts_job ON job_attempts(job_id, attempt_number DESC);
CREATE INDEX IF NOT EXISTS idx_job_attempts_status ON job_attempts(status);
CREATE INDEX IF NOT EXISTS idx_job_attempts_launch ON job_attempts(launch_id);
CREATE TABLE IF NOT EXISTS autopilot_state (
			id                       INTEGER PRIMARY KEY CHECK (id = 1),
			paused                   INTEGER NOT NULL DEFAULT 0,
			paused_at                INTEGER,
			paused_by                TEXT,
			paused_reason            TEXT,
			active_runner_pid        INTEGER,
			active_runner_label      TEXT,
			active_runner_host       TEXT,
			pass_started_at          INTEGER,
			last_heartbeat           INTEGER,
			last_pass_finished_at    INTEGER,
			last_pass_duration_ms    INTEGER,
			last_pass_summary        TEXT,
			last_pass_error          TEXT
		, active_binary_path TEXT, active_binary_size INTEGER, active_binary_mtime INTEGER, active_binary_dev INTEGER, active_binary_ino INTEGER);
CREATE TABLE IF NOT EXISTS move_intents (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					source_attempt_id INTEGER REFERENCES job_attempts(id),
					source_launch_id INTEGER REFERENCES launches(id),
					target_kind TEXT NOT NULL CHECK (target_kind IN ('existing','new')),
					target_launch_id INTEGER REFERENCES launches(id),
					target_offer_provider TEXT,
					target_offer_id TEXT,
					target_gpu_name TEXT,
					state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','confirmed','canceled','obsoleted')),
					created_at INTEGER NOT NULL,
					resolved_at INTEGER,
					resolution TEXT
				, attempt_count INTEGER NOT NULL DEFAULT 1, max_attempts INTEGER NOT NULL DEFAULT 1);
CREATE INDEX IF NOT EXISTS idx_move_intents_job ON move_intents(job_id);
CREATE INDEX IF NOT EXISTS idx_move_intents_target_launch ON move_intents(target_launch_id) WHERE state = 'open';
CREATE TABLE IF NOT EXISTS placement_intents (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					operation TEXT NOT NULL,
					state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','confirmed','canceled')),
					created_at INTEGER NOT NULL,
					resolved_at INTEGER,
					resolution TEXT
				);
CREATE INDEX IF NOT EXISTS idx_placement_intents_job ON placement_intents(job_id);
CREATE TABLE IF NOT EXISTS execution_targets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		launch_id INTEGER REFERENCES launches(id),
		status TEXT NOT NULL DEFAULT 'ready',
		gpu_class TEXT,
		gpu_mem_gb INTEGER NOT NULL DEFAULT 0,
		num_gpus INTEGER NOT NULL DEFAULT 0,
		cordoned INTEGER NOT NULL DEFAULT 0,
		cordon_reason TEXT,
		cordoned_at INTEGER,
		deadline_unix INTEGER,
		current_job_id INTEGER REFERENCES jobs(id),
		queue_depth INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		last_observed_at INTEGER,
		CONSTRAINT execution_targets_kind_check CHECK (kind IN ('inventory_host', 'rental_instance')),
		CONSTRAINT execution_targets_status_check CHECK (status IN ('provisioning', 'ready', 'running', 'draining', 'terminated')),
		CONSTRAINT execution_targets_shape_check CHECK (
			(kind = 'inventory_host' AND host != '' AND launch_id IS NULL)
			OR (kind = 'rental_instance' AND launch_id IS NOT NULL)
		)
	);
CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_targets_inventory_host ON execution_targets(host) WHERE kind = 'inventory_host';
CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_targets_launch ON execution_targets(launch_id) WHERE kind = 'rental_instance';
CREATE INDEX IF NOT EXISTS idx_execution_targets_kind_status ON execution_targets(kind, status);
CREATE INDEX IF NOT EXISTS idx_execution_targets_cordoned ON execution_targets(cordoned);
CREATE TABLE IF NOT EXISTS bootstrap_transitions (
			launch_id INTEGER NOT NULL REFERENCES launches(id),
			stage TEXT NOT NULL,
			entered_at INTEGER NOT NULL,
			PRIMARY KEY (launch_id, stage, entered_at)
		);
CREATE INDEX IF NOT EXISTS idx_bootstrap_transitions_stage_entered
			ON bootstrap_transitions(stage, entered_at);
CREATE INDEX IF NOT EXISTS idx_bootstrap_transitions_launch_entered
			ON bootstrap_transitions(launch_id, entered_at);
CREATE INDEX IF NOT EXISTS idx_job_attempts_target ON job_attempts(target_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_attempts_one_open ON job_attempts(job_id) WHERE end_time IS NULL;
CREATE TABLE IF NOT EXISTS job_open_attempts (
			job_id INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
			attempt_id INTEGER NOT NULL UNIQUE REFERENCES job_attempts(id) ON DELETE CASCADE
		);
CREATE INDEX IF NOT EXISTS idx_job_open_attempts_attempt ON job_open_attempts(attempt_id);
CREATE TABLE IF NOT EXISTS prediction_history (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					attempt_id INTEGER REFERENCES job_attempts(id),
					target TEXT NOT NULL,
					host TEXT,
					gpu_class TEXT,
					prediction_json TEXT,
					level0_json TEXT,
					level1_json TEXT,
					level2_json TEXT,
					level3_json TEXT,
					model_fingerprint TEXT,
					metadata_json TEXT,
					created_at INTEGER NOT NULL
				);
CREATE INDEX IF NOT EXISTS idx_prediction_history_job ON prediction_history(job_id, attempt_id);
CREATE INDEX IF NOT EXISTS idx_prediction_history_target ON prediction_history(target, created_at);
CREATE TABLE IF NOT EXISTS placement_decisions (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER REFERENCES jobs(id),
					attempt_id INTEGER REFERENCES job_attempts(id),
					campaign_id INTEGER REFERENCES campaigns(id),
					decision_kind TEXT NOT NULL,
					operation TEXT,
					objective TEXT,
					placement_policy_version TEXT,
					model_fingerprint TEXT,
					selected_kind TEXT,
					selected_target TEXT,
					selected_score REAL,
					sample_candidates INTEGER NOT NULL DEFAULT 0,
					created_at INTEGER NOT NULL
				, details_json TEXT);
CREATE INDEX IF NOT EXISTS idx_placement_decisions_job ON placement_decisions(job_id, attempt_id);
CREATE INDEX IF NOT EXISTS idx_placement_decisions_campaign ON placement_decisions(campaign_id);
CREATE TABLE IF NOT EXISTS placement_candidates (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					decision_id INTEGER NOT NULL REFERENCES placement_decisions(id) ON DELETE CASCADE,
					rank INTEGER NOT NULL,
					candidate_kind TEXT NOT NULL,
					target TEXT,
					provider TEXT,
					offer_id TEXT,
					gpu_name TEXT,
					score REAL,
					est_time_s REAL,
					est_cost REAL,
					survival REAL,
					selected INTEGER NOT NULL DEFAULT 0,
					reasons_json TEXT,
					details_json TEXT
				);
CREATE INDEX IF NOT EXISTS idx_placement_candidates_decision ON placement_candidates(decision_id, rank);
CREATE TABLE IF NOT EXISTS donor_experiments (
					campaign_id INTEGER PRIMARY KEY REFERENCES campaigns(id) ON DELETE CASCADE,
					cohort TEXT NOT NULL,
					sample_rate REAL NOT NULL,
					donor_launch_id INTEGER REFERENCES launches(id),
					reason TEXT,
					assigned_at INTEGER NOT NULL
				);
CREATE TABLE IF NOT EXISTS failure_label_reviews (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER REFERENCES jobs(id),
					attempt_id INTEGER REFERENCES job_attempts(id),
					heuristic_label TEXT,
					human_label TEXT,
					reviewer TEXT,
					notes TEXT,
					reviewed_at INTEGER
				);
CREATE INDEX IF NOT EXISTS idx_failure_label_reviews_attempt ON failure_label_reviews(job_id, attempt_id);
CREATE TABLE IF NOT EXISTS hosts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		provider TEXT,
		provider_machine_id TEXT,
		first_seen_at INTEGER NOT NULL,
		UNIQUE(provider, provider_machine_id)
	);
CREATE INDEX IF NOT EXISTS idx_hosts_provider ON hosts(provider, provider_machine_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_job_name_path_legacy ON artifacts(job_id, name, path) WHERE attempt_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_run_name_path ON artifacts(attempt_id, name, path) WHERE attempt_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_artifacts_run ON artifacts(attempt_id);
CREATE INDEX IF NOT EXISTS idx_job_timeseries_run ON job_timeseries(attempt_id);
CREATE TRIGGER IF NOT EXISTS artifacts_validate_job_run_ownership_on_insert
		BEFORE INSERT ON artifacts
		FOR EACH ROW
		WHEN NEW.attempt_id IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = NEW.attempt_id AND job_id = NEW.job_id)
		BEGIN
			SELECT RAISE(ABORT, 'artifacts.attempt_id must reference an attempt owned by artifacts.job_id');
		END;
CREATE TRIGGER IF NOT EXISTS artifacts_validate_job_run_ownership_on_update
		BEFORE UPDATE OF job_id, attempt_id ON artifacts
		FOR EACH ROW
		WHEN NEW.attempt_id IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = NEW.attempt_id AND job_id = NEW.job_id)
		BEGIN
			SELECT RAISE(ABORT, 'artifacts.attempt_id must reference an attempt owned by artifacts.job_id');
		END;
CREATE TRIGGER IF NOT EXISTS job_timeseries_validate_job_run_ownership_on_insert
		BEFORE INSERT ON job_timeseries
		FOR EACH ROW
		WHEN NEW.attempt_id IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = NEW.attempt_id AND job_id = NEW.job_id)
		BEGIN
			SELECT RAISE(ABORT, 'job_timeseries.attempt_id must reference an attempt owned by job_timeseries.job_id');
		END;
CREATE TRIGGER IF NOT EXISTS job_timeseries_validate_job_run_ownership_on_update
		BEFORE UPDATE OF job_id, attempt_id ON job_timeseries
		FOR EACH ROW
		WHEN NEW.attempt_id IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = NEW.attempt_id AND job_id = NEW.job_id)
		BEGIN
			SELECT RAISE(ABORT, 'job_timeseries.attempt_id must reference an attempt owned by job_timeseries.job_id');
		END;
CREATE TRIGGER IF NOT EXISTS launch_live_state_orphan_open_queued_attempts_on_insert
		AFTER INSERT ON launch_live_state
		FOR EACH ROW
		WHEN lower(
			CASE
				WHEN instr(COALESCE(NEW.instance_phase, ''), ':') > 0
					THEN substr(NEW.instance_phase, 1, instr(NEW.instance_phase, ':') - 1)
				ELSE COALESCE(NEW.instance_phase, '')
			END
		) = 'destroying'
		BEGIN
			UPDATE jobs
			   SET requested_status = 'queued'
			 WHERE id IN (
				SELECT job_id
				  FROM job_attempts
				 WHERE launch_id = NEW.launch_id
				   AND end_time IS NULL
				   AND status IN ('queued', 'pending_placement')
			 );
			UPDATE job_attempts
			   SET status = 'canceled',
			       end_time = strftime('%s','now'),
			       cloud_outcome = 'orphaned',
			       pending_status = NULL
			 WHERE launch_id = NEW.launch_id
			   AND end_time IS NULL
			   AND status IN ('queued', 'pending_placement');
		END;
CREATE TRIGGER IF NOT EXISTS launch_live_state_orphan_open_queued_attempts_on_update
		AFTER UPDATE OF instance_phase ON launch_live_state
		FOR EACH ROW
		WHEN lower(
			CASE
				WHEN instr(COALESCE(NEW.instance_phase, ''), ':') > 0
					THEN substr(NEW.instance_phase, 1, instr(NEW.instance_phase, ':') - 1)
				ELSE COALESCE(NEW.instance_phase, '')
			END
		) = 'destroying'
		BEGIN
			UPDATE jobs
			   SET requested_status = 'queued'
			 WHERE id IN (
				SELECT job_id
				  FROM job_attempts
				 WHERE launch_id = NEW.launch_id
				   AND end_time IS NULL
				   AND status IN ('queued', 'pending_placement')
			 );
			UPDATE job_attempts
			   SET status = 'canceled',
			       end_time = strftime('%s','now'),
			       cloud_outcome = 'orphaned',
			       pending_status = NULL
			 WHERE launch_id = NEW.launch_id
			   AND end_time IS NULL
			   AND status IN ('queued', 'pending_placement');
		END;
CREATE TABLE IF NOT EXISTS "launches" (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		campaign_id INTEGER REFERENCES campaigns(id),
		host_id INTEGER REFERENCES hosts(id),
		status TEXT NOT NULL DEFAULT 'planned',
		provider TEXT NOT NULL DEFAULT 'vastai',
		gpu_spec TEXT,
		gpu_class TEXT,
		gpu_mem_gb INTEGER,
		max_spend_cents INTEGER,
		max_time_seconds INTEGER,
		actual_spend_cents INTEGER,
		created_at INTEGER NOT NULL,
		ready_at INTEGER,
		launched_at INTEGER,
		ended_at INTEGER,
		resolved_gpu_name TEXT,
		cost_per_hour_cents INTEGER,
		num_gpus INTEGER,
		dl_perf REAL,
		reliability REAL,
		inet_down_mbps REAL,
		inet_up_mbps REAL,
		cuda_version REAL,
		cpu_cores_effective INTEGER,
		cpu_name TEXT,
		ram_gb INTEGER,
		provider_instance_id TEXT,
		data_center TEXT,
		instance_role TEXT DEFAULT 'worker',
		donor_instance_id INTEGER,
		seed_download_secs INTEGER,
		seed_copy_secs INTEGER,
		replaced_instance_id INTEGER,
		grace_period_seconds INTEGER,
		grace_started_at INTEGER,
		grace_deadline INTEGER,
		termination_reason TEXT,
		termination_detail TEXT,
		disk_gb INTEGER,
		provisioned_inputs TEXT,
		termination_requested_at INTEGER,
		termination_intent_json TEXT,
		results_verified INTEGER,
		machine_id TEXT DEFAULT '',
		docker_image TEXT,
		provider_running_at INTEGER,
		oplog_synced_at INTEGER,
		oplog_not_found INTEGER DEFAULT 0,
		oplog_timeout INTEGER DEFAULT 0,
		instance_type TEXT,
		runpod_cloud_type TEXT,
		max_bid_price_cents INTEGER,
		on_demand_ref_cents INTEGER,
		cordoned INTEGER DEFAULT 0,
		cordon_reason TEXT,
		cordoned_at INTEGER,
		bootstrap_deadline_unix INTEGER,
		agent_ready_at_unix INTEGER,
		target_id INTEGER,
		hedge_cohort_id INTEGER,
		first_onstart_probe_seen_unix INTEGER,
		CONSTRAINT launches_termination_reason_check CHECK (termination_reason IS NULL OR termination_reason IN ('completed', 'provider_failure', 'job_failure', 'disk_full', 'infra_failure', 'bootstrap_timeout', 'phase_stall', 'preempted', 'canceled', 'unknown', 'weft_bug')),
		CONSTRAINT launches_status_check CHECK (status IN ('planned', 'launching', 'running', 'paused', 'grace', 'completed', 'failed', 'canceled'))
	);
CREATE TRIGGER IF NOT EXISTS job_open_attempts_validate_insert
			BEFORE INSERT ON job_open_attempts
			FOR EACH ROW
			WHEN NOT EXISTS (
				SELECT 1 FROM job_attempts
				 WHERE id = NEW.attempt_id
				   AND job_id = NEW.job_id
				   AND end_time IS NULL
			)
			BEGIN
				SELECT RAISE(ABORT, 'job_open_attempts must reference an open attempt owned by the job');
			END;
CREATE TRIGGER IF NOT EXISTS job_open_attempts_validate_update
			BEFORE UPDATE OF job_id, attempt_id ON job_open_attempts
			FOR EACH ROW
			WHEN NOT EXISTS (
				SELECT 1 FROM job_attempts
				 WHERE id = NEW.attempt_id
				   AND job_id = NEW.job_id
				   AND end_time IS NULL
			)
			BEGIN
				SELECT RAISE(ABORT, 'job_open_attempts must reference an open attempt owned by the job');
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_insert
			AFTER INSERT ON job_attempts
			FOR EACH ROW
			WHEN NEW.end_time IS NULL
			BEGIN
				INSERT INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_close_or_move
			AFTER UPDATE OF job_id, end_time ON job_attempts
			FOR EACH ROW
			WHEN OLD.end_time IS NULL
			     AND (NEW.end_time IS NOT NULL OR NEW.job_id != OLD.job_id)
			BEGIN
				DELETE FROM job_open_attempts WHERE attempt_id = OLD.id;
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_open_or_move
			AFTER UPDATE OF job_id, end_time ON job_attempts
			FOR EACH ROW
			WHEN NEW.end_time IS NULL
			     AND (OLD.end_time IS NOT NULL OR NEW.job_id != OLD.job_id)
			BEGIN
				INSERT INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_delete
			AFTER DELETE ON job_attempts
			FOR EACH ROW
			WHEN OLD.end_time IS NULL
			BEGIN
				DELETE FROM job_open_attempts WHERE attempt_id = OLD.id;
			END;
CREATE TRIGGER IF NOT EXISTS move_intents_open_new_requires_live_source_insert
			BEFORE INSERT ON move_intents
			FOR EACH ROW
			WHEN NEW.state = 'open'
			     AND NEW.target_kind = 'new'
			     AND NEW.source_launch_id IS NOT NULL
			     AND NOT EXISTS (
			        SELECT 1 FROM launches
			         WHERE id = NEW.source_launch_id
			           AND status IN ('running','launching','paused','grace')
			     )
			BEGIN
				SELECT RAISE(ABORT, 'open move-to-new intent requires live source launch');
			END;
CREATE TRIGGER IF NOT EXISTS move_intents_open_new_requires_live_source_update
			BEFORE UPDATE OF state, target_kind, source_launch_id ON move_intents
			FOR EACH ROW
			WHEN NEW.state = 'open'
			     AND NEW.target_kind = 'new'
			     AND NEW.source_launch_id IS NOT NULL
			     AND NOT EXISTS (
			        SELECT 1 FROM launches
			         WHERE id = NEW.source_launch_id
			           AND status IN ('running','launching','paused','grace')
			     )
			BEGIN
				SELECT RAISE(ABORT, 'open move-to-new intent requires live source launch');
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_prevent_mixed_host_launch_insert
			BEFORE INSERT ON job_attempts
			FOR EACH ROW
			WHEN COALESCE(NEW.host, '') != '' AND NEW.launch_id IS NOT NULL
			     AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'rental:%'
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts cannot target both inventory host and launch');
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_prevent_mixed_host_launch_update
			BEFORE UPDATE OF host, launch_id ON job_attempts
			FOR EACH ROW
			WHEN COALESCE(NEW.host, '') != '' AND NEW.launch_id IS NOT NULL
			     AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'rental:%'
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts cannot target both inventory host and launch');
			END;
CREATE UNIQUE INDEX IF NOT EXISTS idx_move_intents_open ON move_intents(job_id) WHERE state = 'open';
CREATE UNIQUE INDEX IF NOT EXISTS idx_placement_intents_open ON placement_intents(job_id) WHERE state = 'open';
CREATE TRIGGER IF NOT EXISTS job_attempts_sync_target_shadow_insert
			AFTER INSERT ON job_attempts
			FOR EACH ROW
			WHEN NEW.target_id IS NOT NULL
			BEGIN
				UPDATE job_attempts
				   SET host = CASE
				              WHEN (SELECT kind FROM execution_targets WHERE id = NEW.target_id) = 'inventory_host'
				              THEN (SELECT host FROM execution_targets WHERE id = NEW.target_id)
				              ELSE host
				          END,
				       launch_id = (SELECT launch_id FROM execution_targets WHERE id = NEW.target_id)
				 WHERE id = NEW.id;
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_sync_target_shadow_update
			AFTER UPDATE OF target_id ON job_attempts
			FOR EACH ROW
			WHEN NEW.target_id IS NOT NULL
			BEGIN
				UPDATE job_attempts
				   SET host = CASE
				              WHEN (SELECT kind FROM execution_targets WHERE id = NEW.target_id) = 'inventory_host'
				              THEN (SELECT host FROM execution_targets WHERE id = NEW.target_id)
				              ELSE host
				          END,
				       launch_id = (SELECT launch_id FROM execution_targets WHERE id = NEW.target_id)
				 WHERE id = NEW.id;
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_reject_target_shadow_mismatch_insert
			BEFORE INSERT ON job_attempts
			FOR EACH ROW
			WHEN NEW.target_id IS NOT NULL
			 AND EXISTS (
				SELECT 1
				  FROM execution_targets et
				 WHERE et.id = NEW.target_id
				   AND (
				        (et.kind = 'inventory_host'
				         AND NEW.launch_id IS NOT NULL)
				        OR (et.kind = 'inventory_host'
				            AND COALESCE(NULLIF(NEW.host, ''), et.host) != et.host)
				        OR (et.kind = 'rental_instance'
				            AND COALESCE(NEW.launch_id, et.launch_id) != et.launch_id)
				        OR (et.kind = 'rental_instance'
				            AND COALESCE(NEW.host, '') != ''
				            AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
				            AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
				            AND COALESCE(NEW.host, '') NOT LIKE 'rental:%')
				   )
			 )
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts target_id disagrees with placement shadow columns');
			END;
CREATE TRIGGER IF NOT EXISTS job_attempts_reject_target_shadow_mismatch_update
			BEFORE UPDATE OF host, launch_id, target_id ON job_attempts
			FOR EACH ROW
			WHEN NEW.target_id IS NOT NULL
			 AND EXISTS (
				SELECT 1
				  FROM execution_targets et
				 WHERE et.id = NEW.target_id
				   AND (
				        (et.kind = 'inventory_host'
				         AND NEW.launch_id IS NOT NULL)
				        OR (et.kind = 'inventory_host'
				            AND COALESCE(NULLIF(NEW.host, ''), et.host) != et.host)
				        OR (et.kind = 'rental_instance'
				            AND COALESCE(NEW.launch_id, et.launch_id) != et.launch_id)
				        OR (et.kind = 'rental_instance'
				            AND COALESCE(NEW.host, '') != ''
				            AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
				            AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
				            AND COALESCE(NEW.host, '') NOT LIKE 'rental:%')
				   )
			 )
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts target_id disagrees with placement shadow columns');
			END;
CREATE VIEW IF NOT EXISTS launch_job_membership AS
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
		JOIN job_attempts ja ON ja.id = historical_memberships.attempt_id
;
CREATE TRIGGER IF NOT EXISTS launches_clear_pending_placement_on_terminal
		AFTER UPDATE OF status ON launches
		FOR EACH ROW
		WHEN NEW.status IN ('failed', 'canceled', 'completed')
		     AND OLD.status <> NEW.status
		BEGIN
			UPDATE job_attempts
			   SET pending_status = NULL,
			       pending_at = NULL
			 WHERE launch_id = NEW.id
			   AND pending_status = 'pending_placement';
		END;
CREATE VIEW IF NOT EXISTS job_status AS
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
				-- Terminal attempts: derive script outcomes from the
				-- same facts for both inventory hosts and rentals.
				WHEN la.end_time IS NOT NULL THEN
					CASE
						-- Non-execution closures stay queued for replan.
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
				-- Cloud jobs: derive active/not-yet-started state from instance lifecycle
				WHEN COALESCE(et.launch_id, la.launch_id) IS NOT NULL THEN
					CASE
				WHEN la.start_time IS NOT NULL THEN
					CASE
						WHEN la.status = 'starting' THEN 'starting'
						WHEN l.status IN ('failed','canceled') THEN 'orphaned'
						ELSE 'running'
					END
						-- Placed but not yet started
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
			-- retry_count = number of prior attempts (attempt_count - 1), or 0
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
			-- Target kind for placement queries. execution_targets is
			-- authoritative; host and launch_id are compatibility shadows.
			-- Only count a launch as claiming if it is actively progressing;
			-- planned/failed/cancelled launches do not block re-launch.
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
				     AND COALESCE(la.host, '') NOT LIKE 'vastai:%%'
				     AND COALESCE(la.host, '') NOT LIKE 'runpod:%%'
				THEN 'inventory_host'
				WHEN COALESCE(la.host, '') LIKE 'vastai:%%' OR COALESCE(la.host, '') LIKE 'runpod:%%'
				THEN 'rental_instance'
				ELSE 'unplaced'
			END AS effective_target_kind
		FROM jobs j
		LEFT JOIN latest_attempt la ON la.job_id = j.id AND la.rn = 1
		LEFT JOIN execution_targets et ON et.id = la.target_id
		LEFT JOIN launches l ON l.id = COALESCE(et.launch_id, la.launch_id)
;
CREATE VIEW IF NOT EXISTS training_examples AS
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
				ja.cloud_outcome,
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
				
				jpt.wrapper_start,
				jpt.setup_start,
				jpt.setup_end,
				jpt.upload_start,
				jpt.upload_end,
				NULLIF(REPLACE(COALESCE(json_extract(ja.job_metadata, '$.resource.gpu_devices'), ''), ' ', ''), '') AS assigned_gpu_devices_csv,
				CASE
					WHEN json_type(ja.job_metadata, '$.telemetry.assigned_gpu_indices') = 'array'
					THEN json_extract(ja.job_metadata, '$.telemetry.assigned_gpu_indices')
				END AS assigned_gpu_indices_json
			FROM job_attempts ja
			JOIN jobs j ON j.id = ja.job_id
			LEFT JOIN host_info_cache hic ON hic.name = ja.host
			LEFT JOIN launches l ON l.id = ja.launch_id
			LEFT JOIN job_phase_timings jpt ON jpt.job_id = ja.job_id
			WHERE ja.start_time IS NOT NULL AND ja.end_time IS NOT NULL
		),
		selected_gpu_inventory AS (
			SELECT
				cr.run_id,
				json_extract(je.value, '$.Index') AS gpu_index,
				NULLIF(json_extract(je.value, '$.Name'), '') AS gpu_name,
				
		CASE
			WHEN NULLIF(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')), '') IS NULL THEN NULL
			WHEN lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')) GLOB '*gib' THEN CAST(replace(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')), 'gib', '') AS INTEGER) * 1024
			WHEN lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')) GLOB '*gb' THEN CAST(replace(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')), 'gb', '') AS INTEGER) * 1024
			WHEN lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')) GLOB '*mib' THEN CAST(replace(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')), 'mib', '') AS INTEGER)
			WHEN lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')) GLOB '*mb' THEN CAST(replace(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')), 'mb', '') AS INTEGER)
			ELSE CAST(lower(replace(trim(coalesce(json_extract(je.value, '$.MemTotal'), '')), ' ', '')) AS INTEGER)
		END AS gpu_vram_mib,
				
		CASE
			WHEN NULLIF(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), '') IS NULL THEN NULL
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%b200%' THEN 'b200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h200%' THEN 'h200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h100%' THEN 'h100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a100%' THEN 'a100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40s%' THEN 'l40s'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40%' THEN 'l40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a40%' THEN 'a40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10g%' THEN 'a10g'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10%' THEN 'a10'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%m2max%' THEN 'm2max'
			WHEN instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx') > 0 THEN substr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx'))
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]' OR replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]ti' THEN 'rtx' || replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
			ELSE replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(json_extract(je.value, '$.Name'), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
		END AS gpu_class
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
			base.cloud_outcome,
			CASE
				WHEN COALESCE(base.cloud_outcome, '') != '' THEN base.cloud_outcome
				WHEN LOWER(TRIM(COALESCE(base.status, ''))) = 'completed' AND COALESCE(base.exit_code, 0) = 0 THEN 'completed'
				WHEN LOWER(TRIM(COALESCE(base.status, ''))) IN ('canceled', 'cancelled') THEN 'canceled'
				WHEN LOWER(TRIM(COALESCE(base.status, ''))) = 'preempted' THEN 'preempted'
				ELSE 'failed'
			END AS terminal_outcome,
			CASE
				WHEN LOWER(TRIM(COALESCE(base.status, ''))) = 'completed' AND COALESCE(base.exit_code, 0) = 0 THEN 0
				ELSE 1
			END AS censored,
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
				WHEN NULLIF(
		CASE
			WHEN NULLIF(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), '') IS NULL THEN NULL
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%b200%' THEN 'b200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h200%' THEN 'h200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h100%' THEN 'h100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a100%' THEN 'a100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40s%' THEN 'l40s'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40%' THEN 'l40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a40%' THEN 'a40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10g%' THEN 'a10g'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10%' THEN 'a10'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%m2max%' THEN 'm2max'
			WHEN instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx') > 0 THEN substr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx'))
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]' OR replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]ti' THEN 'rtx' || replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
			ELSE replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
		END, '') IS NOT NULL THEN 
		CASE
			WHEN NULLIF(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), '') IS NULL THEN NULL
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%b200%' THEN 'b200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h200%' THEN 'h200'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%h100%' THEN 'h100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a100%' THEN 'a100'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40s%' THEN 'l40s'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%l40%' THEN 'l40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a40%' THEN 'a40'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10g%' THEN 'a10g'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%a10%' THEN 'a10'
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') LIKE '%m2max%' THEN 'm2max'
			WHEN instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx') > 0 THEN substr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), instr(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', ''), 'rtx'))
			WHEN replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]' OR replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '') GLOB '[0-9][0-9][0-9][0-9]ti' THEN 'rtx' || replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
			ELSE replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(replace(lower(trim(coalesce(COALESCE(base.launch_resolved_gpu_name, base.launch_gpu_class), ''))), 'nvidia', ''), 'geforce', ''), 'tesla', ''), 'amd', ''), 'radeon', ''), 'instinct', ''), ' ', ''), '-', ''), '_', ''), '.', ''), '/', ''), '(', ''), ')', ''), '[', ''), ']', '')
		END
			END AS actual_gpu_class,
			base.host_hardware_last_updated,
			CAST(json_extract(base.job_metadata, '$.resource.peak_rss_kb') AS INTEGER) AS peak_rss_kb,
			CAST(json_extract(base.job_metadata, '$.resource.max_gpu_mem_mib') AS INTEGER) AS max_gpu_mem_mib,
			CAST(json_extract(base.job_metadata, '$.cpu.mean') AS REAL) AS cpu_mean,
			CASE WHEN base.setup_start > 0 AND base.setup_end > base.setup_start THEN base.setup_end - base.setup_start END AS setup_duration_s,
			CASE WHEN base.upload_start > 0 AND base.upload_end > base.upload_start THEN base.upload_end - base.upload_start END AS upload_duration_s,
			CASE WHEN base.wrapper_start > 0 AND base.start_time > base.wrapper_start THEN base.start_time - base.wrapper_start END AS wrapper_to_start_s
		FROM completed_runs base
		LEFT JOIN selected_gpu_summary gpu ON gpu.run_id = base.run_id
;
CREATE VIEW IF NOT EXISTS job_run_training_examples AS SELECT * FROM training_examples
;
