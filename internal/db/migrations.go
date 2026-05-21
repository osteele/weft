package db

import (
	"database/sql"
	"fmt"
)

// migration is a single schema-versioned change. Its position in
// versionedMigrations determines the version it bumps to: the entry at
// index i brings the DB from baseSchemaVersion+i to baseSchemaVersion+i+1.
//
// To add a new schema change, append a new migration entry — there is no
// separate currentSchemaVersion constant to update. This makes the
// "added a migration but forgot to bump the version" bug class
// structurally impossible going forward.
type migration struct {
	// Description is a short label used in pre-migration backup names
	// and migration log lines. Example: "add jobs.foo column".
	Description string
	// Apply runs the migration against a writable handle. Implementations
	// must be idempotent (use addColumnIfMissing, CREATE TABLE IF NOT
	// EXISTS, etc.) so reruns after a partial-failure recovery are safe.
	Apply func(*sql.DB) error
}

// baseSchemaVersion is the highest schema version produced by the legacy
// monolithic initSchema body. DBs created or migrated by binaries from
// before the versionedMigrations refactor report this as user_version when
// they're at the latest state the legacy code knew about. Version numbers
// for new migrations start at baseSchemaVersion+1.
const baseSchemaVersion = 11

// versionedMigrations is the canonical list of schema changes added since
// baseSchemaVersion. Append to this slice to introduce a new migration —
// currentSchemaVersion derives from its length, so the version bumps
// automatically.
//
// Each Apply must be idempotent (see migration.Apply).
var versionedMigrations = []migration{
	// Append new migrations here, e.g.:
	// {
	//     Description: "add jobs.foo column",
	//     Apply: func(db *sql.DB) error {
	//         return addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN foo TEXT`)
	//     },
	// },
	{
		Description: "add move_intents table",
		Apply: func(db *sql.DB) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS move_intents (
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
				)`,
				`CREATE INDEX IF NOT EXISTS idx_move_intents_job ON move_intents(job_id)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_move_intents_open ON move_intents(job_id) WHERE state = 'open'`,
				`CREATE INDEX IF NOT EXISTS idx_move_intents_target_launch ON move_intents(target_launch_id) WHERE state = 'open'`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add placement_intents table",
		Apply: func(db *sql.DB) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS placement_intents (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER NOT NULL REFERENCES jobs(id),
					operation TEXT NOT NULL,
					state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','confirmed','canceled')),
					created_at INTEGER NOT NULL,
					resolved_at INTEGER,
					resolution TEXT
				)`,
				`CREATE INDEX IF NOT EXISTS idx_placement_intents_job ON placement_intents(job_id)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_placement_intents_open ON placement_intents(job_id) WHERE state = 'open'`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add launches.bootstrap_deadline_unix and launches.agent_ready_at_unix",
		Apply: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE launches ADD COLUMN bootstrap_deadline_unix INTEGER`,
				`ALTER TABLE launches ADD COLUMN agent_ready_at_unix INTEGER`,
			} {
				if err := addColumnIfMissing(db, stmt); err != nil {
					return err
				}
			}
			// Backfill bootstrap_deadline_unix for existing rows so the
			// reconciler can read a single column unconditionally instead
			// of falling back to an inline launched_at + timeout
			// computation. 1200s = 20m matches campaign.BootstrapTerminateTimeout.
			if _, err := db.Exec(`UPDATE launches
				SET bootstrap_deadline_unix = COALESCE(launched_at, created_at) + 1200
				WHERE bootstrap_deadline_unix IS NULL`); err != nil {
				return err
			}
			return nil
		},
	},
	{
		Description: "retire job_attempts.pending_status='pending_placement'",
		Apply: func(db *sql.DB) error {
			// The in-flight placement signal lives on PlacementIntent
			// (specs/job-move.allium); pending_status='pending_placement'
			// is unused. Clear it on existing rows.
			_, err := db.Exec(`UPDATE job_attempts
				SET pending_status = NULL, pending_at = NULL
				WHERE pending_status = 'pending_placement'`)
			return err
		},
	},
	{
		Description: "add launch CPU metadata",
		Apply: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE launches ADD COLUMN cpu_cores_effective INTEGER`,
				`ALTER TABLE launches ADD COLUMN cpu_name TEXT`,
				`ALTER TABLE launches ADD COLUMN ram_gb INTEGER`,
			} {
				if err := addColumnIfMissing(db, stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add cloud_instances view aliasing launches",
		Apply: func(db *sql.DB) error {
			// `launches` is the storage table; `cloud_instances` is the
			// conceptual name used throughout the Go code (CloudInstance
			// struct, jobs.cloud_instance_id FK). Expose a read-only view
			// so ad-hoc SQL written against the conceptual name works.
			_, err := db.Exec(`CREATE VIEW IF NOT EXISTS cloud_instances AS SELECT * FROM launches`)
			return err
		},
	},
	{
		Description: "add execution_targets table",
		Apply: func(db *sql.DB) error {
			if err := addColumnIfMissing(db, `ALTER TABLE launches ADD COLUMN target_id INTEGER`); err != nil {
				return err
			}
			return initExecutionTargetsSchema(db)
		},
	},
	{
		Description: "add job_attempts.target_id column",
		Apply: func(db *sql.DB) error {
			if err := addColumnIfMissing(db, `ALTER TABLE job_attempts ADD COLUMN target_id INTEGER REFERENCES execution_targets(id)`); err != nil {
				return err
			}
			if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_job_attempts_target ON job_attempts(target_id)`); err != nil {
				return err
			}
			return BackfillJobAttemptExecutionTargets(db)
		},
	},
	{
		Description: "add jobs.priority column",
		Apply: func(db *sql.DB) error {
			return addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		Description: "recreate job_status view with priority",
		Apply: func(db *sql.DB) error {
			return createJobStatusView(db)
		},
	},
	{
		// Repair historical rows that violate TerminalJobsHaveEndTime
		// (specs/job-lifecycle.allium). Writers are fixed; this is one-off.
		Description: "stamp end_time on terminal-status attempts that are missing it",
		Apply: func(db *sql.DB) error {
			_, err := db.Exec(`
				UPDATE job_attempts
				   SET end_time = COALESCE(start_time, queued_at, strftime('%s','now'))
				 WHERE (end_time IS NULL OR end_time = 0)
				   AND status IN ('canceled','failed','dead','killed','completed')`)
			return err
		},
	},
	{
		Description: "add bootstrap_transitions table",
		Apply: func(db *sql.DB) error {
			return initBootstrapTransitionsSchema(db)
		},
	},
	{
		Description: "add launches.hedge_cohort_id column",
		Apply: func(db *sql.DB) error {
			if err := addColumnIfMissing(db, `ALTER TABLE launches ADD COLUMN hedge_cohort_id INTEGER`); err != nil {
				return err
			}
			_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_launches_hedge_cohort ON launches(hedge_cohort_id) WHERE hedge_cohort_id IS NOT NULL`)
			return err
		},
	},
	{
		Description: "drop launches.termination_reason CHECK constraint",
		Apply: func(db *sql.DB) error {
			return dropLaunchesTerminationReasonCheck(db)
		},
	},
	{
		Description: "relabel infra_failure rows on terminal-at-create campaigns to weft_bug",
		Apply: func(db *sql.DB) error {
			_, err := db.Exec(`
				UPDATE launches
				   SET termination_reason = 'weft_bug'
				 WHERE id IN (
					SELECT l.id FROM launches l
					  JOIN campaigns c ON c.id = l.campaign_id
					 WHERE c.status IN ('failed','canceled','completed')
					   AND c.ended_at IS NOT NULL
					   AND c.ended_at < l.created_at
					   AND l.termination_reason = 'infra_failure'
				 )`)
			return err
		},
	},
	{
		Description: "rebuild launches.termination_reason CHECK with weft_bug accepted",
		Apply: func(db *sql.DB) error {
			return ensureLaunchesTableConstraints(db)
		},
	},
	{
		Description: "add retry budget to move_intents",
		Apply: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE move_intents ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 1`,
				`ALTER TABLE move_intents ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 1`,
			} {
				if err := addColumnIfMissing(db, stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "record autopilot runner binary identity",
		Apply: func(db *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE autopilot_state ADD COLUMN active_binary_path TEXT`,
				`ALTER TABLE autopilot_state ADD COLUMN active_binary_size INTEGER`,
				`ALTER TABLE autopilot_state ADD COLUMN active_binary_mtime INTEGER`,
				`ALTER TABLE autopilot_state ADD COLUMN active_binary_dev INTEGER`,
				`ALTER TABLE autopilot_state ADD COLUMN active_binary_ino INTEGER`,
			} {
				if err := addColumnIfMissing(db, stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add placement row invariants",
		Apply: func(db *sql.DB) error {
			return ensurePlacementRowInvariants(db)
		},
	},
	{
		Description: "enforce unique open placement state",
		Apply: func(db *sql.DB) error {
			return ensurePlacementRowInvariants(db)
		},
	},
	{
		Description: "make execution target authoritative for placement reads",
		Apply: func(db *sql.DB) error {
			if err := createExecutionTargetShadowTriggers(db); err != nil {
				return err
			}
			if err := BackfillJobAttemptExecutionTargets(db); err != nil {
				return err
			}
			return createJobStatusView(db)
		},
	},
	{
		Description: "add structurally tracked open attempts",
		Apply: func(db *sql.DB) error {
			return ensureOpenAttemptState(db)
		},
	},
	{
		Description: "add scheduling telemetry tables",
		Apply: func(db *sql.DB) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS prediction_history (
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
				)`,
				`CREATE INDEX IF NOT EXISTS idx_prediction_history_job ON prediction_history(job_id, attempt_id)`,
				`CREATE INDEX IF NOT EXISTS idx_prediction_history_target ON prediction_history(target, created_at)`,
				`CREATE TABLE IF NOT EXISTS placement_decisions (
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
					details_json TEXT,
					created_at INTEGER NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS idx_placement_decisions_job ON placement_decisions(job_id, attempt_id)`,
				`CREATE INDEX IF NOT EXISTS idx_placement_decisions_campaign ON placement_decisions(campaign_id)`,
				`CREATE TABLE IF NOT EXISTS placement_candidates (
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
				)`,
				`CREATE INDEX IF NOT EXISTS idx_placement_candidates_decision ON placement_candidates(decision_id, rank)`,
				`CREATE TABLE IF NOT EXISTS donor_experiments (
					campaign_id INTEGER PRIMARY KEY REFERENCES campaigns(id) ON DELETE CASCADE,
					cohort TEXT NOT NULL,
					sample_rate REAL NOT NULL,
					donor_launch_id INTEGER REFERENCES launches(id),
					reason TEXT,
					assigned_at INTEGER NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS failure_label_reviews (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					job_id INTEGER REFERENCES jobs(id),
					attempt_id INTEGER REFERENCES job_attempts(id),
					heuristic_label TEXT,
					human_label TEXT,
					reviewer TEXT,
					notes TEXT,
					reviewed_at INTEGER
				)`,
				`CREATE INDEX IF NOT EXISTS idx_failure_label_reviews_attempt ON failure_label_reviews(job_id, attempt_id)`,
			}
			for _, stmt := range stmts {
				if _, err := db.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Description: "add placement decision details",
		Apply: func(db *sql.DB) error {
			return addColumnIfMissing(db, `ALTER TABLE placement_decisions ADD COLUMN details_json TEXT`)
		},
	},
	{
		Description: "add error diagnosis backfill marker",
		Apply: func(db *sql.DB) error {
			return addColumnIfMissing(db, `ALTER TABLE job_attempts ADD COLUMN error_diagnosis_backfilled INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		Description: "recreate launch job membership view",
		Apply: func(db *sql.DB) error {
			return createJobStateViews(db)
		},
	},
	{
		// Fallback metadata store for jobs submitted while unplaced (no
		// job_attempts row yet); SetJobMetadata writes here and the
		// job_status view COALESCEs jobs.job_metadata with the attempt
		// column. Must be a versioned migration: the job_status view is
		// recreated by startupRepair on every Open, so an existing DB at
		// the current schema version (which fast-paths past initSchema's
		// legacy body) needs the version bump to receive the column.
		Description: "add jobs.job_metadata fallback column",
		Apply: func(db *sql.DB) error {
			return addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN job_metadata TEXT`)
		},
	},
	{
		Description: "derive job status target kind from execution targets",
		Apply: func(db *sql.DB) error {
			if err := BackfillJobAttemptExecutionTargets(db); err != nil {
				return err
			}
			if err := ValidateExecutionTargetPlacement(db); err != nil {
				return err
			}
			return createJobStatusView(db)
		},
	},
	{
		// Structured launch/reuse breakdown for an unplaced job's blocker.
		// Must be versioned: the job_status view (recreated on every Open)
		// references j.placement_blocked, so existing DBs at the current
		// schema version need the column without re-running initSchema.
		Description: "add jobs.placement_blocked column",
		Apply: func(db *sql.DB) error {
			if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN placement_blocked TEXT`); err != nil {
				return err
			}
			return createJobStatusView(db)
		},
	},
}

// currentSchemaVersion is the version this binary expects on disk. Derived
// from versionedMigrations so it advances automatically when a migration
// is appended. This eliminates the "forgot to bump after adding a
// migration" bug class that bit us in commit 2a698ef.
var currentSchemaVersion = baseSchemaVersion + len(versionedMigrations)

// runVersionedMigrations applies every entry in migrations whose target
// version is greater than fromVersion, advancing PRAGMA user_version
// after each one so a partial failure leaves the DB at a well-defined
// intermediate version. The migrations slice is a parameter (rather than
// reading the package global directly) so tests can drive the loop with
// fake migrations.
func runVersionedMigrations(db *sql.DB, fromVersion int, migrations []migration) error {
	for i, m := range migrations {
		targetVersion := baseSchemaVersion + i + 1
		if targetVersion <= fromVersion {
			continue
		}
		if err := m.Apply(db); err != nil {
			return fmt.Errorf("migration v%d (%s): %w", targetVersion, m.Description, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, targetVersion)); err != nil {
			return fmt.Errorf("set user_version=%d: %w", targetVersion, err)
		}
	}
	return nil
}

func ensurePlacementRowInvariants(db *sql.DB) error {
	if err := normalizeAttemptEndTimes(db); err != nil {
		return err
	}
	if err := resolveDuplicateOpenIntents(db, "move_intents", string(MoveIntentStateObsoleted)); err != nil {
		return err
	}
	if err := resolveDuplicateOpenIntents(db, "placement_intents", string(PlacementIntentStateCanceled)); err != nil {
		return err
	}
	if err := resolveDuplicateOpenAttempts(db); err != nil {
		return err
	}
	if err := ensureOpenAttemptState(db); err != nil {
		return err
	}
	if err := createExecutionTargetShadowTriggers(db); err != nil {
		return err
	}
	stmts := []string{
		`DROP INDEX IF EXISTS idx_move_intents_open`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_move_intents_open ON move_intents(job_id) WHERE state = 'open'`,
		`DROP INDEX IF EXISTS idx_placement_intents_open`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_placement_intents_open ON placement_intents(job_id) WHERE state = 'open'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_job_attempts_one_open ON job_attempts(job_id) WHERE end_time IS NULL`,
		`CREATE TRIGGER IF NOT EXISTS move_intents_open_new_requires_live_source_insert
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS move_intents_open_new_requires_live_source_update
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_prevent_mixed_host_launch_insert
			BEFORE INSERT ON job_attempts
			FOR EACH ROW
			WHEN COALESCE(NEW.host, '') != '' AND NEW.launch_id IS NOT NULL
			     AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'rental:%'
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts cannot target both inventory host and launch');
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_prevent_mixed_host_launch_update
			BEFORE UPDATE OF host, launch_id ON job_attempts
			FOR EACH ROW
			WHEN COALESCE(NEW.host, '') != '' AND NEW.launch_id IS NOT NULL
			     AND COALESCE(NEW.host, '') NOT LIKE 'vastai:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'runpod:%'
			     AND COALESCE(NEW.host, '') NOT LIKE 'rental:%'
			BEGIN
				SELECT RAISE(ABORT, 'job_attempts cannot target both inventory host and launch');
			END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAttemptEndTimes(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE job_attempts
		   SET end_time = NULL
		 WHERE end_time = 0
		   AND status NOT IN ('canceled','failed','dead','killed','completed')`)
	return err
}

func createExecutionTargetShadowTriggers(db *sql.DB) error {
	for _, name := range []string{
		"job_attempts_sync_target_shadow_insert",
		"job_attempts_sync_target_shadow_update",
		"job_attempts_reject_target_shadow_mismatch_insert",
		"job_attempts_reject_target_shadow_mismatch_update",
	} {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return err
		}
	}
	stmts := []string{
		`CREATE TRIGGER IF NOT EXISTS job_attempts_sync_target_shadow_insert
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_sync_target_shadow_update
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_reject_target_shadow_mismatch_insert
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_reject_target_shadow_mismatch_update
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
			END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func resolveDuplicateOpenIntents(db *sql.DB, tableName, resolvedState string) error {
	_, err := db.Exec(fmt.Sprintf(`
		WITH ranked AS (
			SELECT id,
			       ROW_NUMBER() OVER (
			       	PARTITION BY job_id
			       	ORDER BY created_at DESC, id DESC
			       ) AS rn
			  FROM %s
			 WHERE state = 'open'
		)
		UPDATE %s
		   SET state = ?,
		       resolved_at = COALESCE(resolved_at, CAST(strftime('%%s','now') AS INTEGER)),
		       resolution = COALESCE(NULLIF(resolution, ''), 'duplicate open intent auto-resolved')
		 WHERE id IN (SELECT id FROM ranked WHERE rn > 1)`, tableName, tableName), resolvedState)
	return err
}

func resolveDuplicateOpenAttempts(db *sql.DB) error {
	_, err := db.Exec(`
		WITH ranked AS (
			SELECT id,
			       ROW_NUMBER() OVER (
			       	PARTITION BY job_id
			       	ORDER BY attempt_number DESC, id DESC
			       ) AS rn
			  FROM job_attempts
			 WHERE end_time IS NULL
		)
		UPDATE job_attempts
		   SET end_time = CAST(strftime('%s','now') AS INTEGER),
		       pending_status = NULL,
		       pending_at = NULL,
		       cloud_outcome = CASE
		       	WHEN launch_id IS NOT NULL THEN COALESCE(cloud_outcome, 'superseded')
		       	ELSE cloud_outcome
		       END
		 WHERE id IN (SELECT id FROM ranked WHERE rn > 1)`)
	return err
}

func ensureOpenAttemptState(db *sql.DB) error {
	if err := normalizeAttemptEndTimes(db); err != nil {
		return err
	}
	if err := resolveDuplicateOpenAttempts(db); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS job_open_attempts (
			job_id INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
			attempt_id INTEGER NOT NULL UNIQUE REFERENCES job_attempts(id) ON DELETE CASCADE
		)`,
		`CREATE TRIGGER IF NOT EXISTS job_open_attempts_validate_insert
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_open_attempts_validate_update
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
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_insert
			AFTER INSERT ON job_attempts
			FOR EACH ROW
			WHEN NEW.end_time IS NULL
			BEGIN
				INSERT INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_close_or_move
			AFTER UPDATE OF job_id, end_time ON job_attempts
			FOR EACH ROW
			WHEN OLD.end_time IS NULL
			     AND (NEW.end_time IS NOT NULL OR NEW.job_id != OLD.job_id)
			BEGIN
				DELETE FROM job_open_attempts WHERE attempt_id = OLD.id;
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_open_or_move
			AFTER UPDATE OF job_id, end_time ON job_attempts
			FOR EACH ROW
			WHEN NEW.end_time IS NULL
			     AND (OLD.end_time IS NOT NULL OR NEW.job_id != OLD.job_id)
			BEGIN
				INSERT INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
			END`,
		`CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_delete
			AFTER DELETE ON job_attempts
			FOR EACH ROW
			WHEN OLD.end_time IS NULL
			BEGIN
				DELETE FROM job_open_attempts WHERE attempt_id = OLD.id;
			END`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_job_attempts_one_open ON job_attempts(job_id) WHERE end_time IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_job_open_attempts_attempt ON job_open_attempts(attempt_id)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`DELETE FROM job_open_attempts`); err != nil {
		return err
	}
	_, err := db.Exec(`
		INSERT INTO job_open_attempts(job_id, attempt_id)
		SELECT job_id, id
		  FROM job_attempts
		 WHERE end_time IS NULL`)
	return err
}
