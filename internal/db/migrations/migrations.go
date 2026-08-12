// Package migrations owns weft's database schema evolution. It uses goose
// (github.com/pressly/goose) as the migration runner.
//
// History before this package was a hand-rolled two-tier system (a monolithic
// idempotent initSchema body plus an append-only versionedMigrations list).
// That history has been squashed: the entire schema as of the squash is the
// single v1 baseline below, applied as a goose Go migration. New schema
// changes are added as ordinary goose SQL files under sql/ (NNNNN_name.sql).
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

// baselineSchema is the complete schema as of the squash — every table,
// index, trigger and view. Extracted verbatim from the live database with
// `sqlite3 .schema`, then normalized so every statement is idempotent
// (CREATE ... IF NOT EXISTS). It is therefore safe to apply to the one
// pre-goose database, where every statement is a no-op.
//
//go:embed baseline_schema.sql
var baselineSchema string

// sqlMigrations holds versioned .sql migrations (v2 onward). goose scans it
// for files matching NNNNN_name.sql; non-matching files (README.md) are
// ignored.
//
//go:embed sql
var sqlMigrations embed.FS

// newProvider builds a goose provider over weft's schema: the squashed v1
// baseline (a Go migration) plus any versioned .sql migrations under sql/.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(sqlMigrations, "sql")
	if err != nil {
		return nil, fmt.Errorf("migrations sub-fs: %w", err)
	}
	baseline := goose.NewGoMigration(
		1,
		&goose.GoFunc{RunDB: applyBaseline},
		&goose.GoFunc{RunDB: dropBaseline},
	)
	// Go-only at version 9: the squashed pre-goose baseline already contains
	// this column, so the one-time pre-goose upgrade path needs a conditional
	// ALTER TABLE.
	addProbeSeen := goose.NewGoMigration(
		9,
		&goose.GoFunc{RunDB: applyAddOnStartProbeSeenColumn},
		&goose.GoFunc{RunDB: dropAddOnStartProbeSeenColumn},
	)
	addAbandonedAttempts := goose.NewGoMigration(
		18,
		&goose.GoFunc{RunDB: applyAddAbandonedAttemptFields},
		&goose.GoFunc{RunDB: dropAddAbandonedAttemptFields},
	)
	addMoveIntentTargetHost := goose.NewGoMigration(
		19,
		&goose.GoFunc{RunDB: applyAddMoveIntentTargetHost},
		&goose.GoFunc{RunDB: dropAddMoveIntentTargetHost},
	)
	addMoveTargetAttempts := goose.NewGoMigration(
		20,
		&goose.GoFunc{RunDB: applyAddMoveTargetAttempts},
		&goose.GoFunc{RunDB: dropAddMoveTargetAttempts},
	)
	addMoveIntentTargetRequest := goose.NewGoMigration(
		21,
		&goose.GoFunc{RunDB: applyAddMoveIntentTargetRequest},
		&goose.GoFunc{RunDB: dropAddMoveIntentTargetRequest},
	)
	repairMoveIntentLaunchConfirmTrigger := goose.NewGoMigration(
		22,
		&goose.GoFunc{RunDB: applyRepairMoveIntentLaunchConfirmTrigger},
		&goose.GoFunc{RunDB: dropRepairMoveIntentLaunchConfirmTrigger},
	)
	// Go-only: a column add must guard with columnExists so re-applying the
	// latest migration (e.g. the pre-migration-backup upgrade path) does not
	// fail on a duplicate column.
	addResultsVerifyDetail := goose.NewGoMigration(
		23,
		&goose.GoFunc{RunDB: applyAddResultsVerifyDetailColumn},
		&goose.GoFunc{RunDB: dropAddResultsVerifyDetailColumn},
	)
	addCampaignMachineAntiAffinity := goose.NewGoMigration(
		24,
		&goose.GoFunc{RunDB: applyAddCampaignMachineAntiAffinity},
		&goose.GoFunc{RunDB: dropAddCampaignMachineAntiAffinity},
	)
	repairCampaignAffinityMachines := goose.NewGoMigration(
		26,
		&goose.GoFunc{RunDB: applyRepairCampaignAffinityMachinesColumn},
		&goose.GoFunc{RunDB: dropRepairCampaignAffinityMachinesColumn},
	)
	addLaunchRunpodCloudType := goose.NewGoMigration(
		27,
		&goose.GoFunc{RunDB: applyAddLaunchRunpodCloudTypeColumn},
		&goose.GoFunc{RunDB: dropAddLaunchRunpodCloudTypeColumn},
	)
	addCheckpointAssetMetadata := goose.NewGoMigration(
		28,
		&goose.GoFunc{RunDB: applyAddCheckpointAssetMetadataColumns},
		&goose.GoFunc{RunDB: dropAddCheckpointAssetMetadataColumns},
	)
	addJobSubmitToken := goose.NewGoMigration(
		30,
		&goose.GoFunc{RunDB: applyAddJobSubmitToken},
		&goose.GoFunc{RunDB: dropAddJobSubmitToken},
	)
	repairCloudStartingJobStatus := goose.NewGoMigration(
		31,
		&goose.GoFunc{RunDB: applyRepairCloudStartingJobStatus},
		&goose.GoFunc{RunDB: dropRepairCloudStartingJobStatus},
	)
	optimizeJobStatusLatestAttempt := goose.NewGoMigration(
		32,
		&goose.GoFunc{RunDB: applyOptimizeJobStatusLatestAttempt},
		&goose.GoFunc{RunDB: dropOptimizeJobStatusLatestAttempt},
	)
	addExternalSyncWarning := goose.NewGoMigration(
		35,
		&goose.GoFunc{RunDB: applyAddExternalSyncWarningColumns},
		&goose.GoFunc{RunDB: dropAddExternalSyncWarningColumns},
	)
	dropSpeculativeHostRegistry := goose.NewGoMigration(
		37,
		&goose.GoFunc{RunDB: applyDropSpeculativeHostRegistry},
		&goose.GoFunc{RunDB: undoDropSpeculativeHostRegistry},
	)
	addLaunchDriverVersion := goose.NewGoMigration(
		39,
		&goose.GoFunc{RunDB: applyAddLaunchDriverVersionColumn},
		&goose.GoFunc{RunDB: dropAddLaunchDriverVersionColumn},
	)
	addAgentProtocol := goose.NewGoMigration(
		40,
		&goose.GoFunc{RunDB: applyAddAgentProtocolColumn},
		&goose.GoFunc{RunDB: dropAddAgentProtocolColumn},
	)
	addLaunchNVLinkBandwidth := goose.NewGoMigration(
		43,
		&goose.GoFunc{RunDB: applyAddLaunchNVLinkBandwidthColumn},
		&goose.GoFunc{RunDB: dropAddLaunchNVLinkBandwidthColumn},
	)
	addJobProgressChangedAt := goose.NewGoMigration(
		44,
		&goose.GoFunc{RunDB: applyAddJobProgressChangedAtColumn},
		&goose.GoFunc{RunDB: dropAddJobProgressChangedAtColumn},
	)
	return goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sub,
		goose.WithGoMigrations(baseline, addProbeSeen, addAbandonedAttempts, addMoveIntentTargetHost, addMoveTargetAttempts, addMoveIntentTargetRequest, repairMoveIntentLaunchConfirmTrigger, addResultsVerifyDetail, addCampaignMachineAntiAffinity, repairCampaignAffinityMachines, addLaunchRunpodCloudType, addCheckpointAssetMetadata, addJobSubmitToken, repairCloudStartingJobStatus, optimizeJobStatusLatestAttempt, addExternalSyncWarning, dropSpeculativeHostRegistry, addLaunchDriverVersion, addAgentProtocol, addLaunchNVLinkBandwidth, addJobProgressChangedAt),
		goose.WithDisableGlobalRegistry(true),
	)
}

// applyAddOnStartProbeSeenColumn adds launches.first_onstart_probe_seen_unix
// and its partial index. SQLite has no ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS, so we ask pragma_table_info first and skip the ALTER if the column
// is already there (true for any DB that picked up the column from the
// baseline). This keeps the one existing pre-goose upgrade path harmless while
// goose records version 9.
func applyAddOnStartProbeSeenColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "first_onstart_probe_seen_unix")
	if err != nil {
		return fmt.Errorf("inspect launches columns: %w", err)
	}
	if !exists {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE launches ADD COLUMN first_onstart_probe_seen_unix INTEGER`,
		); err != nil {
			return fmt.Errorf("add first_onstart_probe_seen_unix column: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_launches_first_onstart_probe_seen
		    ON launches(first_onstart_probe_seen_unix)
		    WHERE first_onstart_probe_seen_unix IS NOT NULL`,
	); err != nil {
		return fmt.Errorf("create first_onstart_probe_seen index: %w", err)
	}
	return nil
}

func dropAddOnStartProbeSeenColumn(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`DROP INDEX IF EXISTS idx_launches_first_onstart_probe_seen`,
	); err != nil {
		return err
	}
	// SQLite ALTER TABLE DROP COLUMN exists since 3.35 but is brittle in
	// the presence of indexes and FKs. Down is best-effort; the index
	// drop above is the load-bearing part for a clean re-up.
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN first_onstart_probe_seen_unix`)
	return nil
}

func applyAddAbandonedAttemptFields(ctx context.Context, db *sql.DB) error {
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{"abandoned_at", `ALTER TABLE job_attempts ADD COLUMN abandoned_at INTEGER`},
		{"abandoned_reason", `ALTER TABLE job_attempts ADD COLUMN abandoned_reason TEXT`},
		{"abandoned_by_intent_id", `ALTER TABLE job_attempts ADD COLUMN abandoned_by_intent_id INTEGER REFERENCES move_intents(id)`},
	} {
		exists, err := columnExists(ctx, db, "job_attempts", col.name)
		if err != nil {
			return fmt.Errorf("inspect job_attempts.%s: %w", col.name, err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("add job_attempts.%s: %w", col.name, err)
			}
		}
	}
	sqlBytes, err := fs.ReadFile(sqlMigrations, "sql/abandoned_attempts_v18.sql")
	if err != nil {
		return fmt.Errorf("read abandoned-attempt migration SQL: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("apply abandoned-attempt migration SQL: %w", err)
	}
	return nil
}

func dropAddAbandonedAttemptFields(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`DROP VIEW IF EXISTS job_status`,
		`DROP VIEW IF EXISTS authoritative_job_attempts`,
		`DROP INDEX IF EXISTS idx_job_attempts_authoritative`,
		`ALTER TABLE job_attempts DROP COLUMN abandoned_by_intent_id`,
		`ALTER TABLE job_attempts DROP COLUMN abandoned_reason`,
		`ALTER TABLE job_attempts DROP COLUMN abandoned_at`,
	} {
		_, _ = db.ExecContext(ctx, stmt)
	}
	return nil
}

// applyAddResultsVerifyDetailColumn adds launches.results_verify_detail, a
// reason code recorded when results_verified is set false so the display can
// distinguish an upload failure from a completion manifest that omits a job.
func applyAddResultsVerifyDetailColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "results_verify_detail")
	if err != nil {
		return fmt.Errorf("inspect launches.results_verify_detail: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launches ADD COLUMN results_verify_detail TEXT`,
	); err != nil {
		return fmt.Errorf("add launches.results_verify_detail: %w", err)
	}
	return nil
}

func dropAddResultsVerifyDetailColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN results_verify_detail`)
	return nil
}

func applyAddLaunchRunpodCloudTypeColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "runpod_cloud_type")
	if err != nil {
		return fmt.Errorf("inspect launches.runpod_cloud_type: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launches ADD COLUMN runpod_cloud_type TEXT`,
	); err != nil {
		return fmt.Errorf("add launches.runpod_cloud_type: %w", err)
	}
	return nil
}

func dropAddLaunchRunpodCloudTypeColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN runpod_cloud_type`)
	return nil
}

// applyAddLaunchDriverVersionColumn adds launches.driver_version: the offer's
// NVIDIA driver version (dotted string; "" = unknown), so reuse admission can
// re-check a job's driver-major floor against the instance it would share.
func applyAddLaunchDriverVersionColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "driver_version")
	if err != nil {
		return fmt.Errorf("inspect launches.driver_version: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launches ADD COLUMN driver_version TEXT NOT NULL DEFAULT ''`,
	); err != nil {
		return fmt.Errorf("add launches.driver_version: %w", err)
	}
	return nil
}

func dropAddLaunchDriverVersionColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN driver_version`)
	return nil
}

// applyAddLaunchNVLinkBandwidthColumn preserves provider-measured topology
// evidence for reuse. NULL is unknown; zero is a reported absence.
func applyAddLaunchNVLinkBandwidthColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "nvlink_bandwidth")
	if err != nil {
		return fmt.Errorf("inspect launches.nvlink_bandwidth: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launches ADD COLUMN nvlink_bandwidth REAL`,
	); err != nil {
		return fmt.Errorf("add launches.nvlink_bandwidth: %w", err)
	}
	return nil
}

func dropAddLaunchNVLinkBandwidthColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN nvlink_bandwidth`)
	return nil
}

func applyAddJobProgressChangedAtColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launch_live_state", "job_progress_changed_at")
	if err != nil {
		return fmt.Errorf("inspect launch_live_state.job_progress_changed_at: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launch_live_state ADD COLUMN job_progress_changed_at INTEGER`,
	); err != nil {
		return fmt.Errorf("add launch_live_state.job_progress_changed_at: %w", err)
	}
	return nil
}

func dropAddJobProgressChangedAtColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launch_live_state DROP COLUMN job_progress_changed_at`)
	return nil
}

func applyAddAgentProtocolColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launch_live_state", "agent_protocol")
	if err != nil {
		return fmt.Errorf("inspect launch_live_state.agent_protocol: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE launch_live_state ADD COLUMN agent_protocol INTEGER NOT NULL DEFAULT 0`,
	); err != nil {
		return fmt.Errorf("add launch_live_state.agent_protocol: %w", err)
	}
	return nil
}

func dropAddAgentProtocolColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launch_live_state DROP COLUMN agent_protocol`)
	return nil
}

func applyAddCheckpointAssetMetadataColumns(ctx context.Context, db *sql.DB) error {
	cols := []struct {
		table string
		name  string
		ddl   string
	}{
		{"host_data", "content_hash", `ALTER TABLE host_data ADD COLUMN content_hash TEXT DEFAULT ''`},
		{"host_data", "content_type", `ALTER TABLE host_data ADD COLUMN content_type TEXT DEFAULT ''`},
		{"named_assets", "content_type", `ALTER TABLE named_assets ADD COLUMN content_type TEXT NOT NULL DEFAULT 'file'`},
	}
	for _, col := range cols {
		exists, err := columnExists(ctx, db, col.table, col.name)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", col.table, col.name, err)
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, col.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
		}
	}
	return nil
}

func dropAddCheckpointAssetMetadataColumns(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE named_assets DROP COLUMN content_type`)
	_, _ = db.ExecContext(ctx, `ALTER TABLE host_data DROP COLUMN content_type`)
	_, _ = db.ExecContext(ctx, `ALTER TABLE host_data DROP COLUMN content_hash`)
	return nil
}

func applyAddJobSubmitToken(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "jobs", "submit_token")
	if err != nil {
		return fmt.Errorf("inspect jobs.submit_token: %w", err)
	}
	if !exists {
		if _, err := db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN submit_token TEXT`); err != nil {
			return fmt.Errorf("add jobs.submit_token: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_submit_token ON jobs(submit_token) WHERE submit_token IS NOT NULL AND submit_token != ''`); err != nil {
		return fmt.Errorf("create jobs submit_token index: %w", err)
	}
	return nil
}

func dropAddJobSubmitToken(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_jobs_submit_token`)
	_, _ = db.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN submit_token`)
	return nil
}

func applyAddCampaignMachineAntiAffinity(ctx context.Context, db *sql.DB) error {
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{"distinct_machines", `ALTER TABLE campaigns ADD COLUMN distinct_machines INTEGER NOT NULL DEFAULT 0`},
		{"avoid_machines", `ALTER TABLE campaigns ADD COLUMN avoid_machines TEXT`},
		{"affinity_machines", `ALTER TABLE campaigns ADD COLUMN affinity_machines TEXT`},
	} {
		exists, err := columnExists(ctx, db, "campaigns", col.name)
		if err != nil {
			return fmt.Errorf("inspect campaigns.%s: %w", col.name, err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("add campaigns.%s: %w", col.name, err)
			}
		}
	}
	return nil
}

func dropAddCampaignMachineAntiAffinity(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE campaigns DROP COLUMN affinity_machines`,
		`ALTER TABLE campaigns DROP COLUMN avoid_machines`,
		`ALTER TABLE campaigns DROP COLUMN distinct_machines`,
	} {
		_, _ = db.ExecContext(ctx, stmt)
	}
	return nil
}

func applyRepairCampaignAffinityMachinesColumn(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "campaigns", "affinity_machines")
	if err != nil {
		return fmt.Errorf("inspect campaigns.affinity_machines: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE campaigns ADD COLUMN affinity_machines TEXT`); err != nil {
		return fmt.Errorf("add campaigns.affinity_machines: %w", err)
	}
	return nil
}

func dropRepairCampaignAffinityMachinesColumn(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE campaigns DROP COLUMN affinity_machines`)
	return nil
}

func applyAddMoveIntentTargetHost(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "move_intents", "target_host")
	if err != nil {
		return fmt.Errorf("inspect move_intents.target_host: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE move_intents ADD COLUMN target_host TEXT`); err != nil {
		return fmt.Errorf("add move_intents.target_host: %w", err)
	}
	return nil
}

func dropAddMoveIntentTargetHost(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE move_intents DROP COLUMN target_host`)
	return nil
}

func applyAddMoveTargetAttempts(ctx context.Context, db *sql.DB) error {
	for _, col := range []struct {
		table string
		name  string
		ddl   string
	}{
		{"move_intents", "target_attempt_id", `ALTER TABLE move_intents ADD COLUMN target_attempt_id INTEGER REFERENCES job_attempts(id)`},
		{"job_attempts", "move_intent_id", `ALTER TABLE job_attempts ADD COLUMN move_intent_id INTEGER REFERENCES move_intents(id)`},
	} {
		exists, err := columnExists(ctx, db, col.table, col.name)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", col.table, col.name, err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, moveTargetAttemptsSQL); err != nil {
		return fmt.Errorf("apply move target attempts schema: %w", err)
	}
	if err := recreateAuthoritativeJobStatusView(ctx, db); err != nil {
		return fmt.Errorf("recreate authoritative job status view: %w", err)
	}
	return nil
}

func dropAddMoveTargetAttempts(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE move_intents DROP COLUMN target_attempt_id`,
		`ALTER TABLE job_attempts DROP COLUMN move_intent_id`,
	} {
		_, _ = db.ExecContext(ctx, stmt)
	}
	return nil
}

func applyAddMoveIntentTargetRequest(ctx context.Context, db *sql.DB) error {
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{"target_request_id", `ALTER TABLE move_intents ADD COLUMN target_request_id TEXT`},
		{"target_request_kind", `ALTER TABLE move_intents ADD COLUMN target_request_kind TEXT`},
		{"target_request_created_at", `ALTER TABLE move_intents ADD COLUMN target_request_created_at INTEGER`},
	} {
		exists, err := columnExists(ctx, db, "move_intents", col.name)
		if err != nil {
			return fmt.Errorf("inspect move_intents.%s: %w", col.name, err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("add move_intents.%s: %w", col.name, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_move_intents_target_request ON move_intents(target_launch_id, target_request_id) WHERE state = 'open' AND target_request_id IS NOT NULL`); err != nil {
		return fmt.Errorf("index move_intents target request: %w", err)
	}
	return nil
}

func dropAddMoveIntentTargetRequest(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_move_intents_target_request`)
	for _, stmt := range []string{
		`ALTER TABLE move_intents DROP COLUMN target_request_created_at`,
		`ALTER TABLE move_intents DROP COLUMN target_request_kind`,
		`ALTER TABLE move_intents DROP COLUMN target_request_id`,
	} {
		_, _ = db.ExecContext(ctx, stmt)
	}
	return nil
}

func applyRepairMoveIntentLaunchConfirmTrigger(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS intents_auto_confirm_on_launch_becomes_live`,
		`CREATE TRIGGER IF NOT EXISTS intents_auto_confirm_on_launch_becomes_live
AFTER UPDATE OF status ON launches
WHEN NEW.status IN ('running','grace','completed')
  AND (OLD.status IS NULL OR OLD.status != NEW.status)
BEGIN
    UPDATE placement_intents
       SET state = 'confirmed',
           resolved_at = strftime('%s','now'),
           resolution = 'auto-canceled by launch ' || NEW.id || ' becoming ' || NEW.status
     WHERE state = 'open'
       AND EXISTS (
           SELECT 1 FROM job_attempts ja
            WHERE ja.job_id = placement_intents.job_id AND ja.launch_id = NEW.id
       );
END`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("repair move-intent launch confirmation trigger: %w", err)
		}
	}
	return nil
}

func dropRepairMoveIntentLaunchConfirmTrigger(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS intents_auto_confirm_on_launch_becomes_live`)
	return nil
}

const moveTargetAttemptsSQL = `
DROP INDEX IF EXISTS idx_job_attempts_one_open;
CREATE INDEX IF NOT EXISTS idx_job_attempts_open_by_job ON job_attempts(job_id, attempt_number DESC) WHERE end_time IS NULL;
CREATE INDEX IF NOT EXISTS idx_job_attempts_move_intent ON job_attempts(move_intent_id) WHERE move_intent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_move_intents_target_attempt ON move_intents(target_attempt_id) WHERE target_attempt_id IS NOT NULL;

DROP TRIGGER IF EXISTS job_open_attempts_validate_insert;
DROP TRIGGER IF EXISTS job_open_attempts_validate_update;
DROP TRIGGER IF EXISTS job_attempts_open_state_insert;
DROP TRIGGER IF EXISTS job_attempts_open_state_close_or_move;
DROP TRIGGER IF EXISTS job_attempts_open_state_open_or_move;
DROP TRIGGER IF EXISTS job_attempts_open_state_delete;
DROP TRIGGER IF EXISTS job_attempts_terminal_auto_stamp_end_time_insert;
DROP TRIGGER IF EXISTS job_attempts_terminal_auto_stamp_end_time_update;

CREATE TABLE IF NOT EXISTS job_open_attempts_v20 (
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	attempt_id INTEGER NOT NULL PRIMARY KEY REFERENCES job_attempts(id) ON DELETE CASCADE
);
INSERT OR IGNORE INTO job_open_attempts_v20(job_id, attempt_id)
	SELECT job_id, attempt_id FROM job_open_attempts;
DROP TABLE IF EXISTS job_open_attempts;
ALTER TABLE job_open_attempts_v20 RENAME TO job_open_attempts;
CREATE INDEX IF NOT EXISTS idx_job_open_attempts_job ON job_open_attempts(job_id);
CREATE INDEX IF NOT EXISTS idx_job_open_attempts_attempt ON job_open_attempts(attempt_id);

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
	INSERT OR IGNORE INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
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
	INSERT OR IGNORE INTO job_open_attempts(job_id, attempt_id) VALUES (NEW.job_id, NEW.id);
END;

CREATE TRIGGER IF NOT EXISTS job_attempts_open_state_delete
AFTER DELETE ON job_attempts
FOR EACH ROW
WHEN OLD.end_time IS NULL
BEGIN
	DELETE FROM job_open_attempts WHERE attempt_id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS job_attempts_terminal_auto_stamp_end_time_insert
AFTER INSERT ON job_attempts
WHEN NEW.status IN ('completed','failed','dead','killed','canceled')
  AND (NEW.end_time IS NULL OR NEW.end_time = 0)
  AND NOT (NEW.launch_id IS NOT NULL AND NEW.last_synced_status IS NULL)
BEGIN
	UPDATE job_attempts
	   SET end_time = strftime('%s','now')
	 WHERE id = NEW.id;
	DELETE FROM job_open_attempts WHERE attempt_id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS job_attempts_terminal_auto_stamp_end_time_update
AFTER UPDATE OF status ON job_attempts
WHEN NEW.status IN ('completed','failed','dead','killed','canceled')
  AND OLD.status != NEW.status
  AND (NEW.end_time IS NULL OR NEW.end_time = 0)
  AND NOT (NEW.launch_id IS NOT NULL AND NEW.last_synced_status IS NULL)
BEGIN
	UPDATE job_attempts
	   SET end_time = strftime('%s','now')
	 WHERE id = NEW.id;
	DELETE FROM job_open_attempts WHERE attempt_id = NEW.id;
END;

DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_insert;
DROP TRIGGER IF EXISTS move_intents_auto_confirm_on_attempt_launch_update;
DROP TRIGGER IF EXISTS move_intents_auto_obsolete_on_source_attempt_end;
DROP TRIGGER IF EXISTS move_intents_auto_obsolete_on_source_attempt_status_terminal;

CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_insert
AFTER INSERT ON job_attempts
WHEN NEW.launch_id IS NOT NULL
 AND NEW.move_intent_id IS NULL
 AND EXISTS (
	SELECT 1 FROM launches l
	 WHERE l.id = NEW.launch_id
	   AND l.status IN ('planned','launching','running','paused','grace','completed')
 )
BEGIN
	UPDATE move_intents
	   SET state = 'confirmed',
	       resolved_at = strftime('%s','now'),
	       resolution = 'auto-confirmed by job_attempts insert on live launch ' || NEW.launch_id,
	       target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
	 WHERE job_id = NEW.job_id
	   AND state = 'open'
	   AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;

CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_attempt_launch_update
AFTER UPDATE OF launch_id ON job_attempts
WHEN NEW.launch_id IS NOT NULL
 AND NEW.move_intent_id IS NULL
 AND (OLD.launch_id IS NULL OR OLD.launch_id != NEW.launch_id)
 AND EXISTS (
	SELECT 1 FROM launches l
	 WHERE l.id = NEW.launch_id
	   AND l.status IN ('planned','launching','running','paused','grace','completed')
 )
BEGIN
	UPDATE move_intents
	   SET state = 'confirmed',
	       resolved_at = strftime('%s','now'),
	       resolution = 'auto-confirmed by job_attempts.launch_id update to live launch ' || NEW.launch_id,
	       target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
	 WHERE job_id = NEW.job_id
	   AND state = 'open'
	   AND (target_launch_id IS NULL OR target_launch_id = NEW.launch_id);
END;

CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_end
AFTER UPDATE OF end_time ON job_attempts
WHEN NEW.end_time IS NOT NULL
 AND OLD.end_time IS NULL
 AND NEW.status IN ('completed','failed')
BEGIN
	UPDATE job_attempts
	   SET abandoned_at = COALESCE(abandoned_at, strftime('%s','now')),
	       abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), 'move_source_won'),
	       abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, (
	           SELECT id FROM move_intents
	            WHERE source_attempt_id = NEW.id
	              AND state = 'open'
	            LIMIT 1
	       ))
	 WHERE id IN (
	       SELECT target_attempt_id FROM move_intents
	        WHERE source_attempt_id = NEW.id
	          AND state = 'open'
	          AND target_attempt_id IS NOT NULL
	 );
	UPDATE move_intents
	   SET state = 'obsoleted',
	       resolved_at = strftime('%s','now'),
	       resolution = 'source attempt finished before target won (status=' || NEW.status || ')'
	 WHERE source_attempt_id = NEW.id
	   AND state = 'open';
END;

CREATE TRIGGER IF NOT EXISTS move_intents_auto_obsolete_on_source_attempt_status_terminal
AFTER UPDATE OF status ON job_attempts
WHEN NEW.status IN ('completed','failed')
 AND OLD.status != NEW.status
 AND NEW.end_time IS NOT NULL
BEGIN
	UPDATE job_attempts
	   SET abandoned_at = COALESCE(abandoned_at, strftime('%s','now')),
	       abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), 'move_source_won'),
	       abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, (
	           SELECT id FROM move_intents
	            WHERE source_attempt_id = NEW.id
	              AND state = 'open'
	            LIMIT 1
	       ))
	 WHERE id IN (
	       SELECT target_attempt_id FROM move_intents
	        WHERE source_attempt_id = NEW.id
	          AND state = 'open'
	          AND target_attempt_id IS NOT NULL
	 );
	UPDATE move_intents
	   SET state = 'obsoleted',
	       resolved_at = strftime('%s','now'),
	       resolution = 'source attempt finished before target won (status=' || NEW.status || ')'
	 WHERE source_attempt_id = NEW.id
	   AND state = 'open';
END;

CREATE TRIGGER IF NOT EXISTS move_intents_auto_confirm_on_target_attempt_end
AFTER UPDATE OF end_time ON job_attempts
WHEN NEW.end_time IS NOT NULL
 AND OLD.end_time IS NULL
 AND NEW.status IN ('completed','failed')
 AND NEW.move_intent_id IS NOT NULL
BEGIN
	UPDATE job_attempts
	   SET abandoned_at = COALESCE(abandoned_at, strftime('%s','now')),
	       abandoned_reason = COALESCE(NULLIF(abandoned_reason, ''), 'move_target_won'),
	       abandoned_by_intent_id = COALESCE(abandoned_by_intent_id, NEW.move_intent_id)
	 WHERE id IN (
	       SELECT source_attempt_id FROM move_intents
	        WHERE id = NEW.move_intent_id
	          AND state = 'open'
	          AND source_attempt_id IS NOT NULL
	 );
	UPDATE move_intents
	   SET state = 'confirmed',
	       resolved_at = strftime('%s','now'),
	       resolution = 'target attempt finished first (status=' || NEW.status || ')',
	       target_attempt_id = COALESCE(target_attempt_id, NEW.id),
	       target_launch_id = COALESCE(target_launch_id, NEW.launch_id)
	 WHERE id = NEW.move_intent_id
	   AND state = 'open';
END;
`

func recreateAuthoritativeJobStatusView(ctx context.Context, db *sql.DB) error {
	sqlBytes, err := fs.ReadFile(sqlMigrations, "sql/current_authoritative_views.sql")
	if err != nil {
		return fmt.Errorf("read current authoritative view SQL: %w", err)
	}
	for _, stmt := range []string{
		`DROP VIEW IF EXISTS job_status`,
		`DROP VIEW IF EXISTS authoritative_job_attempts`,
		string(sqlBytes),
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func applyRepairCloudStartingJobStatus(ctx context.Context, db *sql.DB) error {
	return recreateAuthoritativeJobStatusView(ctx, db)
}

func dropRepairCloudStartingJobStatus(context.Context, *sql.DB) error {
	return nil
}

// applyOptimizeJobStatusLatestAttempt recreates job_status with the
// latest-attempt lookup rewritten from a ROW_NUMBER() window CTE to an
// index-probed scalar subquery (idx_job_attempts_authoritative). The window
// form forced SQLite to materialize the full attempt history for every query
// against the view, so single-job lookups cost O(total attempts) and joins
// against the view multiplied that per outer row (autopilot passes spent
// minutes in ResetJobsOnTerminalLaunches candidate discovery alone).
func applyOptimizeJobStatusLatestAttempt(ctx context.Context, db *sql.DB) error {
	return recreateAuthoritativeJobStatusView(ctx, db)
}

func dropOptimizeJobStatusLatestAttempt(context.Context, *sql.DB) error {
	return nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&exists)
	return exists > 0, err
}

// HasPending reports whether any migration has not yet been applied to db.
func HasPending(ctx context.Context, db *sql.DB) (bool, error) {
	p, err := newProvider(db)
	if err != nil {
		return false, err
	}
	return p.HasPending(ctx)
}

// Version returns the highest migration version applied to db. It returns 0
// for a pre-goose database that has no goose version table yet.
func Version(ctx context.Context, db *sql.DB) int64 {
	p, err := newProvider(db)
	if err != nil {
		return 0
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return 0
	}
	return v
}

// goMigrationVersions enumerates versions implemented as Go migrations.
// Keep in sync with the goose.WithGoMigrations call in newProvider.
var goMigrationVersions = []int64{9, 18, 19, 20, 21, 22, 23, 24, 26, 27, 28, 30, 31, 32, 39, 40, 43, 44}

// Target returns the highest migration version this binary knows about — the
// version a fully-migrated database should report. It is the v1 baseline plus
// any higher-numbered .sql migration under sql/ and any Go-migration version
// registered in goMigrationVersions.
func Target() int64 {
	target := int64(1) // the squashed baseline
	entries, err := fs.ReadDir(sqlMigrations, "sql")
	if err != nil {
		// Fall through to Go-migration scan; baseline + goMigrationVersions
		// still gives a meaningful Target even without the embed.
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil && v > target {
			target = v
		}
	}
	for _, v := range goMigrationVersions {
		if v > target {
			target = v
		}
	}
	return target
}

// Up applies every pending migration to db.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return err
	}
	return nil
}

// applyBaseline creates the entire current schema. It is the v1 migration —
// the squash of the pre-goose migration history. Every statement is
// CREATE ... IF NOT EXISTS, so applying it to the existing pre-goose database
// is a no-op that simply lets goose record v1.
func applyBaseline(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, baselineSchema); err != nil {
		return fmt.Errorf("apply baseline schema: %w", err)
	}
	// Seed singleton rows the schema requires but a .schema dump does not
	// carry. INSERT OR IGNORE keeps this a no-op on the existing database.
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO autopilot_state(id, paused) VALUES (1, 0)`,
	); err != nil {
		return fmt.Errorf("seed autopilot_state singleton: %w", err)
	}
	return nil
}

// dropBaseline is the down migration for the baseline. A squashed baseline has
// no meaningful down step; reset by deleting the database file.
func dropBaseline(ctx context.Context, db *sql.DB) error {
	return fmt.Errorf("the v1 baseline cannot be reverted; delete the database file to reset")
}

// applyAddExternalSyncWarningColumns gives weft's own observation failures a
// channel separate from raw_status_message, which belongs to the executor. The
// warning was previously written over that column, so a transport blip deleted
// the most recent thing actually known about the job. See
// external-executor.allium ExternalObservationLeavesStateAlone.
func applyAddExternalSyncWarningColumns(ctx context.Context, db *sql.DB) error {
	cols := []struct {
		name string
		ddl  string
	}{
		{"sync_warning", `ALTER TABLE external_job_bindings ADD COLUMN sync_warning TEXT`},
		{"sync_warning_at", `ALTER TABLE external_job_bindings ADD COLUMN sync_warning_at INTEGER`},
	}
	for _, col := range cols {
		exists, err := columnExists(ctx, db, "external_job_bindings", col.name)
		if err != nil {
			return fmt.Errorf("inspect external_job_bindings.%s: %w", col.name, err)
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, col.ddl); err != nil {
			return fmt.Errorf("add external_job_bindings.%s: %w", col.name, err)
		}
	}
	return nil
}

func dropAddExternalSyncWarningColumns(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE external_job_bindings DROP COLUMN sync_warning`)
	_, _ = db.ExecContext(ctx, `ALTER TABLE external_job_bindings DROP COLUMN sync_warning_at`)
	return nil
}

// applyDropSpeculativeHostRegistry removes launches.host_id and the hosts
// table. Both were speculative: never written, never read.
//
// launches.host_id held no value in 6,261 rows while machine_id beside it held
// 5,648. That adjacency is the hazard — a user looking for machine identity
// checked host_id, found it empty, and concluded weft did not record which
// physical machine ran a job. It does.
//
// The hosts table (name, provider, provider_machine_id, first_seen_at) reads
// as a machine registry, but weft already has two identity mechanisms and
// needs no third: on-prem hosts are files under ~/.config/weft/hosts, and
// cloud machines are launches.machine_id. What a registry would have added is
// derivable — first-seen, last-seen and usage count come from one GROUP BY
// over launches — and per-machine reliability already lives in the survival
// model's MachineStats, keyed on the same id. If per-machine measurements ever
// need storing, that table should be designed against the need rather than
// inherited from this guess.
func applyDropSpeculativeHostRegistry(ctx context.Context, db *sql.DB) error {
	exists, err := columnExists(ctx, db, "launches", "host_id")
	if err != nil {
		return fmt.Errorf("inspect launches.host_id: %w", err)
	}
	if exists {
		if _, err := db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN host_id`); err != nil {
			return fmt.Errorf("drop launches.host_id: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS hosts`); err != nil {
		return fmt.Errorf("drop hosts: %w", err)
	}
	return nil
}

func undoDropSpeculativeHostRegistry(ctx context.Context, db *sql.DB) error {
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches ADD COLUMN host_id INTEGER`)
	_, _ = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hosts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		provider TEXT,
		provider_machine_id TEXT,
		first_seen_at INTEGER NOT NULL,
		UNIQUE(provider, provider_machine_id)
	)`)
	return nil
}
