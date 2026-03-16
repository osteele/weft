// Package db provides database operations for job management.
//
// # Sync Operations Pattern
//
// Functions that update job status based on remote state (sync operations) MUST
// also update last_synced_status to maintain consistency. These functions are:
//
//   - RecordCompletionByID: job completed on remote
//   - MarkDeadByID: job died unexpectedly on remote
//   - MarkQueuedJobRunning: sync detected job started running
//   - MarkQueuedByID: sync found job still in queue
//   - RecordCompletion: session-based completion (legacy)
//   - MarkDead: session-based death (legacy)
//   - UpdateStatusAndLastSynced: explicit sync update
//   - ClearPendingAndUpdateStatus: reconciliation succeeded
//
// Functions that update status for LOCAL operations (user intent, not remote state)
// should NOT update last_synced_status. Examples: MarkPausedByID (user requested pause),
// MarkRunningByID (local state machine transition).
//
// To prevent bugs: any new function that detects remote state changes must update
// both status and last_synced_status together.
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

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/queuefile"
	"github.com/osteele/weft/internal/workdir"
	_ "modernc.org/sqlite"
)

// Job represents a job record
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
	Backend              string // Execution backend ("queue-runner", "slurm")
	RemoteID             string // Backend-specific job identifier (e.g., SLURM job ID)
	RemoteState          string // Backend-specific state (e.g., SLURM state)
	FailureReason        string // Normalized failure reason (e.g., "timeout", "oom")
	QueueName            string // Name of the queue this job belongs to (empty for non-queued jobs)
	GPU                  string // CUDA_VISIBLE_DEVICES value (e.g., "0", "0,1")
	GPUClass             string // GPU class name (e.g., "A100") — resolved to device at runtime
	CPUAllotment         *int   // Requested CPU allotment percent (nil = default)
	GPUMemGB             *int   // GPU memory reservation in GB per device (nil = use default)
	Metadata             *JobMetadata
	EnvVars              []string
	Tags                 []string
	DepSpec              string   // Dependency specification (e.g., "42" or "42+" for after-any)
	Inputs               []string // Data asset refs consumed by this job (e.g., "hf:meta-llama/Llama-3-8B")
	Outputs              []string // Data asset refs produced by this job
	OutputDirs           []string // Convention-based output directories from .weft.toml
	Produces             []string // Artifact specs this job produces (e.g., "output/model.pt" or "output/model.pt:100")
	Needs                []string // Artifact specs this job needs (e.g., "output/model.pt:100")
	Project              string   // Basename of working directory (stored at creation time)
	CreatedAt            int64    // When the job was created/queued (0 for legacy jobs)
	QueuedAt             int64    // When job was added to remote queue (for queue ordering)
	StartTime            int64
	EndTime              *int64
	ExitCode             *int
	Status               string
	Tombstoned           bool
	Cost                 *float64       // Actual cost in dollars (for cloud-run jobs)
	VastaiInstanceID     *int           // Vast.ai instance ID (for vastai backend jobs)
	ErrorDiagnosis       string         // JSON-encoded remediation diagnosis (see remediation.ErrorDiagnosis)
	RetryCount           int            // Number of auto-remediation retries attempted
	PlacementMeta        *PlacementMeta // Placement telemetry (predictions, scores)
	PlacementReasons     []string       // Why the job is currently unplaced
	CloudInstanceID      *int64         // Cloud instance ID if this job is part of a cloud instance
	CampaignJobIndex     *int           // Position within a cloud campaign sequence, if assigned
	LatestRunID          *int64         // Latest execution attempt row for this logical job

	// Three-way merge state for reconciliation
	LastSyncedStatus string  // Base: what remote was at last successful sync
	PendingStatus    *string // Local: what user wants (nil = no pending change)
	PendingAt        *int64  // When pending state was set
}

// JobRun stores the immutable spec snapshot plus mutable execution state for a
// single execution attempt of a logical job.
type JobRun struct {
	ID               int64
	JobID            int64
	ArchivedAt       int64
	ArchiveReason    string
	Status           string
	Host             string
	WorkingDir       string
	Command          string
	Description      string
	SessionName      string
	QueueName        string
	Backend          string
	RemoteID         string
	RemoteState      string
	GPU              string
	GPUClass         string
	CPUAllotment     *int
	GPUMemGB         *int
	EnvVars          []string
	Tags             []string
	DepSpec          string
	Inputs           []string
	Outputs          []string
	OutputDirs       []string
	Produces         []string
	Needs            []string
	Project          string
	StartTime        int64
	EndTime          *int64
	ExitCode         *int
	ErrorMessage     string
	FailureReason    string
	ErrorDiagnosis   string
	Metadata         *JobMetadata
	PlacementMeta    *PlacementMeta
	Cost             *float64
	VastaiInstanceID *int
	RetryCount       int
	CloudInstanceID  *int64
}

// JobTargetKind describes how a job is currently targeted.
type JobTargetKind string

const (
	JobTargetUnplaced       JobTargetKind = "unplaced"
	JobTargetInventoryHost  JobTargetKind = "inventory_host"
	JobTargetRentalInstance JobTargetKind = "rental_instance"
)

// UsesQueueRunner reports whether this job should be managed by the queue runner backend.
func (j *Job) UsesQueueRunner() bool {
	if j == nil {
		return true
	}
	if j.Backend == BackendSlurm {
		return false
	}
	return j.SessionName == ""
}

// UsesSlurm reports whether this job should be managed by the SLURM backend.
func (j *Job) UsesSlurm() bool {
	if j == nil {
		return false
	}
	return j.Backend == BackendSlurm
}

// IsCloudJob reports whether this job is associated with a cloud instance.
func (j *Job) IsCloudJob() bool {
	return j.IsRentalJob()
}

// TargetKind reports whether the job is unplaced, assigned to an inventory
// host, or assigned to a rental instance. Legacy synthetic rental host strings
// are still recognized for compatibility with older rows.
func (j *Job) TargetKind() JobTargetKind {
	if j == nil {
		return JobTargetUnplaced
	}
	if j.CloudInstanceID != nil && *j.CloudInstanceID > 0 {
		return JobTargetRentalInstance
	}
	host := strings.TrimSpace(j.Host)
	switch {
	case host == "":
		return JobTargetUnplaced
	case IsCloudHost(host):
		return JobTargetRentalInstance
	default:
		return JobTargetInventoryHost
	}
}

// IsRentalJob reports whether the job is assigned to a rental instance.
func (j *Job) IsRentalJob() bool {
	return j != nil && j.TargetKind() == JobTargetRentalInstance
}

// HasInventoryHost reports whether the job is currently assigned to an
// inventory host.
func (j *Job) HasInventoryHost() bool {
	return j != nil && j.TargetKind() == JobTargetInventoryHost
}

// TargetDisplay returns a user-facing label for the current target.
func (j *Job) TargetDisplay() string {
	switch j.TargetKind() {
	case JobTargetRentalInstance:
		if j != nil && j.CloudInstanceID != nil && *j.CloudInstanceID > 0 {
			return fmt.Sprintf("rental:%d", *j.CloudInstanceID)
		}
		if j != nil {
			host := strings.TrimSpace(j.Host)
			if host != "" {
				return host
			}
		}
		return "rental"
	case JobTargetInventoryHost:
		if j != nil {
			return strings.TrimSpace(j.Host)
		}
	}
	return "(unplaced)"
}

// UsesRentalPlacement reports whether a job is explicitly or effectively on a
// rental workflow.
func (j *Job) UsesRentalPlacement() bool {
	if j == nil {
		return false
	}
	return j.HasTag(TagRental) || j.IsRentalJob()
}

// UsesInventoryPlacement reports whether a job is inventory-only or currently
// assigned to a non-rental host.
func (j *Job) UsesInventoryPlacement() bool {
	if j == nil || j.UsesRentalPlacement() {
		return false
	}
	if j.HasTag(TagInventory) {
		return true
	}
	return j.HasInventoryHost()
}

// PlacementMeta holds placement telemetry stored as JSON on the job record.
type PlacementMeta struct {
	PredictedDurationS *float64 `json:"pred_dur_s,omitempty"`
	PredictedRSSKB     *float64 `json:"pred_rss_kb,omitempty"`
	PredictedGPUMemMiB *float64 `json:"pred_gpu_mib,omitempty"`
	SelectedScore      float64  `json:"score"`
	RunnerUpHost       string   `json:"runner_up,omitempty"`
	RunnerUpScore      float64  `json:"runner_up_score,omitempty"`
}

const jobSelectColumns = `id, host, session_name, working_dir, command, description, generated_description, generation_hash, created_at, queued_at, start_time, end_time, exit_code, status, error_message, backend, remote_id, remote_state, failure_reason, queue_name, gpu, gpu_class, cpu_allotment, gpu_mem_gb, env_vars, tags, dep_spec, inputs, outputs, output_dirs, produces, needs, project, tombstoned, last_synced_status, pending_status, pending_at, job_metadata, cost, vastai_instance_id, error_diagnosis, retry_count, placement_meta, placement_reasons, cloud_instance_id, campaign_job_index, latest_run_id`

const jobRunSelectColumns = `id, job_id, archived_at, archive_reason, status, host, working_dir, command, description, session_name, queue_name, backend, remote_id, remote_state, gpu, gpu_class, cpu_allotment, gpu_mem_gb, env_vars, tags, dep_spec, inputs, outputs, output_dirs, produces, needs, project, start_time, end_time, exit_code, error_message, failure_reason, error_diagnosis, job_metadata, placement_meta, cost, vastai_instance_id, retry_count, cloud_instance_id`

const jobTableColumns = `id, host, session_name, working_dir, command, description, generated_description, generation_hash, created_at, queued_at, start_time, end_time, exit_code, status, error_message, backend, remote_id, remote_state, failure_reason, queue_name, gpu, gpu_class, cpu_allotment, gpu_mem_gb, env_vars, tags, dep_spec, inputs, outputs, output_dirs, produces, needs, project, tombstoned, last_synced_status, pending_status, pending_at, job_metadata, cost, vastai_instance_id, error_diagnosis, retry_count, placement_meta, placement_host, placement_reasons, cloud_instance_id, campaign_job_index, latest_run_id`

const campaignTableColumns = `id, status, created_at, ended_at, estimated_cost_cents`

const cloudInstanceTableColumns = `id, campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb, vastai_instance_id, max_spend_cents, max_time_seconds, actual_spend_cents, created_at, ready_at, launched_at, ended_at, resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability, inet_down_mbps, inet_up_mbps, cuda_version, provider_instance_id, data_center, instance_role, donor_instance_id, seed_download_secs, seed_copy_secs, grace_period_seconds, grace_started_at, grace_deadline, termination_reason, disk_gb, provisioned_inputs, termination_requested_at, termination_intent_json`

const jobCloudAttemptTableColumns = `id, job_id, cloud_instance_id, started_at, ended_at, outcome`

func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

func statusCheckConstraintSQL(column string, values []string, allowNull bool) string {
	expr := fmt.Sprintf("%s IN (%s)", column, sqlStringList(values))
	if allowNull {
		return fmt.Sprintf("%s IS NULL OR %s", column, expr)
	}
	return expr
}

func jobStatusValues() []string {
	return []string{
		StatusStarting,
		StatusRunning,
		StatusCompleted,
		StatusDead,
		StatusQueued,
		StatusFailed,
		StatusKilled,
		StatusCanceled,
		StatusPaused,
		StatusDraft,
		StatusPendingPlacement,
	}
}

func cloudInstanceStatusValues() []string {
	return []string{
		CloudInstanceStatusPlanned,
		CloudInstanceStatusLaunching,
		CloudInstanceStatusRunning,
		CloudInstanceStatusGrace,
		CloudInstanceStatusCompleted,
		CloudInstanceStatusFailed,
		CloudInstanceStatusCancelled,
	}
}

func campaignStatusValues() []string {
	return []string{
		CampaignStatusPlanned,
		CampaignStatusLaunching,
		CampaignStatusRunning,
		CampaignStatusCompleted,
		CampaignStatusFailed,
		CampaignStatusCancelled,
	}
}

func terminationReasonValues() []string {
	return []string{
		TerminationReasonCompleted,
		TerminationReasonPreempted,
		TerminationReasonJobFailure,
		TerminationReasonDiskFull,
		TerminationReasonInfraFailure,
		TerminationReasonCancelled,
	}
}

func jobCloudAttemptOutcomeValues() []string {
	return []string{
		AttemptOutcomeCompleted,
		AttemptOutcomeFailed,
		AttemptOutcomeCancelled,
		AttemptOutcomeOrphaned,
		AttemptOutcomeSuperseded,
	}
}

func createCampaignsTableSQL(table string, ifNotExists bool) string {
	ifClause := ""
	if ifNotExists {
		ifClause = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE TABLE %s%s (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		status TEXT NOT NULL DEFAULT 'planned',
		created_at INTEGER NOT NULL,
		ended_at INTEGER,
		estimated_cost_cents INTEGER,
		CONSTRAINT campaigns_status_check CHECK (%s)
	)`, ifClause, table, statusCheckConstraintSQL("status", campaignStatusValues(), false))
}

func createJobsTableSQL(table string, ifNotExists bool) string {
	ifClause := ""
	if ifNotExists {
		ifClause = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE TABLE %s%s (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		session_name TEXT,
		working_dir TEXT NOT NULL,
		command TEXT NOT NULL,
		description TEXT,
		generated_description TEXT,
		generation_hash TEXT,
		created_at INTEGER,
		queued_at INTEGER,
		start_time INTEGER,
		end_time INTEGER,
		exit_code INTEGER,
		status TEXT NOT NULL DEFAULT 'running',
		error_message TEXT,
		backend TEXT DEFAULT 'queue-runner',
		remote_id TEXT,
		remote_state TEXT,
		failure_reason TEXT,
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
		last_synced_status TEXT,
		pending_status TEXT,
		pending_at INTEGER,
		job_metadata TEXT,
		cost REAL,
		vastai_instance_id INTEGER,
		error_diagnosis TEXT,
		retry_count INTEGER DEFAULT 0,
		placement_meta TEXT,
		placement_host TEXT,
		placement_reasons TEXT,
		cloud_instance_id INTEGER,
		campaign_job_index INTEGER,
		latest_run_id INTEGER,
		CONSTRAINT jobs_status_check CHECK (%s),
		CONSTRAINT jobs_pending_status_check CHECK (%s)
	)`, ifClause, table,
		statusCheckConstraintSQL("status", jobStatusValues(), false),
		statusCheckConstraintSQL("pending_status", jobStatusValues(), true),
	)
}

func createCloudInstancesTableSQL(table string, ifNotExists bool) string {
	ifClause := ""
	if ifNotExists {
		ifClause = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE TABLE %s%s (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		campaign_id INTEGER REFERENCES campaigns(id),
		status TEXT NOT NULL DEFAULT 'planned',
		provider TEXT NOT NULL DEFAULT 'vastai',
		gpu_spec TEXT,
		gpu_class TEXT,
		gpu_mem_gb INTEGER,
		vastai_instance_id TEXT,
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
		provider_instance_id TEXT,
		data_center TEXT,
		instance_role TEXT DEFAULT 'worker',
		donor_instance_id INTEGER,
		seed_download_secs INTEGER,
		seed_copy_secs INTEGER,
		grace_period_seconds INTEGER,
		grace_started_at INTEGER,
		grace_deadline INTEGER,
		termination_reason TEXT,
		disk_gb INTEGER,
		provisioned_inputs TEXT,
		termination_requested_at INTEGER,
		termination_intent_json TEXT,
		CONSTRAINT cloud_instances_termination_reason_check CHECK (%s),
		CONSTRAINT cloud_instances_status_check CHECK (%s)
	)`, ifClause, table,
		statusCheckConstraintSQL("termination_reason", terminationReasonValues(), true),
		statusCheckConstraintSQL("status", cloudInstanceStatusValues(), false))
}

func createJobCloudAttemptsTableSQL(table string, ifNotExists bool) string {
	ifClause := ""
	if ifNotExists {
		ifClause = "IF NOT EXISTS "
	}
	return fmt.Sprintf(`CREATE TABLE %s%s (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL REFERENCES jobs(id),
		cloud_instance_id INTEGER NOT NULL REFERENCES cloud_instances(id),
		started_at INTEGER NOT NULL,
		ended_at INTEGER,
		outcome TEXT,
		CONSTRAINT job_cloud_attempts_outcome_check CHECK (%s),
		CONSTRAINT job_cloud_attempts_shape_check CHECK (
			(ended_at IS NULL AND outcome IS NULL)
			OR
			(ended_at IS NOT NULL AND outcome IS NOT NULL)
		)
	)`, ifClause, table, statusCheckConstraintSQL("outcome", jobCloudAttemptOutcomeValues(), true))
}

// qualifiedJobSelectColumns returns jobSelectColumns with each column prefixed
// by the given table alias (e.g. "jobs" → "jobs.id, jobs.host, ...").
func qualifiedJobSelectColumns(table string) string {
	cols := strings.Split(jobSelectColumns, ",")
	for i, col := range cols {
		cols[i] = table + "." + strings.TrimSpace(col)
	}
	return strings.Join(cols, ", ")
}

// qualifiedJobSelectColumnsWithOverrides returns jobSelectColumns with each
// column prefixed by table, except for any columns present in overrides. The
// override expression should omit the alias; this function aliases it to the
// original column name in place.
func qualifiedJobSelectColumnsWithOverrides(table string, overrides map[string]string) string {
	cols := strings.Split(jobSelectColumns, ",")
	for i, rawCol := range cols {
		col := strings.TrimSpace(rawCol)
		if override, ok := overrides[col]; ok {
			cols[i] = override + " AS " + col
			continue
		}
		cols[i] = table + "." + col
	}
	return strings.Join(cols, ", ")
}

func createJobStateViews(db *sql.DB) error {
	for _, name := range []string{"cloud_instance_job_membership", "job_effective_state"} {
		if _, err := db.Exec(`DROP VIEW IF EXISTS ` + name); err != nil {
			return err
		}
	}

	effectiveColumns := qualifiedJobSelectColumnsWithOverrides("decorated", map[string]string{
		"host":              "decorated.effective_host",
		"status":            "decorated.effective_status",
		"cloud_instance_id": "decorated.effective_cloud_instance_id",
	})
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE VIEW job_effective_state AS
		WITH latest_open_cloud_attempt AS (
			SELECT jca.job_id,
			       jca.cloud_instance_id,
			       ci.status AS instance_status
			FROM job_cloud_attempts jca
			JOIN cloud_instances ci ON ci.id = jca.cloud_instance_id
			WHERE jca.ended_at IS NULL
			  AND NOT EXISTS (
				SELECT 1
				FROM job_cloud_attempts newer
				WHERE newer.job_id = jca.job_id
				  AND newer.ended_at IS NULL
				  AND (newer.started_at > jca.started_at
				       OR (newer.started_at = jca.started_at AND newer.id > jca.id))
			  )
		),
		base AS (
			SELECT jobs.*,
			       assigned_ci.status AS assigned_instance_status,
			       open_attempt.cloud_instance_id AS current_cloud_attempt_instance_id,
			       open_attempt.instance_status AS current_cloud_attempt_instance_status,
			       CASE
					WHEN jobs.cloud_instance_id IS NOT NULL
					     AND assigned_ci.status IN ('running', 'launching', 'grace')
					THEN jobs.cloud_instance_id
					WHEN open_attempt.cloud_instance_id IS NOT NULL
					     AND open_attempt.instance_status IN ('running', 'launching', 'grace')
					THEN open_attempt.cloud_instance_id
					ELSE NULL
			       END AS effective_cloud_instance_id
			FROM jobs
			LEFT JOIN cloud_instances assigned_ci ON assigned_ci.id = jobs.cloud_instance_id
			LEFT JOIN latest_open_cloud_attempt open_attempt ON open_attempt.job_id = jobs.id
		),
		decorated AS (
			SELECT base.*,
			       CASE
					WHEN base.effective_cloud_instance_id IS NOT NULL THEN ''
					ELSE base.host
			       END AS effective_host,
			       CASE
					WHEN base.effective_cloud_instance_id IS NOT NULL THEN 'rental_instance'
					WHEN base.host != ''
					     AND base.host NOT LIKE 'vastai:%%'
					     AND base.host NOT LIKE 'runpod:%%'
					THEN 'inventory_host'
					WHEN base.host LIKE 'vastai:%%' OR base.host LIKE 'runpod:%%'
					THEN 'rental_instance'
					ELSE 'unplaced'
			       END AS effective_target_kind,
			       CASE
					WHEN (
						CASE
							WHEN base.effective_cloud_instance_id IS NOT NULL THEN 'rental_instance'
							WHEN base.host != ''
							     AND base.host NOT LIKE 'vastai:%%'
							     AND base.host NOT LIKE 'runpod:%%'
							THEN 'inventory_host'
							WHEN base.host LIKE 'vastai:%%' OR base.host LIKE 'runpod:%%'
							THEN 'rental_instance'
							ELSE 'unplaced'
						END
					) = 'unplaced'
					AND COALESCE(base.pending_status, base.status) IN ('running', 'starting', 'paused')
					THEN 'queued'
					ELSE COALESCE(base.pending_status, base.status)
			       END AS effective_status
			FROM base
		)
		SELECT %s,
		       decorated.host AS raw_host,
		       decorated.status AS raw_status,
		       decorated.cloud_instance_id AS raw_cloud_instance_id,
		       decorated.effective_status,
		       decorated.effective_host,
		       decorated.effective_cloud_instance_id,
		       decorated.effective_target_kind,
		       decorated.current_cloud_attempt_instance_id,
		       decorated.current_cloud_attempt_instance_status,
		       CASE
				WHEN decorated.current_cloud_attempt_instance_id IS NOT NULL THEN 1
				ELSE 0
		       END AS has_open_cloud_attempt,
		       CASE
				WHEN decorated.current_cloud_attempt_instance_status IN ('running', 'launching', 'grace') THEN 1
				ELSE 0
		       END AS has_open_live_cloud_attempt,
		       CASE
				WHEN decorated.effective_target_kind = 'unplaced' THEN 1
				ELSE 0
		       END AS is_effectively_unplaced,
		       CASE
				WHEN decorated.effective_target_kind = 'rental_instance' THEN 1
				ELSE 0
		       END AS is_effectively_current_rental
		FROM decorated
	`, effectiveColumns)); err != nil {
		return err
	}

	currentMembershipColumns := qualifiedJobSelectColumnsWithOverrides("jes", map[string]string{
		"cloud_instance_id": "current_memberships.membership_cloud_instance_id",
	})
	historicalMembershipColumns := qualifiedJobSelectColumns("jes")
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE VIEW cloud_instance_job_membership AS
		WITH current_memberships AS (
			SELECT DISTINCT
			       jobs.id AS job_id,
			       jobs.cloud_instance_id AS membership_cloud_instance_id
			FROM jobs
			WHERE jobs.cloud_instance_id IS NOT NULL
			UNION
			SELECT DISTINCT
			       jes.id AS job_id,
			       jes.current_cloud_attempt_instance_id AS membership_cloud_instance_id
			FROM job_effective_state jes
			WHERE jes.current_cloud_attempt_instance_id IS NOT NULL
		),
		historical_memberships AS (
			SELECT DISTINCT
			       jes.id AS job_id,
			       jca.cloud_instance_id AS membership_cloud_instance_id
			FROM job_effective_state jes
			JOIN job_cloud_attempts jca ON jca.job_id = jes.id
			WHERE jca.cloud_instance_id IS NOT NULL
			  AND (jes.cloud_instance_id IS NULL OR jca.cloud_instance_id != jes.cloud_instance_id)
		)
		SELECT %s,
		       current_memberships.membership_cloud_instance_id,
		       'current' AS membership_kind,
		       0 AS membership_rank
		FROM job_effective_state jes
		JOIN current_memberships ON current_memberships.job_id = jes.id
		UNION ALL
		SELECT %s,
		       historical_memberships.membership_cloud_instance_id,
		       'historical' AS membership_kind,
		       1 AS membership_rank
		FROM job_effective_state jes
		JOIN historical_memberships ON historical_memberships.job_id = jes.id
	`, currentMembershipColumns, historicalMembershipColumns)); err != nil {
		return err
	}

	return nil
}

func createCloudAttemptTriggers(db *sql.DB) error {
	stmts := []string{
		`CREATE TRIGGER job_cloud_attempts_require_instance
		BEFORE INSERT ON job_cloud_attempts
		FOR EACH ROW
		WHEN NOT EXISTS (SELECT 1 FROM cloud_instances WHERE id = NEW.cloud_instance_id)
		BEGIN
			SELECT RAISE(ABORT, 'job_cloud_attempt references missing cloud instance');
		END`,
		`CREATE TRIGGER job_cloud_attempts_prevent_second_open
		BEFORE INSERT ON job_cloud_attempts
		FOR EACH ROW
		WHEN NEW.ended_at IS NULL
		     AND EXISTS (
				SELECT 1
				FROM job_cloud_attempts
				WHERE job_id = NEW.job_id
				  AND ended_at IS NULL
		     )
		BEGIN
			SELECT RAISE(ABORT, 'job already has an open cloud attempt');
		END`,
		`CREATE TRIGGER job_cloud_attempts_shape_on_insert
		BEFORE INSERT ON job_cloud_attempts
		FOR EACH ROW
		WHEN (NEW.ended_at IS NULL AND NEW.outcome IS NOT NULL)
		     OR (NEW.ended_at IS NOT NULL AND NEW.outcome IS NULL)
		BEGIN
			SELECT RAISE(ABORT, 'job_cloud_attempt ended_at and outcome must both be NULL or both be set');
		END`,
		`CREATE TRIGGER job_cloud_attempts_shape_on_update
		BEFORE UPDATE OF ended_at, outcome ON job_cloud_attempts
		FOR EACH ROW
		WHEN (NEW.ended_at IS NULL AND NEW.outcome IS NOT NULL)
		     OR (NEW.ended_at IS NOT NULL AND NEW.outcome IS NULL)
		BEGIN
			SELECT RAISE(ABORT, 'job_cloud_attempt ended_at and outcome must both be NULL or both be set');
		END`,
		`CREATE TRIGGER job_cloud_attempts_sync_job_on_open
		AFTER INSERT ON job_cloud_attempts
		FOR EACH ROW
		WHEN NEW.ended_at IS NULL
		     AND EXISTS (
				SELECT 1
				FROM cloud_instances
				WHERE id = NEW.cloud_instance_id
				  AND status IN ('running', 'launching', 'grace')
		     )
		BEGIN
			UPDATE jobs
			SET cloud_instance_id = NEW.cloud_instance_id,
			    host = '',
			    placement_reasons = NULL
			WHERE id = NEW.job_id;
		END`,
		`CREATE TRIGGER job_cloud_attempts_sync_job_on_close
		AFTER UPDATE OF ended_at ON job_cloud_attempts
		FOR EACH ROW
		WHEN OLD.ended_at IS NULL AND NEW.ended_at IS NOT NULL
		BEGIN
			UPDATE jobs
			SET cloud_instance_id = (
					SELECT jca.cloud_instance_id
					FROM job_cloud_attempts jca
					JOIN cloud_instances ci ON ci.id = jca.cloud_instance_id
					WHERE jca.job_id = NEW.job_id
					  AND jca.ended_at IS NULL
					  AND ci.status IN ('running', 'launching', 'grace')
					ORDER BY jca.started_at DESC, jca.id DESC
					LIMIT 1
			    ),
			    host = CASE
					WHEN EXISTS (
						SELECT 1
						FROM job_cloud_attempts jca
						JOIN cloud_instances ci ON ci.id = jca.cloud_instance_id
						WHERE jca.job_id = NEW.job_id
						  AND jca.ended_at IS NULL
						  AND ci.status IN ('running', 'launching', 'grace')
					) THEN ''
					ELSE host
			    END
			WHERE id = NEW.job_id;
		END`,
		`CREATE TRIGGER jobs_prevent_clearing_live_cloud_assignment
		BEFORE UPDATE OF cloud_instance_id ON jobs
		FOR EACH ROW
		WHEN OLD.cloud_instance_id IS NOT NULL
		     AND NEW.cloud_instance_id IS NULL
		     AND EXISTS (
				SELECT 1
				FROM job_cloud_attempts jca
				JOIN cloud_instances ci ON ci.id = jca.cloud_instance_id
				WHERE jca.job_id = OLD.id
				  AND jca.ended_at IS NULL
				  AND ci.status IN ('running', 'launching', 'grace')
		     )
		BEGIN
			SELECT RAISE(ABORT, 'cannot clear cloud_instance_id while a live cloud attempt exists');
		END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func createIntegrityTriggers(db *sql.DB) error {
	stmts := []string{
		`CREATE TRIGGER jobs_prevent_mixed_cloud_host_on_insert
		BEFORE INSERT ON jobs
		FOR EACH ROW
		WHEN NEW.cloud_instance_id IS NOT NULL AND TRIM(COALESCE(NEW.host, '')) <> ''
		BEGIN
			SELECT RAISE(ABORT, 'jobs.host must be empty when cloud_instance_id is set');
		END`,
		`CREATE TRIGGER jobs_prevent_mixed_cloud_host_on_update
		BEFORE UPDATE OF host, cloud_instance_id ON jobs
		FOR EACH ROW
		WHEN NEW.cloud_instance_id IS NOT NULL AND TRIM(COALESCE(NEW.host, '')) <> ''
		BEGIN
			SELECT RAISE(ABORT, 'jobs.host must be empty when cloud_instance_id is set');
		END`,
		`CREATE TRIGGER jobs_validate_latest_run_id_on_insert
		AFTER INSERT ON jobs
		FOR EACH ROW
		WHEN NEW.latest_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.latest_run_id
				  AND job_id = NEW.id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'jobs.latest_run_id must reference a run owned by the same job');
		END`,
		`CREATE TRIGGER jobs_validate_latest_run_id_on_update
		BEFORE UPDATE OF latest_run_id ON jobs
		FOR EACH ROW
		WHEN NEW.latest_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.latest_run_id
				  AND job_id = NEW.id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'jobs.latest_run_id must reference a run owned by the same job');
		END`,
		`CREATE TRIGGER artifacts_validate_job_run_ownership_on_insert
		BEFORE INSERT ON artifacts
		FOR EACH ROW
		WHEN NEW.job_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.job_run_id
				  AND job_id = NEW.job_id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'artifacts.job_run_id must reference a run owned by artifacts.job_id');
		END`,
		`CREATE TRIGGER artifacts_validate_job_run_ownership_on_update
		BEFORE UPDATE OF job_id, job_run_id ON artifacts
		FOR EACH ROW
		WHEN NEW.job_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.job_run_id
				  AND job_id = NEW.job_id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'artifacts.job_run_id must reference a run owned by artifacts.job_id');
		END`,
		`CREATE TRIGGER job_timeseries_validate_job_run_ownership_on_insert
		BEFORE INSERT ON job_timeseries
		FOR EACH ROW
		WHEN NEW.job_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.job_run_id
				  AND job_id = NEW.job_id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'job_timeseries.job_run_id must reference a run owned by job_timeseries.job_id');
		END`,
		`CREATE TRIGGER job_timeseries_validate_job_run_ownership_on_update
		BEFORE UPDATE OF job_id, job_run_id ON job_timeseries
		FOR EACH ROW
		WHEN NEW.job_run_id IS NOT NULL
		     AND NOT EXISTS (
				SELECT 1
				FROM job_runs
				WHERE id = NEW.job_run_id
				  AND job_id = NEW.job_id
		     )
		BEGIN
			SELECT RAISE(ABORT, 'job_timeseries.job_run_id must reference a run owned by job_timeseries.job_id');
		END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func dropIntegrityViewsAndTriggers(db *sql.DB) error {
	for _, name := range []string{"cloud_instance_job_membership", "job_effective_state"} {
		if _, err := db.Exec(`DROP VIEW IF EXISTS ` + name); err != nil {
			return err
		}
	}
	for _, name := range []string{
		"job_cloud_attempts_require_instance",
		"job_cloud_attempts_prevent_second_open",
		"job_cloud_attempts_shape_on_insert",
		"job_cloud_attempts_shape_on_update",
		"job_cloud_attempts_sync_job_on_open",
		"job_cloud_attempts_sync_job_on_close",
		"jobs_prevent_clearing_live_cloud_assignment",
		"jobs_prevent_mixed_cloud_host_on_insert",
		"jobs_prevent_mixed_cloud_host_on_update",
		"jobs_validate_latest_run_id_on_insert",
		"jobs_validate_latest_run_id_on_update",
		"artifacts_validate_job_run_ownership_on_insert",
		"artifacts_validate_job_run_ownership_on_update",
		"job_timeseries_validate_job_run_ownership_on_insert",
		"job_timeseries_validate_job_run_ownership_on_update",
	} {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return err
		}
	}
	return nil
}

func tableSchemaContains(db *sql.DB, tableName, needle string) (bool, error) {
	var sqlText sql.NullString
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, tableName).Scan(&sqlText); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return sqlText.Valid && strings.Contains(strings.ToLower(sqlText.String), strings.ToLower(needle)), nil
}

func rebuildTable(db *sql.DB, createSQL, copySQL, dropSQL, renameSQL string, postStatements ...string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range append([]string{createSQL, copySQL, dropSQL, renameSQL}, postStatements...) {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureJobsTableConstraints(db *sql.DB) error {
	hasConstraint, err := tableSchemaContains(db, "jobs", "jobs_status_check")
	if err != nil {
		return err
	}
	if hasConstraint {
		return nil
	}
	return rebuildTable(
		db,
		createJobsTableSQL("jobs_new", false),
		fmt.Sprintf(`INSERT INTO jobs_new (%s) SELECT %s FROM jobs`, jobTableColumns, jobTableColumns),
		`DROP TABLE jobs`,
		`ALTER TABLE jobs_new RENAME TO jobs`,
		`CREATE INDEX idx_jobs_host ON jobs(host)`,
		`CREATE INDEX idx_jobs_session ON jobs(session_name)`,
		`CREATE INDEX idx_jobs_status ON jobs(status)`,
		`CREATE INDEX idx_jobs_start ON jobs(start_time DESC)`,
	)
}

func ensureCloudInstancesTableConstraints(db *sql.DB) error {
	hasStatusConstraint, err := tableSchemaContains(db, "cloud_instances", "cloud_instances_status_check")
	if err != nil {
		return err
	}
	hasTerminationReasonConstraint, err := tableSchemaContains(db, "cloud_instances", "cloud_instances_termination_reason_check")
	if err != nil {
		return err
	}
	if hasStatusConstraint && hasTerminationReasonConstraint {
		return nil
	}
	return rebuildTable(
		db,
		createCloudInstancesTableSQL("cloud_instances_new", false),
		fmt.Sprintf(`INSERT INTO cloud_instances_new (%s) SELECT %s FROM cloud_instances`, cloudInstanceTableColumns, cloudInstanceTableColumns),
		`DROP TABLE cloud_instances`,
		`ALTER TABLE cloud_instances_new RENAME TO cloud_instances`,
	)
}

func ensureCampaignsTableConstraints(db *sql.DB) error {
	hasConstraint, err := tableSchemaContains(db, "campaigns", "campaigns_status_check")
	if err != nil {
		return err
	}
	if hasConstraint {
		return nil
	}
	return rebuildTable(
		db,
		createCampaignsTableSQL("campaigns_new", false),
		fmt.Sprintf(`INSERT INTO campaigns_new (%s) SELECT %s FROM campaigns`, campaignTableColumns, campaignTableColumns),
		`DROP TABLE campaigns`,
		`ALTER TABLE campaigns_new RENAME TO campaigns`,
	)
}

func ensureJobCloudAttemptsTableConstraints(db *sql.DB) error {
	hasShapeCheck, err := tableSchemaContains(db, "job_cloud_attempts", "job_cloud_attempts_shape_check")
	if err != nil {
		return err
	}
	hasSuperseded, err := tableSchemaContains(db, "job_cloud_attempts", "'superseded'")
	if err != nil {
		return err
	}
	if hasShapeCheck && hasSuperseded {
		return nil
	}
	return rebuildTable(
		db,
		createJobCloudAttemptsTableSQL("job_cloud_attempts_new", false),
		fmt.Sprintf(`INSERT INTO job_cloud_attempts_new (%s) SELECT %s FROM job_cloud_attempts`, jobCloudAttemptTableColumns, jobCloudAttemptTableColumns),
		`DROP TABLE job_cloud_attempts`,
		`ALTER TABLE job_cloud_attempts_new RENAME TO job_cloud_attempts`,
		`CREATE INDEX idx_job_cloud_attempts_instance ON job_cloud_attempts(cloud_instance_id)`,
		`CREATE INDEX idx_job_cloud_attempts_job ON job_cloud_attempts(job_id)`,
		`CREATE INDEX idx_job_cloud_attempts_open ON job_cloud_attempts(job_id, ended_at, started_at DESC, id DESC)`,
	)
}

func repairLegacyCloudPlacementHosts(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE jobs
		SET host = ''
		WHERE cloud_instance_id IS NOT NULL
		  AND (
				TRIM(COALESCE(host, '')) = ''
				OR host LIKE 'vastai:%'
				OR host LIKE 'runpod:%'
		  )`)
	return err
}

func validateRepresentativeRows(db *sql.DB, query string, format func(*sql.Rows) (string, error), message string) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	var samples []string
	for rows.Next() {
		sample, err := format(rows)
		if err != nil {
			return err
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(samples) > 0 {
		return fmt.Errorf("%s: %s", message, strings.Join(samples, ", "))
	}
	return nil
}

func validateEnumAndRelationshipConstraints(db *sql.DB) error {
	jobStatusSQL := sqlStringList(jobStatusValues())
	campaignStatusSQL := sqlStringList(campaignStatusValues())
	cloudStatusSQL := sqlStringList(cloudInstanceStatusValues())
	terminationReasonSQL := sqlStringList(terminationReasonValues())
	attemptOutcomeSQL := sqlStringList(jobCloudAttemptOutcomeValues())

	validations := []struct {
		query   string
		format  func(*sql.Rows) (string, error)
		message string
	}{
		{
			query: fmt.Sprintf(`SELECT id, status FROM jobs WHERE status NOT IN (%s) ORDER BY id ASC LIMIT 5`, jobStatusSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, status), nil
			},
			message: "invalid jobs.status values",
		},
		{
			query: fmt.Sprintf(`SELECT id, pending_status FROM jobs WHERE pending_status IS NOT NULL AND pending_status NOT IN (%s) ORDER BY id ASC LIMIT 5`, jobStatusSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, status), nil
			},
			message: "invalid jobs.pending_status values",
		},
		{
			query: fmt.Sprintf(`SELECT id, status FROM campaigns WHERE status NOT IN (%s) ORDER BY id ASC LIMIT 5`, campaignStatusSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, status), nil
			},
			message: "invalid campaigns.status values",
		},
		{
			query: fmt.Sprintf(`SELECT id, status FROM cloud_instances WHERE status NOT IN (%s) ORDER BY id ASC LIMIT 5`, cloudStatusSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, status), nil
			},
			message: "invalid cloud_instances.status values",
		},
		{
			query: fmt.Sprintf(`SELECT id, termination_reason FROM cloud_instances WHERE termination_reason IS NOT NULL AND termination_reason != '' AND termination_reason NOT IN (%s) ORDER BY id ASC LIMIT 5`, terminationReasonSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var reason string
				if err := rows.Scan(&id, &reason); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, reason), nil
			},
			message: "invalid cloud_instances.termination_reason values",
		},
		{
			query: fmt.Sprintf(`SELECT id, outcome FROM job_cloud_attempts WHERE outcome IS NOT NULL AND outcome NOT IN (%s) ORDER BY id ASC LIMIT 5`, attemptOutcomeSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var outcome string
				if err := rows.Scan(&id, &outcome); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, outcome), nil
			},
			message: "invalid job_cloud_attempts.outcome values",
		},
		{
			query: `SELECT id, ended_at, outcome FROM job_cloud_attempts
				WHERE (ended_at IS NULL AND outcome IS NOT NULL)
				   OR (ended_at IS NOT NULL AND outcome IS NULL)
				ORDER BY id ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var endedAt sql.NullInt64
				var outcome sql.NullString
				if err := rows.Scan(&id, &endedAt, &outcome); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=(ended_at=%v,outcome=%q)", id, endedAt.Valid, outcome.String), nil
			},
			message: "invalid job_cloud_attempts ended_at/outcome shape",
		},
		{
			query: `SELECT j.id, j.latest_run_id, jr.job_id
				FROM jobs j
				LEFT JOIN job_runs jr ON jr.id = j.latest_run_id
				WHERE j.latest_run_id IS NOT NULL
				  AND (jr.id IS NULL OR jr.job_id != j.id)
				ORDER BY j.id ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var jobID int64
				var latestRunID int64
				var runJobID sql.NullInt64
				if err := rows.Scan(&jobID, &latestRunID, &runJobID); err != nil {
					return "", err
				}
				return fmt.Sprintf("job %d -> run %d owned by %v", jobID, latestRunID, runJobID), nil
			},
			message: "invalid jobs.latest_run_id ownership",
		},
		{
			query: `SELECT a.id, a.job_id, a.job_run_id, jr.job_id
				FROM artifacts a
				LEFT JOIN job_runs jr ON jr.id = a.job_run_id
				WHERE a.job_run_id IS NOT NULL
				  AND (jr.id IS NULL OR jr.job_id != a.job_id)
				ORDER BY a.id ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var id, jobID int64
				var runID sql.NullInt64
				var runJobID sql.NullInt64
				if err := rows.Scan(&id, &jobID, &runID, &runJobID); err != nil {
					return "", err
				}
				return fmt.Sprintf("artifact %d job=%d run=%v run_job=%v", id, jobID, runID, runJobID), nil
			},
			message: "invalid artifacts job_run ownership",
		},
		{
			query: `SELECT jt.job_id, jt.ts, jt.job_run_id, jr.job_id
				FROM job_timeseries jt
				LEFT JOIN job_runs jr ON jr.id = jt.job_run_id
				WHERE jt.job_run_id IS NOT NULL
				  AND (jr.id IS NULL OR jr.job_id != jt.job_id)
				ORDER BY jt.job_id ASC, jt.ts ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var jobID, ts int64
				var runID sql.NullInt64
				var runJobID sql.NullInt64
				if err := rows.Scan(&jobID, &ts, &runID, &runJobID); err != nil {
					return "", err
				}
				return fmt.Sprintf("timeseries (%d,%d) run=%v run_job=%v", jobID, ts, runID, runJobID), nil
			},
			message: "invalid job_timeseries job_run ownership",
		},
		{
			query: `SELECT id, host, cloud_instance_id
				FROM jobs
				WHERE cloud_instance_id IS NOT NULL
				  AND TRIM(COALESCE(host, '')) <> ''
				ORDER BY id ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var id, cloudInstanceID int64
				var host string
				if err := rows.Scan(&id, &host, &cloudInstanceID); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=(host=%q,cloud_instance_id=%d)", id, host, cloudInstanceID), nil
			},
			message: "invalid jobs host/cloud placement conflicts",
		},
	}

	for _, validation := range validations {
		if err := validateRepresentativeRows(db, validation.query, validation.format, validation.message); err != nil {
			return err
		}
	}
	return nil
}

func detectMultipleOpenCloudAttempts(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT job_id, COUNT(*)
		FROM job_cloud_attempts
		WHERE ended_at IS NULL
		GROUP BY job_id
		HAVING COUNT(*) > 1
		ORDER BY job_id ASC
		LIMIT 5`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var jobIDs []string
	for rows.Next() {
		var jobID int64
		var count int
		if err := rows.Scan(&jobID, &count); err != nil {
			return err
		}
		jobIDs = append(jobIDs, fmt.Sprintf("%d(%d)", jobID, count))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(jobIDs) > 0 {
		return fmt.Errorf("multiple open cloud attempts detected for jobs: %s", strings.Join(jobIDs, ", "))
	}
	return nil
}

func repairLiveCloudAssignments(db *sql.DB) error {
	_, err := db.Exec(`
		WITH latest_live_open_attempt AS (
			SELECT jca.job_id,
			       jca.cloud_instance_id
			FROM job_cloud_attempts jca
			JOIN cloud_instances ci ON ci.id = jca.cloud_instance_id
			WHERE jca.ended_at IS NULL
			  AND ci.status IN ('running', 'launching', 'grace')
			  AND NOT EXISTS (
				SELECT 1
				FROM job_cloud_attempts newer
				JOIN cloud_instances newer_ci ON newer_ci.id = newer.cloud_instance_id
				WHERE newer.job_id = jca.job_id
				  AND newer.ended_at IS NULL
				  AND newer_ci.status IN ('running', 'launching', 'grace')
				  AND (newer.started_at > jca.started_at
				       OR (newer.started_at = jca.started_at AND newer.id > jca.id))
			  )
		)
		UPDATE jobs
		SET cloud_instance_id = (
				SELECT latest_live_open_attempt.cloud_instance_id
				FROM latest_live_open_attempt
				WHERE latest_live_open_attempt.job_id = jobs.id
		    ),
		    host = '',
		    placement_reasons = NULL
		WHERE EXISTS (
				SELECT 1
				FROM latest_live_open_attempt
				WHERE latest_live_open_attempt.job_id = jobs.id
		    )
		  AND (
				jobs.cloud_instance_id IS NULL
				OR jobs.cloud_instance_id != (
					SELECT latest_live_open_attempt.cloud_instance_id
					FROM latest_live_open_attempt
					WHERE latest_live_open_attempt.job_id = jobs.id
				)
				OR jobs.host != ''
		    )`)
	return err
}

// Special job tags that affect scheduling and execution behavior.
const (
	ProcessedTag = "processed"
	TagExclusive = "exclusive"
	TagBenchmark = "benchmark"
	TagRental    = "rental"
	TagInventory = "inventory"

	// Legacy tag aliases accepted on input and in existing database rows.
	TagCloudLegacy  = "cloud"
	TagOnPremLegacy = "on-prem"

	// Deprecated aliases kept for internal compatibility while the codebase
	// moves to the preferred rental/inventory terminology.
	TagCloud  = TagRental
	TagOnPrem = TagInventory
)

const BackendQueueRunner = "queue-runner"
const BackendSlurm = "slurm"
const BackendVastai = "vastai"

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

// StatusPaused indicates a job that has been paused (SIGSTOP)
const StatusPaused = "paused"

// StatusDraft indicates a job that exists locally but should not run remotely
const StatusDraft = "draft"

// StatusPendingPlacement indicates a job submitted to the coordinator but not yet placed on a host
const StatusPendingPlacement = "pending_placement"

// statusNeedsRental is the legacy DB value for unplaced jobs. Migrated to StatusQueued with host="".
const statusNeedsRental = "needs_rental"

var dbPath string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	dbPath = filepath.Join(home, ".config", "weft", "jobs.db")
}

// Open opens the database, creating it if necessary
func Open() (*sql.DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	// Use _pragma DSN parameters to set per-connection PRAGMAs.
	// modernc.org/sqlite applies these on every new connection from the pool.
	// - journal_mode(WAL): better concurrent access (TUI + sync + CLI)
	// - busy_timeout(30000): wait up to 30s for locks instead of failing immediately
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)", dbPath)
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
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
	schema := createJobsTableSQL("jobs", true) + `;
	CREATE INDEX IF NOT EXISTS idx_jobs_host ON jobs(host);
	CREATE INDEX IF NOT EXISTS idx_jobs_session ON jobs(session_name);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_start ON jobs(start_time DESC);

	CREATE TABLE IF NOT EXISTS processed_relay_requests (
		request_id TEXT PRIMARY KEY,
		op TEXT NOT NULL,
		job_id INTEGER NOT NULL DEFAULT 0,
		processed_at INTEGER NOT NULL
	);
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

	// Migration: add backend column for execution backend
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN backend TEXT DEFAULT 'queue-runner'`); err != nil {
		return err
	}

	// Migration: add remote_id column for backend-specific job IDs
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN remote_id TEXT`); err != nil {
		return err
	}

	// Migration: add remote_state column for backend-specific states
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN remote_state TEXT`); err != nil {
		return err
	}

	// Migration: add failure_reason column for normalized failures
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN failure_reason TEXT`); err != nil {
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

	// Migration: add gpu_class column for GPU class-based scheduling (e.g., "A100")
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN gpu_class TEXT`); err != nil {
		return err
	}

	// Migration: add cpu_allotment column for per-job CPU allocation
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN cpu_allotment INTEGER`); err != nil {
		return err
	}

	// Migration: add gpu_mem_gb column for per-job GPU memory reservation
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN gpu_mem_gb INTEGER`); err != nil {
		return err
	}

	// Migration: add env vars column for storing job environment
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN env_vars TEXT`); err != nil {
		return err
	}

	// Migration: add tags column for job metadata
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN tags TEXT`); err != nil {
		return err
	}

	// Migration: add job_metadata column for derived stats
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN job_metadata TEXT`); err != nil {
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

	// Migration: add project column for storing derived project name
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN project TEXT`); err != nil {
		return err
	}

	// Backfill project for recent jobs (last 24 hours)
	if err := backfillRecentProjects(db); err != nil {
		return err
	}
	// Migration: add queued_at column for queue ordering (independent of job ID)
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN queued_at INTEGER`); err != nil {
		return err
	}

	// Migration: add inputs/outputs columns for data locality tracking
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN inputs TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN outputs TEXT`); err != nil {
		return err
	}

	// Migration: add placement columns for coordinator-based job placement
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN placement_host TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN placement_reasons TEXT`); err != nil {
		return err
	}

	// Migration: add cost column for cloud job cost tracking
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN cost REAL`); err != nil {
		return err
	}

	// Migration: add vastai_instance_id column for Vast.ai instance tracking
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN vastai_instance_id INTEGER`); err != nil {
		return err
	}

	// Migration: add error_diagnosis column for auto-remediation diagnosis
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN error_diagnosis TEXT`); err != nil {
		return err
	}

	// Migration: add retry_count column for auto-remediation retry tracking
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN retry_count INTEGER DEFAULT 0`); err != nil {
		return err
	}

	// Migration: add output_dirs column for convention-based output collection
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN output_dirs TEXT`); err != nil {
		return err
	}

	// Migration: add produces/needs columns for artifact-based job dependencies
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN produces TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN needs TEXT`); err != nil {
		return err
	}

	// Migration: add placement_meta column for placement telemetry
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN placement_meta TEXT`); err != nil {
		return err
	}

	// Migration: track the latest execution attempt row for each logical job.
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN latest_run_id INTEGER`); err != nil {
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

	// Track when a host was last successfully synced
	hostSyncSchema := `
	CREATE TABLE IF NOT EXISTS host_syncs (
		name TEXT PRIMARY KEY,
		last_synced INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(hostSyncSchema); err != nil {
		return err
	}

	// Migration: add last_restart_check column for optimized restart detection
	if err := addColumnIfMissing(db, `ALTER TABLE host_syncs ADD COLUMN last_restart_check INTEGER DEFAULT 0`); err != nil {
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

	// Create artifacts table for cached job artifacts
	artifactsSchema := `
	CREATE TABLE IF NOT EXISTS artifacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL,
		name TEXT,
		path TEXT NOT NULL,
		stored_path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT,
		created_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_job_name_path ON artifacts(job_id, name, path);
	CREATE INDEX IF NOT EXISTS idx_artifacts_job ON artifacts(job_id);
	`
	if _, err := db.Exec(artifactsSchema); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE artifacts ADD COLUMN job_run_id INTEGER`); err != nil {
		return err
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_artifacts_job_name_path`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_job_name_path_legacy ON artifacts(job_id, name, path) WHERE job_run_id IS NULL`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_run_name_path ON artifacts(job_run_id, name, path) WHERE job_run_id IS NOT NULL`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_artifacts_run ON artifacts(job_run_id)`); err != nil {
		return err
	}

	// Create host_data table for data locality tracking
	hostDataSchema := `
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
	`
	if _, err := db.Exec(hostDataSchema); err != nil {
		return err
	}

	if err := dataloc.InitSchema(db); err != nil {
		return err
	}

	// Create job_timeseries table for per-sample telemetry data
	timeseriesSchema := `
	CREATE TABLE IF NOT EXISTS job_timeseries (
		job_id INTEGER NOT NULL,
		ts INTEGER NOT NULL,
		cpu_pct INTEGER,
		rss_kb INTEGER,
		gpu_mib INTEGER,
		disk_free_bytes INTEGER,
		disk_total_bytes INTEGER,
		host_rss_kb INTEGER,
		host_mem_total_kb INTEGER,
		gpu_util_pct INTEGER,
		gpu_mem_used_mib INTEGER,
		gpu_mem_total_mib INTEGER,
		tenant TEXT,
		PRIMARY KEY (job_id, ts)
	);
	CREATE INDEX IF NOT EXISTS idx_job_timeseries_job ON job_timeseries(job_id);
	`
	if _, err := db.Exec(timeseriesSchema); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE job_timeseries ADD COLUMN job_run_id INTEGER`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_job_timeseries_run ON job_timeseries(job_run_id)`); err != nil {
		return err
	}

	// Migration: add GPU temperature and clock frequency to timeseries
	if err := addColumnIfMissing(db, `ALTER TABLE job_timeseries ADD COLUMN gpu_temp_c INTEGER`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE job_timeseries ADD COLUMN gpu_clock_mhz INTEGER`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE job_timeseries ADD COLUMN disk_free_bytes INTEGER`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE job_timeseries ADD COLUMN disk_total_bytes INTEGER`); err != nil {
		return err
	}

	telemetrySchema := `
	CREATE TABLE IF NOT EXISTS job_telemetry_samples (
		job_id INTEGER NOT NULL,
		job_run_id INTEGER NOT NULL,
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
		PRIMARY KEY (job_run_id, ts)
	);
	CREATE INDEX IF NOT EXISTS idx_job_telemetry_samples_job ON job_telemetry_samples(job_id);
	CREATE INDEX IF NOT EXISTS idx_job_telemetry_samples_run ON job_telemetry_samples(job_run_id);
	CREATE TABLE IF NOT EXISTS job_telemetry_gpus (
		job_id INTEGER NOT NULL,
		job_run_id INTEGER NOT NULL,
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
		PRIMARY KEY (job_run_id, ts, gpu_index)
	);
	CREATE INDEX IF NOT EXISTS idx_job_telemetry_gpus_job ON job_telemetry_gpus(job_id);
	CREATE INDEX IF NOT EXISTS idx_job_telemetry_gpus_run ON job_telemetry_gpus(job_run_id);
	`
	if _, err := db.Exec(telemetrySchema); err != nil {
		return err
	}

	// Create campaigns table (batch of cloud instances)
	campaignsBatchSchema := createCampaignsTableSQL("campaigns", true)
	if _, err := db.Exec(campaignsBatchSchema); err != nil {
		return err
	}

	// Create cloud_instances table (individual cloud GPU deployments)
	cloudInstancesSchema := createCloudInstancesTableSQL("cloud_instances", true)
	if _, err := db.Exec(cloudInstancesSchema); err != nil {
		return err
	}

	// Migration: add cloud_instance_id column to jobs
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN cloud_instance_id INTEGER`); err != nil {
		return err
	}

	// Create job_phase_timings table
	jobPhaseTimingsSchema := `
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
		output_upload_files INTEGER,
		output_upload_retries INTEGER,
		output_upload_duration_ms INTEGER,
		results_upload_files INTEGER,
		results_upload_retries INTEGER,
		results_upload_duration_ms INTEGER,
		cache_hf_bytes INTEGER,
		cache_uv_bytes INTEGER,
		peak_gpu_mem_mib INTEGER,
		mean_gpu_util INTEGER,
		peak_gpu_util INTEGER
	);
	`
	if _, err := db.Exec(jobPhaseTimingsSchema); err != nil {
		return err
	}

	// Migration: add uv sync timing, post-job cache size, and disk usage columns
	for _, stmt := range []string{
		`ALTER TABLE job_phase_timings ADD COLUMN uv_sync_seconds INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN cache_uv_post_bytes INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN cache_hf_post_bytes INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN disk_used_bytes INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN disk_total_bytes INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN output_upload_files INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN output_upload_retries INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN output_upload_duration_ms INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN results_upload_files INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN results_upload_retries INTEGER`,
		`ALTER TABLE job_phase_timings ADD COLUMN results_upload_duration_ms INTEGER`,
	} {
		if err := addColumnIfMissing(db, stmt); err != nil {
			return err
		}
	}

	// Migration: add campaign_job_index to jobs
	if err := addColumnIfMissing(db, `ALTER TABLE jobs ADD COLUMN campaign_job_index INTEGER`); err != nil {
		return err
	}

	// Migration: add offer metadata and phase timing columns to cloud_instances
	cloudInstanceMigrations := []string{
		`ALTER TABLE cloud_instances ADD COLUMN ready_at INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN resolved_gpu_name TEXT`,
		`ALTER TABLE cloud_instances ADD COLUMN cost_per_hour_cents INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN num_gpus INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN dl_perf REAL`,
		`ALTER TABLE cloud_instances ADD COLUMN reliability REAL`,
		`ALTER TABLE cloud_instances ADD COLUMN inet_down_mbps REAL`,
		`ALTER TABLE cloud_instances ADD COLUMN inet_up_mbps REAL`,
		`ALTER TABLE cloud_instances ADD COLUMN cuda_version REAL`,
		`ALTER TABLE cloud_instances ADD COLUMN provider_instance_id TEXT`,
		`ALTER TABLE cloud_instances ADD COLUMN data_center TEXT`,
		`ALTER TABLE cloud_instances ADD COLUMN instance_role TEXT DEFAULT 'worker'`,
		`ALTER TABLE cloud_instances ADD COLUMN donor_instance_id INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN seed_download_secs INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN seed_copy_secs INTEGER`,
	}
	for _, stmt := range cloudInstanceMigrations {
		if err := addColumnIfMissing(db, stmt); err != nil {
			return err
		}
	}

	// Migration: add grace period columns to cloud_instances
	graceMigrations := []string{
		`ALTER TABLE cloud_instances ADD COLUMN grace_period_seconds INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN grace_started_at INTEGER`,
		`ALTER TABLE cloud_instances ADD COLUMN grace_deadline INTEGER`,
	}
	for _, stmt := range graceMigrations {
		if err := addColumnIfMissing(db, stmt); err != nil {
			return err
		}
	}

	// Migration: add termination_reason column to cloud_instances
	if err := addColumnIfMissing(db, `ALTER TABLE cloud_instances ADD COLUMN termination_reason TEXT`); err != nil {
		return err
	}

	// Migration: add estimated_cost_cents to campaigns
	if err := addColumnIfMissing(db, `ALTER TABLE campaigns ADD COLUMN estimated_cost_cents INTEGER`); err != nil {
		return err
	}

	// Migration: add disk_gb and provisioned_inputs to cloud_instances for instance reuse
	if err := addColumnIfMissing(db, `ALTER TABLE cloud_instances ADD COLUMN disk_gb INTEGER`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE cloud_instances ADD COLUMN provisioned_inputs TEXT`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE cloud_instances ADD COLUMN termination_requested_at INTEGER`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, `ALTER TABLE cloud_instances ADD COLUMN termination_intent_json TEXT`); err != nil {
		return err
	}

	// Backfill provider_instance_id from vastai_instance_id
	if _, err := db.Exec(`UPDATE cloud_instances SET provider_instance_id = vastai_instance_id WHERE provider_instance_id IS NULL AND vastai_instance_id IS NOT NULL`); err != nil {
		return err
	}

	// Create job_cloud_attempts table (tracks each job ↔ cloud instance association)
	jobCloudAttemptsSchema := createJobCloudAttemptsTableSQL("job_cloud_attempts", true)
	if _, err := db.Exec(jobCloudAttemptsSchema); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_job_cloud_attempts_instance ON job_cloud_attempts(cloud_instance_id)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_job_cloud_attempts_job ON job_cloud_attempts(job_id)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_job_cloud_attempts_open ON job_cloud_attempts(job_id, ended_at, started_at DESC, id DESC)`); err != nil {
		return err
	}

	// Create job_runs table (archives overwritten execution state when a logical
	// job row is reused for another attempt).
	jobRunsSchema := `
	CREATE TABLE IF NOT EXISTS job_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NOT NULL REFERENCES jobs(id),
		archived_at INTEGER NOT NULL,
		archive_reason TEXT NOT NULL,
		status TEXT NOT NULL,
		host TEXT NOT NULL,
		working_dir TEXT NOT NULL,
		command TEXT NOT NULL,
		description TEXT,
		session_name TEXT,
		queue_name TEXT,
		backend TEXT,
		remote_id TEXT,
		remote_state TEXT,
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
		start_time INTEGER,
		end_time INTEGER,
		exit_code INTEGER,
		error_message TEXT,
		failure_reason TEXT,
		error_diagnosis TEXT,
		job_metadata TEXT,
		placement_meta TEXT,
		cost REAL,
		vastai_instance_id INTEGER,
		retry_count INTEGER DEFAULT 0,
		cloud_instance_id INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_job_runs_job ON job_runs(job_id, archived_at, id);
	`
	if _, err := db.Exec(jobRunsSchema); err != nil {
		return err
	}
	for _, stmt := range []string{
		`ALTER TABLE job_runs ADD COLUMN description TEXT`,
		`ALTER TABLE job_runs ADD COLUMN gpu TEXT`,
		`ALTER TABLE job_runs ADD COLUMN gpu_class TEXT`,
		`ALTER TABLE job_runs ADD COLUMN cpu_allotment INTEGER`,
		`ALTER TABLE job_runs ADD COLUMN gpu_mem_gb INTEGER`,
		`ALTER TABLE job_runs ADD COLUMN env_vars TEXT`,
		`ALTER TABLE job_runs ADD COLUMN tags TEXT`,
		`ALTER TABLE job_runs ADD COLUMN dep_spec TEXT`,
		`ALTER TABLE job_runs ADD COLUMN inputs TEXT`,
		`ALTER TABLE job_runs ADD COLUMN outputs TEXT`,
		`ALTER TABLE job_runs ADD COLUMN output_dirs TEXT`,
		`ALTER TABLE job_runs ADD COLUMN produces TEXT`,
		`ALTER TABLE job_runs ADD COLUMN needs TEXT`,
		`ALTER TABLE job_runs ADD COLUMN project TEXT`,
		`ALTER TABLE job_runs ADD COLUMN error_diagnosis TEXT`,
		`ALTER TABLE job_runs ADD COLUMN job_metadata TEXT`,
		`ALTER TABLE job_runs ADD COLUMN placement_meta TEXT`,
		`ALTER TABLE job_runs ADD COLUMN cost REAL`,
		`ALTER TABLE job_runs ADD COLUMN vastai_instance_id INTEGER`,
		`ALTER TABLE job_runs ADD COLUMN retry_count INTEGER DEFAULT 0`,
	} {
		if err := addColumnIfMissing(db, stmt); err != nil {
			return err
		}
	}

	if _, err := db.Exec(`DROP VIEW IF EXISTS job_run_training_examples`); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE VIEW job_run_training_examples AS
		SELECT
			id AS run_id,
			job_id,
			archived_at,
			archive_reason,
			status,
			host,
			working_dir,
			command,
			description,
			gpu,
			gpu_class,
			cpu_allotment,
			gpu_mem_gb,
			env_vars,
			tags,
			dep_spec,
			inputs,
			outputs,
			output_dirs,
			produces,
			needs,
			project,
			COALESCE(backend, 'queue-runner') AS backend,
			CASE WHEN COALESCE(backend, 'queue-runner') = 'vastai' THEN 'single' ELSE 'multi' END AS tenant,
			start_time,
			end_time,
			CASE
				WHEN start_time IS NOT NULL AND end_time IS NOT NULL THEN end_time - start_time
				ELSE NULL
			END AS duration_s,
			exit_code,
			job_metadata,
			placement_meta,
			cost,
			vastai_instance_id,
			retry_count,
			cloud_instance_id,
			error_message,
			failure_reason,
			error_diagnosis
		FROM job_runs
		WHERE start_time IS NOT NULL AND end_time IS NOT NULL
	`); err != nil {
		return err
	}
	if err := backfillLegacyJobRuns(db); err != nil {
		return err
	}
	if err := repairPlaceholderProjects(db); err != nil {
		return err
	}

	// Migration: convert legacy needs_rental status to queued (host is already empty)
	if _, err := db.Exec(`UPDATE jobs SET status = ? WHERE status = ?`, StatusQueued, statusNeedsRental); err != nil {
		return err
	}
	if err := repairLegacyCloudPlacementHosts(db); err != nil {
		return err
	}
	if err := detectMultipleOpenCloudAttempts(db); err != nil {
		return err
	}
	if err := repairLiveCloudAssignments(db); err != nil {
		return err
	}
	if err := validateEnumAndRelationshipConstraints(db); err != nil {
		return err
	}
	if err := dropIntegrityViewsAndTriggers(db); err != nil {
		return err
	}
	if err := ensureJobsTableConstraints(db); err != nil {
		return err
	}
	if err := ensureCampaignsTableConstraints(db); err != nil {
		return err
	}
	if err := ensureCloudInstancesTableConstraints(db); err != nil {
		return err
	}
	if err := ensureJobCloudAttemptsTableConstraints(db); err != nil {
		return err
	}
	if err := createJobStateViews(db); err != nil {
		return err
	}
	if err := createCloudAttemptTriggers(db); err != nil {
		return err
	}
	if err := createIntegrityTriggers(db); err != nil {
		return err
	}

	// Transfer bandwidth observations (used by internal/transferbw package).
	// Defined here to avoid import cycle: db → transferbw → estimate → db.
	if _, err := db.Exec(`
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
		)
	`); err != nil {
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
			backend TEXT DEFAULT 'queue-runner',
			remote_id TEXT,
			remote_state TEXT,
			failure_reason TEXT,
			queue_name TEXT,
			cpu_allotment INTEGER,
			tags TEXT,
			tombstoned INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO jobs_new SELECT id, host, session_name, working_dir, command, description,
			start_time, end_time, exit_code, status, error_message, backend, remote_id, remote_state, failure_reason, queue_name, cpu_allotment, NULL, tombstoned FROM jobs`,
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
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, start_time, status)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, now, now, StatusStarting,
	)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), jobID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := startNewLatestRunTx(tx, job, "run_start"); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return jobID, nil
}

// UpdateJobRunning transitions a starting job to running
func UpdateJobRunning(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusStarting,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, ""); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateJobFailed marks a starting job as failed to start
func UpdateJobFailed(db *sql.DB, id int64, errorMsg string) error {
	endTime := time.Now().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// Store error in error_message column (not description) for debugging
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, end_time = ?, error_message = ? WHERE id = ? AND status = ?`,
		StatusDead, endTime, errorMsg, id, StatusStarting,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusDead)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, "failed_to_start"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateJobStartingToQueued transitions a starting job to queued state and assigns a queue name.
func UpdateJobStartingToQueued(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusStarting)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "starting_to_queued"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, queue_name = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL,
		 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
		 WHERE id = ? AND status = ?`,
		StatusQueued, queuefile.DefaultQueueName, id, StatusStarting,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateJobRunningToQueued transitions a running job back to queued state.
func UpdateJobRunningToQueued(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "running_to_queued"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, queue_name = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL,
		 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
		 WHERE id = ? AND status = ?`,
		StatusQueued, queuefile.DefaultQueueName, id, StatusRunning,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
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
	var err error
	workingDir, err = workdir.Normalize(workingDir)
	if err != nil {
		return err
	}
	_, err = db.Exec(
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
// Also updates last_synced_status since recording completion is a sync operation.
// Note: Also accepts failed/dead status because a status file appearing is authoritative
// evidence of completion, even if the job was previously marked as failed due to race conditions.
func RecordCompletionByID(db *sql.DB, id int64, exitCode int, endTime int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?, last_synced_status = ?, pending_status = NULL, session_name = NULL
		 WHERE id = ? AND status IN (?, ?, ?, ?, ?, ?)`,
		exitCode, endTime, StatusCompleted, StatusCompleted, id, StatusRunning, StatusStarting, StatusQueued, StatusPaused, StatusFailed, StatusDead,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, ""); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MarkDeadByID marks a running or queued job as failed (unexpected termination) by ID.
// Clears session_name per spec: SessionImpliesRunning (session => status = running).
// Also updates last_synced_status since this is detecting remote state.
func MarkDeadByID(db *sql.DB, id int64) error {
	endTime := time.Now().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET end_time = ?, status = ?, last_synced_status = ?, pending_status = NULL, session_name = NULL
		 WHERE id = ? AND status IN (?, ?, ?, ?)`,
		endTime, StatusFailed, StatusFailed, id, StatusRunning, StatusStarting, StatusQueued, StatusPaused,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, ""); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetJobVastaiInstance stores the Vast.ai instance ID and backend on a job record.
// This should be called immediately after creating the instance, before any other work.
func SetJobVastaiInstance(db *sql.DB, jobID int64, instanceID int) error {
	_, err := db.Exec(
		`UPDATE jobs SET vastai_instance_id = ?, backend = ? WHERE id = ?`,
		instanceID, BackendVastai, jobID,
	)
	if err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, jobID, "")
}

// SetJobCost updates the actual cost for a cloud-run job.
func SetJobCost(db *sql.DB, jobID int64, cost float64) error {
	_, err := db.Exec(`UPDATE jobs SET cost = ? WHERE id = ?`, cost, jobID)
	if err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, jobID, "")
}

// SetJobPlacementMeta stores placement telemetry on a job record.
func SetJobPlacementMeta(db *sql.DB, jobID int64, meta *PlacementMeta) error {
	encoded, err := encodePlacementMeta(meta)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET placement_meta = ? WHERE id = ?`, encoded, jobID)
	if err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, jobID, "")
}

// SetJobPlacementReasons stores why a job is currently unplaced.
func SetJobPlacementReasons(db *sql.DB, jobID int64, reasons []string) error {
	encoded := encodeStringSlice(reasons)
	_, err := db.Exec(`UPDATE jobs SET placement_reasons = ? WHERE id = ?`, encoded, jobID)
	return err
}

func encodePlacementMeta(meta *PlacementMeta) (any, error) {
	if meta == nil {
		return nil, nil
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode placement_meta: %w", err)
	}
	return string(data), nil
}

func decodePlacementMeta(value sql.NullString) *PlacementMeta {
	if !value.Valid || value.String == "" {
		return nil
	}
	var meta PlacementMeta
	if err := json.Unmarshal([]byte(value.String), &meta); err != nil {
		return nil
	}
	return &meta
}

// ListActiveVastaiJobs returns jobs with backend=vastai that have an instance ID
// and are in a non-terminal status.
func ListActiveVastaiJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE backend = ? AND vastai_instance_id IS NOT NULL AND status NOT IN (?, ?, ?, ?) AND tombstoned = 0 ORDER BY id`, jobSelectColumns)
	rows, err := db.Query(query, BackendVastai, StatusCompleted, StatusFailed, StatusKilled, StatusCanceled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ListActiveCloudJobs returns jobs associated with a cloud instance that are in a non-terminal status.
// This covers both legacy vastai-backend jobs and campaign-launched queue-runner jobs.
func ListActiveCloudJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_effective_state
		WHERE effective_target_kind = ?
		  AND status NOT IN (?, ?, ?, ?)
		  AND tombstoned = 0
		ORDER BY id`, qualifiedJobSelectColumns("job_effective_state"))
	rows, err := db.Query(query, string(JobTargetRentalInstance), StatusCompleted, StatusFailed, StatusKilled, StatusCanceled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
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

// MarkPausedByID transitions a job to paused from queued/starting/running.
func MarkPausedByID(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status IN (?, ?, ?)`,
		StatusPaused, id, StatusQueued, StatusStarting, StatusRunning,
	)
	return err
}

// MarkPausedFromTerminal transitions a job from a terminal status back to paused.
func MarkPausedFromTerminal(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, end_time = NULL, exit_code = NULL, error_message = NULL WHERE id = ? AND status IN (?, ?, ?, ?)`,
		StatusPaused, id, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
	)
	return err
}

// MarkRunningFromPaused transitions a job from paused back to running.
func MarkRunningFromPaused(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`,
		StatusRunning, id, StatusPaused,
	)
	return err
}

// MarkRunningFromTerminal transitions a job from a terminal status (failed, dead, etc.) back to running.
// This handles jobs that were restarted by the queue runner after previously failing.
func MarkRunningFromTerminal(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status IN (?, ?, ?, ?)`, jobSelectColumns), id, StatusFailed, StatusDead, StatusKilled, StatusCanceled)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "mark_running_from_terminal"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL,
		 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL, session_name = NULL
		 WHERE id = ? AND status IN (?, ?, ?, ?)`,
		StatusRunning, StatusRunning, id, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err = queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job != nil {
		if err := startNewLatestRunTx(tx, job, "run_start"); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// MarkQueuedJobRunning transitions a queued job to running without touching start_time.
// Called from sync when detecting a job has started running remotely.
// Updates last_synced_status since this is a sync operation.
func MarkQueuedJobRunning(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ? WHERE id = ? AND status = ?`,
		StatusRunning, StatusRunning, id, StatusQueued,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := startNewLatestRunTx(tx, job, "run_start"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MarkQueuedByID resets a job back to queued status (e.g., when sync finds it's still in queue)
// Updates last_synced_status since this is a sync operation.
func MarkQueuedByID(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "mark_queued"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ?, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL,
		 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
		 WHERE id = ?`,
		StatusQueued, StatusQueued, id,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
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
		`UPDATE jobs SET last_synced_status = ? WHERE id = ? AND status NOT IN (?, ?, ?, ?)`,
		status, jobID, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
	)
	return err
}

// ResetLastSyncedStatus clears last_synced_status for a job, marking it as
// needing re-dispatch. Used when the runner state shows a job is missing despite
// the DB believing it was already dispatched.
func ResetLastSyncedStatus(db *sql.DB, jobID int64) error {
	_, err := db.Exec(`UPDATE jobs SET last_synced_status = NULL WHERE id = ?`, jobID)
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
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), jobID)
		if err != nil {
			tx.Rollback()
			return err
		}
		if job == nil {
			return tx.Commit()
		}
		reason := "clear_pending_to_queued"
		if status == StatusDraft {
			reason = "clear_pending_to_draft"
		}
		if err := archiveJobRunTx(tx, job, reason); err != nil {
			tx.Rollback()
			return err
		}
		// Reset all execution-related fields when going back to queued/draft
		_, err = tx.Exec(
			`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL,
			 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL
			 WHERE id = ?`,
			status, status, jobID,
		)
		if err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	if IsTerminalStatus(status) {
		// Clear session_name for terminal states per spec: SessionImpliesRunning
		now := time.Now().Unix()
		_, err := db.Exec(
			`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL, session_name = NULL,
			 end_time = CASE WHEN end_time IS NULL OR end_time = 0 THEN ? ELSE end_time END
			 WHERE id = ?`,
			status, status, now, jobID,
		)
		return err
	}
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, last_synced_status = ?, pending_status = NULL, pending_at = NULL WHERE id = ?`,
		status, status, jobID,
	)
	return err
}

// RequeueByID resets a job back to queued status for user-initiated requeue.
// Unlike MarkQueuedByID, this does NOT set last_synced_status to queued,
// so the sync path will re-append the job to the remote queue if the immediate
// append fails.
func RequeueByID(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "requeue"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, pending_status = ?, last_synced_status = NULL, start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL, session_name = NULL,
		 failure_reason = NULL, error_diagnosis = NULL, remote_state = NULL, remote_id = NULL, cloud_instance_id = NULL
		 WHERE id = ?`,
		StatusQueued, StatusQueued, id,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MoveQueuedJobToUnplaced clears a queued job's host assignment. Jobs that are
// not inventory-only gain the rental placement tag so they remain eligible for
// rental launch workflows. Unlike ResetJobToUnplaced, this does not archive a
// run because the job has not started; it only clears queue placement metadata.
func MoveQueuedJobToUnplaced(db *sql.DB, id int64) error {
	job, err := GetJobByID(db, id)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}
	tags := append([]string(nil), job.Tags...)
	if !job.HasTag(TagInventory) && !job.HasTag(TagRental) {
		tags = append(tags, TagRental)
	}
	tagValue, err := encodeTags(tags)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`UPDATE jobs
		 SET host = '',
		     queue_name = NULL,
		     queued_at = NULL,
		     pending_status = NULL,
		     pending_at = NULL,
		     last_synced_status = NULL,
		     cloud_instance_id = NULL,
		     tags = ?,
		     placement_reasons = ?
		 WHERE id = ? AND status = ?`,
		tagValue, encodeStringSlice([]string{"manually moved to unplaced queue"}), id, StatusQueued,
	)
	return err
}

// ResetJobToUnplaced resets a single job to unplaced state (queued with empty host),
// clearing cloud instance association and run metadata. Used when restarting cloud
// jobs whose original instance is no longer available.
func ResetJobToUnplaced(db *sql.DB, jobID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), jobID)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := archiveJobRunTx(tx, job, "reset_to_unplaced"); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, pending_status = ?, host = '', cloud_instance_id = NULL,
		 start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL,
		 session_name = NULL, last_synced_status = NULL, failure_reason = NULL,
		 error_diagnosis = NULL, remote_state = NULL, remote_id = NULL, placement_reasons = ?
		 WHERE id = ?`,
		StatusQueued, StatusQueued, encodeStringSlice(resetJobPlacementReasons(job)), jobID,
	); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func resetJobPlacementReasons(job *Job) []string {
	if job == nil {
		return []string{"job reset to unplaced queue"}
	}
	if job.CloudInstanceID != nil && *job.CloudInstanceID > 0 {
		return []string{fmt.Sprintf("cloud instance %d unavailable; job reset to unplaced queue", *job.CloudInstanceID)}
	}
	return []string{"job reset to unplaced queue"}
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

// CountQueueRunnerActiveByHost returns queued/running queue-runner jobs for a host.
func CountQueueRunnerActiveByHost(db *sql.DB, host string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM jobs
		 WHERE host = ?
		 AND (backend IS NULL OR backend = ?)
		 AND status IN (?, ?, ?, ?)
		 AND tombstoned = 0`,
		host, BackendQueueRunner, StatusQueued, StatusRunning, StatusStarting, StatusPaused,
	).Scan(&count)
	return count, err
}

// RecordQueued records a queued job for sequential execution and returns its ID
// Note: start_time is NULL until the job actually starts running (set by UpdateQueuedToRunning)
func RecordQueued(db *sql.DB, host, workingDir, command, description string) (int64, error) {
	return RecordQueuedWithGPU(db, host, workingDir, command, description, "")
}

// RecordQueuedWithGPU records a queued job with GPU specification
func RecordQueuedWithGPU(db *sql.DB, host, workingDir, command, description, gpu string) (int64, error) {
	return recordQueuedWithGPU(db, 0, host, workingDir, command, description, gpu, false)
}

// RecordQueuedWithGPUAndID records a queued job using an explicit ID.
func RecordQueuedWithGPUAndID(db *sql.DB, id int64, host, workingDir, command, description, gpu string) error {
	_, err := recordQueuedWithGPU(db, id, host, workingDir, command, description, gpu, true)
	return err
}

func recordQueuedWithGPU(db *sql.DB, id int64, host, workingDir, command, description, gpu string, explicitID bool) (int64, error) {
	if gpu == "" {
		gpu = ParseGPUFromCommandString(command)
	}
	var err error
	workingDir, err = workdir.Normalize(workingDir)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if explicitID {
		_, err := db.Exec(
			`INSERT INTO jobs (id, host, session_name, working_dir, command, description, created_at, queued_at, start_time, status, queue_name, gpu)
			 VALUES (?, ?, NULL, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			 	host = excluded.host,
			 	working_dir = excluded.working_dir,
			 	command = excluded.command,
			 	description = excluded.description,
			 	queued_at = excluded.queued_at,
			 	status = excluded.status,
			 	queue_name = excluded.queue_name,
			 	gpu = excluded.gpu`,
			id, host, workingDir, command, description, now, now, StatusQueued, queuefile.DefaultQueueName, gpu,
		)
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, queued_at, start_time, status, queue_name, gpu)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
		host, workingDir, command, description, now, now, StatusQueued, queuefile.DefaultQueueName, gpu,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// RecordDraftJobWithGPU records a job that should remain in draft locally.
func RecordDraftJobWithGPU(db *sql.DB, host, workingDir, command, description, gpu string) (int64, error) {
	return RecordDraftJob(db, host, workingDir, command, description, gpu, "")
}

// RecordDraftJob records a job that should remain in draft locally, with optional dependency.
func RecordDraftJob(db *sql.DB, host, workingDir, command, description, gpu, depSpec string) (int64, error) {
	if gpu == "" {
		gpu = ParseGPUFromCommandString(command)
	}
	var err error
	workingDir, err = workdir.Normalize(workingDir)
	if err != nil {
		return 0, err
	}
	createdAt := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO jobs (host, session_name, working_dir, command, description, created_at, start_time, status, queue_name, gpu, dep_spec, last_synced_status)
		 VALUES (?, NULL, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		host, workingDir, command, description, createdAt, StatusDraft, queuefile.DefaultQueueName, gpu, depSpec, StatusDraft,
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

// SetJobGPUClass updates the GPU class field for a job
func SetJobGPUClass(db *sql.DB, jobID int64, gpuClass string) error {
	_, err := db.Exec(`UPDATE jobs SET gpu_class = ? WHERE id = ?`, gpuClass, jobID)
	return err
}

// SetJobBackend sets the execution backend for a job.
func SetJobBackend(db *sql.DB, jobID int64, backend string) error {
	if backend == "" {
		backend = BackendQueueRunner
	}
	_, err := db.Exec(`UPDATE jobs SET backend = ? WHERE id = ?`, backend, jobID)
	return err
}

// SetJobRemoteID sets the backend-specific job identifier.
func SetJobRemoteID(db *sql.DB, jobID int64, remoteID string) error {
	_, err := db.Exec(`UPDATE jobs SET remote_id = ? WHERE id = ?`, remoteID, jobID)
	return err
}

// SetJobRemoteState sets the backend-specific state and optional failure reason.
func SetJobRemoteState(db *sql.DB, jobID int64, remoteState, failureReason string) error {
	_, err := db.Exec(`UPDATE jobs SET remote_state = ?, failure_reason = ? WHERE id = ?`, remoteState, failureReason, jobID)
	return err
}

// SetJobCPUAllotment updates the CPU allotment percent for a job (nil clears it).
func SetJobCPUAllotment(db *sql.DB, jobID int64, allotment *int) error {
	var value interface{}
	if allotment != nil {
		value = *allotment
	}
	_, err := db.Exec(`UPDATE jobs SET cpu_allotment = ? WHERE id = ?`, value, jobID)
	return err
}

// SetJobGPUMemGB updates the GPU memory reservation in GB per device (nil clears it).
func SetJobGPUMemGB(db *sql.DB, jobID int64, gpuMemGB *int) error {
	var value interface{}
	if gpuMemGB != nil {
		value = *gpuMemGB
	}
	_, err := db.Exec(`UPDATE jobs SET gpu_mem_gb = ? WHERE id = ?`, value, jobID)
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

// SetJobTags updates the stored tags for a job.
// The values are stored as a JSON array; passing nil or an empty slice clears the field.
func SetJobTags(db *sql.DB, jobID int64, tags []string) error {
	value, err := encodeTags(tags)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET tags = ? WHERE id = ?`, value, jobID)
	return err
}

// SetJobMetadata updates the stored metadata JSON for a job (nil clears it).
func SetJobMetadata(db *sql.DB, jobID int64, meta *JobMetadata) error {
	value, err := encodeJobMetadata(meta)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET job_metadata = ? WHERE id = ?`, value, jobID)
	if err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, jobID, "")
}

// AddJobTag adds a tag to a job if it doesn't already exist.
func AddJobTag(db *sql.DB, jobID int64, tag string) error {
	tag = CanonicalizeTag(tag)
	if tag == "" {
		return fmt.Errorf("tag cannot be empty")
	}
	job, err := GetJobByID(db, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}
	for _, existing := range job.Tags {
		if existing == tag {
			return nil
		}
	}
	updated := append(append([]string(nil), job.Tags...), tag)
	return SetJobTags(db, jobID, updated)
}

// RemoveJobTag removes a tag from a job if present.
func RemoveJobTag(db *sql.DB, jobID int64, tag string) error {
	tag = CanonicalizeTag(tag)
	if tag == "" {
		return fmt.Errorf("tag cannot be empty")
	}
	job, err := GetJobByID(db, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}
	if len(job.Tags) == 0 {
		return nil
	}
	updated := make([]string, 0, len(job.Tags))
	for _, existing := range job.Tags {
		if existing != tag {
			updated = append(updated, existing)
		}
	}
	return SetJobTags(db, jobID, updated)
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

// SetJobInputs sets the data asset inputs for a job (stored as JSON array).
func SetJobInputs(db *sql.DB, jobID int64, inputs []string) error {
	return setJobStringSlice(db, jobID, "inputs", inputs)
}

// SetJobOutputs sets the data asset outputs for a job (stored as JSON array).
func SetJobOutputs(db *sql.DB, jobID int64, outputs []string) error {
	return setJobStringSlice(db, jobID, "outputs", outputs)
}

// SetJobOutputDirs sets the convention-based output directories for a job (stored as JSON array).
func SetJobOutputDirs(db *sql.DB, jobID int64, dirs []string) error {
	return setJobStringSlice(db, jobID, "output_dirs", dirs)
}

// SetJobProduces sets the artifact specs this job produces (stored as JSON array).
func SetJobProduces(db *sql.DB, jobID int64, produces []string) error {
	return setJobStringSlice(db, jobID, "produces", produces)
}

// SetJobNeeds sets the artifact specs this job needs (stored as JSON array).
func SetJobNeeds(db *sql.DB, jobID int64, needs []string) error {
	return setJobStringSlice(db, jobID, "needs", needs)
}

func setJobStringSlice(db *sql.DB, jobID int64, column string, values []string) error {
	if len(values) == 0 {
		_, err := db.Exec(fmt.Sprintf(`UPDATE jobs SET %s = NULL WHERE id = ?`, column), jobID)
		return err
	}
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`UPDATE jobs SET %s = ? WHERE id = ?`, column), string(data), jobID)
	return err
}

// NormalizeProjectName resolves placeholder project values such as "." to a
// stable derived project name using the working directory or command.
func NormalizeProjectName(project, workingDir, command string) (string, error) {
	project = strings.TrimSpace(project)
	if project != "" && project != "." {
		return project, nil
	}
	if strings.TrimSpace(workingDir) != "" {
		derived, err := workdir.ResolveProjectName("", workingDir)
		if err != nil {
			return "", err
		}
		derived = strings.TrimSpace(derived)
		if derived != "" && derived != "." {
			return derived, nil
		}
	}
	derived := strings.TrimSpace(DeriveProject(workingDir, command))
	if derived == "." {
		return "", nil
	}
	return derived, nil
}

// SetJobProject sets the project name for a job.
func SetJobProject(db *sql.DB, jobID int64, project string) error {
	var workingDir, command string
	if err := db.QueryRow(`SELECT working_dir, command FROM jobs WHERE id = ?`, jobID).Scan(&workingDir, &command); err != nil {
		return err
	}
	normalized, err := NormalizeProjectName(project, workingDir, command)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, normalized, jobID); err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, jobID, "")
}

// DeriveProject computes the project name from a working directory and command.
// It checks for a "cd <dir> &&" prefix first, then falls back to the working
// directory basename. This function is used for stored job records (which may
// have remote paths) and must not spawn subprocesses.
func DeriveProject(workingDir, command string) string {
	_, cdDir := ParseCdPrefix(command)
	if cdDir != "" {
		return projectBase(cdDir)
	}
	if workingDir != "" {
		return projectBase(workingDir)
	}
	return ""
}

func projectBase(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." {
		return ""
	}
	return base
}

// FilterJobsByProject filters jobs by project name. Empty project returns all jobs.
func FilterJobsByProject(jobs []*Job, project string) []*Job {
	if project == "" {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		jobProject := job.Project
		if jobProject == "" {
			jobProject = DeriveProject(job.WorkingDir, job.Command)
		}
		if jobProject == project {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

// backfillRecentProjects populates the project column for recent jobs that don't have it set.
func backfillRecentProjects(db *sql.DB) error {
	cutoff := time.Now().Unix() - 86400
	rows, err := db.Query(`SELECT id, working_dir, command FROM jobs WHERE project IS NULL AND created_at > ?`, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var workingDir, command string
		if err := rows.Scan(&id, &workingDir, &command); err != nil {
			return err
		}
		project, err := NormalizeProjectName("", workingDir, command)
		if err != nil {
			return err
		}
		if project != "" {
			if _, err := db.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, project, id); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func repairPlaceholderProjects(db *sql.DB) error {
	for _, table := range []string{"jobs", "job_runs"} {
		if err := repairPlaceholderProjectsInTable(db, table); err != nil {
			return err
		}
	}
	return nil
}

func repairPlaceholderProjectsInTable(db *sql.DB, table string) error {
	rows, err := db.Query(fmt.Sprintf(`SELECT id, working_dir, command FROM %s WHERE TRIM(COALESCE(project, '')) = '.'`, table))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var workingDir, command string
		if err := rows.Scan(&id, &workingDir, &command); err != nil {
			return err
		}
		project, err := NormalizeProjectName("", workingDir, command)
		if err != nil {
			return err
		}
		if project == "" {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET project = ? WHERE id = ?`, table), project, id); err != nil {
			return err
		}
	}
	return rows.Err()
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
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND host = ? AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued, host)
}

// UpdateQueuedToRunning transitions a queued job to running
func UpdateQueuedToRunning(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, start_time = ? WHERE id = ? AND status = ?`,
		StatusRunning, time.Now().Unix(), id, StatusQueued,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := startNewLatestRunTx(tx, job, "run_start"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateQueuedToRunningWithSession transitions a queued job to running and sets session_name.
// Used when starting a queued job directly via tmux (not through queue runner).
func UpdateQueuedToRunningWithSession(db *sql.DB, id int64, sessionName string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET status = ?, start_time = ?, session_name = ? WHERE id = ? AND status = ?`,
		StatusRunning, time.Now().Unix(), sessionName, id, StatusQueued,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ? AND status = ?`, jobSelectColumns), id, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := startNewLatestRunTx(tx, job, "run_start"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ClearSessionName removes the session_name from a job.
// Used when a job that was started via tmux is now being managed by the queue runner.
func ClearSessionName(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE jobs SET session_name = NULL WHERE id = ?`, id)
	return err
}

// RecordCompletion updates a job with its exit code and end time.
// Also updates last_synced_status since this is a sync operation.
func RecordCompletion(db *sql.DB, host, sessionName string, exitCode int, endTime int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET exit_code = ?, end_time = ?, status = ?, last_synced_status = ?, pending_status = NULL
		 WHERE host = ? AND session_name = ? AND status = ?`,
		exitCode, endTime, StatusCompleted, StatusCompleted, host, sessionName, StatusRunning,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND session_name = ? AND status = ? ORDER BY id DESC LIMIT 1`, jobSelectColumns), host, sessionName, StatusCompleted)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, ""); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MarkDead marks a running job as failed (unexpected termination).
// Also updates last_synced_status since this is a sync operation.
func MarkDead(db *sql.DB, host, sessionName string) error {
	endTime := time.Now().Unix()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET end_time = ?, status = ?, last_synced_status = ?, pending_status = NULL
		 WHERE host = ? AND session_name = ? AND status = ?`,
		endTime, StatusFailed, StatusFailed, host, sessionName, StatusRunning,
	); err != nil {
		tx.Rollback()
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND session_name = ? AND status = ? ORDER BY id DESC LIMIT 1`, jobSelectColumns), host, sessionName, StatusFailed)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, ""); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateStartTime updates the start_time for a job (for jobs where start_time was initially null/0)
func UpdateStartTime(db *sql.DB, id int64, startTime int64) error {
	_, err := db.Exec(
		`UPDATE jobs SET start_time = ? WHERE id = ? AND (start_time IS NULL OR start_time = 0)`,
		startTime, id,
	)
	if err != nil {
		return err
	}
	return PersistLatestRunSnapshotIfExists(db, id, "")
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
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 1 AND status IN (?, ?, ?, ?)`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning, StatusQueued, StatusStarting, StatusPaused)
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
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status IN (?, ?) ORDER BY start_time DESC`, jobSelectColumns)
	rows, err := db.Query(query, host, StatusRunning, StatusPaused)
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
	var backend sql.NullString
	var remoteID sql.NullString
	var remoteState sql.NullString
	var failureReason sql.NullString
	var queueName sql.NullString
	var gpu sql.NullString
	var gpuClass sql.NullString
	var cpuAllotment sql.NullInt64
	var gpuMemGB sql.NullInt64
	var envVars sql.NullString
	var tags sql.NullString
	var depSpec sql.NullString
	var inputs sql.NullString
	var outputs sql.NullString
	var outputDirs sql.NullString
	var produces sql.NullString
	var needs sql.NullString
	var project sql.NullString
	var createdAt sql.NullInt64
	var queuedAt sql.NullInt64
	var startTime sql.NullInt64
	var endTime sql.NullInt64
	var exitCode sql.NullInt64
	var tombstoned sql.NullInt64
	var lastSyncedStatus sql.NullString
	var pendingStatus sql.NullString
	var pendingAt sql.NullInt64
	var jobMetadata sql.NullString
	var cost sql.NullFloat64
	var vastaiInstanceID sql.NullInt64
	var errorDiagnosis sql.NullString
	var retryCount sql.NullInt64
	var placementMeta sql.NullString
	var placementReasons sql.NullString
	var cloudInstanceID sql.NullInt64
	var campaignJobIndex sql.NullInt64
	var latestRunID sql.NullInt64

	err := row.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &generatedDesc, &generationHash, &createdAt, &queuedAt, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &backend, &remoteID, &remoteState, &failureReason, &queueName, &gpu, &gpuClass, &cpuAllotment, &gpuMemGB, &envVars, &tags, &depSpec, &inputs, &outputs, &outputDirs, &produces, &needs, &project, &tombstoned, &lastSyncedStatus, &pendingStatus, &pendingAt, &jobMetadata, &cost, &vastaiInstanceID, &errorDiagnosis, &retryCount, &placementMeta, &placementReasons, &cloudInstanceID, &campaignJobIndex, &latestRunID)
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
	if backend.Valid {
		j.Backend = backend.String
	}
	if remoteID.Valid {
		j.RemoteID = remoteID.String
	}
	if remoteState.Valid {
		j.RemoteState = remoteState.String
	}
	if failureReason.Valid {
		j.FailureReason = failureReason.String
	}
	if queueName.Valid {
		j.QueueName = queueName.String
	}
	if gpu.Valid {
		j.GPU = gpu.String
	}
	if gpuClass.Valid {
		j.GPUClass = gpuClass.String
	}
	if cpuAllotment.Valid {
		val := int(cpuAllotment.Int64)
		j.CPUAllotment = &val
	}
	if gpuMemGB.Valid {
		val := int(gpuMemGB.Int64)
		j.GPUMemGB = &val
	}
	j.EnvVars = decodeEnvVars(envVars)
	j.Tags = decodeTags(tags)
	if depSpec.Valid {
		j.DepSpec = depSpec.String
	}
	j.Inputs = decodeStringSlice(inputs)
	j.Outputs = decodeStringSlice(outputs)
	j.OutputDirs = decodeStringSlice(outputDirs)
	j.Produces = decodeStringSlice(produces)
	j.Needs = decodeStringSlice(needs)
	if project.Valid {
		j.Project = project.String
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
	j.Metadata = decodeJobMetadata(jobMetadata)
	if cost.Valid {
		j.Cost = &cost.Float64
	}
	if vastaiInstanceID.Valid {
		val := int(vastaiInstanceID.Int64)
		j.VastaiInstanceID = &val
	}
	if errorDiagnosis.Valid {
		j.ErrorDiagnosis = errorDiagnosis.String
	}
	if retryCount.Valid {
		j.RetryCount = int(retryCount.Int64)
	}
	j.PlacementMeta = decodePlacementMeta(placementMeta)
	j.PlacementReasons = decodeStringSlice(placementReasons)
	if cloudInstanceID.Valid {
		j.CloudInstanceID = &cloudInstanceID.Int64
	}
	if campaignJobIndex.Valid {
		v := int(campaignJobIndex.Int64)
		j.CampaignJobIndex = &v
	}
	if latestRunID.Valid {
		j.LatestRunID = &latestRunID.Int64
	}
	if j.Backend == "" {
		j.Backend = BackendQueueRunner
	}

	return &j, nil
}

// decodeStringSlice converts a stored JSON array string into a Go slice.
func decodeStringSlice(value sql.NullString) []string {
	if !value.Valid {
		return nil
	}
	raw := strings.TrimSpace(value.String)
	if raw == "" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil
	}
	return result
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

func encodeTags(tags []string) (interface{}, error) {
	normalized := normalizeTags(tags)
	if len(normalized) == 0 {
		return nil, nil
	}
	if err := validateReservedPlacementTags(normalized); err != nil {
		return nil, err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode tags: %w", err)
	}
	return string(data), nil
}

func decodeTags(value sql.NullString) []string {
	if !value.Valid {
		return nil
	}
	raw := strings.TrimSpace(value.String)
	if raw == "" {
		return nil
	}

	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err == nil {
		return normalizeTags(tags)
	}

	parts := strings.Split(raw, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			tags = append(tags, part)
		}
	}
	return normalizeTags(tags)
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = CanonicalizeTag(tag)
		if tag == "" {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		normalized = append(normalized, tag)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// CanonicalizeTag rewrites accepted legacy aliases to their preferred names.
func CanonicalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	switch tag {
	case TagCloudLegacy:
		return TagRental
	case TagOnPremLegacy:
		return TagInventory
	default:
		return tag
	}
}

// DisplayTags returns tags normalized to their preferred user-facing names.
func DisplayTags(tags []string) []string {
	return normalizeTags(tags)
}

// IsRentalTag reports whether the tag means "rental placement", including aliases.
func IsRentalTag(tag string) bool {
	return CanonicalizeTag(tag) == TagRental
}

// IsInventoryTag reports whether the tag means "inventory-only placement", including aliases.
func IsInventoryTag(tag string) bool {
	return CanonicalizeTag(tag) == TagInventory
}

// HasRentalTag reports whether the tag set requests rental placement semantics.
func HasRentalTag(tags []string) bool {
	for _, tag := range tags {
		if IsRentalTag(tag) {
			return true
		}
	}
	return false
}

// HasInventoryTag reports whether the tag set requests inventory-only placement.
func HasInventoryTag(tags []string) bool {
	for _, tag := range tags {
		if IsInventoryTag(tag) {
			return true
		}
	}
	return false
}

func validateReservedPlacementTags(tags []string) error {
	if HasRentalTag(tags) && HasInventoryTag(tags) {
		return fmt.Errorf("tags %q and %q cannot be combined", TagRental, TagInventory)
	}
	return nil
}

func FilterJobsByTags(jobs []*Job, tags []string, processedFilter string) []*Job {
	tags = normalizeTags(tags)
	if len(tags) == 0 && processedFilter == "" {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		if processedFilter == "processed" && !job.HasTag(ProcessedTag) {
			continue
		}
		if processedFilter == "unprocessed" && job.HasTag(ProcessedTag) {
			continue
		}
		if len(tags) > 0 {
			matches := true
			for _, tag := range tags {
				if !job.HasTag(tag) {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
		}
		filtered = append(filtered, job)
	}
	return filtered
}

// FilterJobsByExcludedTags removes jobs that contain any of the excluded tags.
func FilterJobsByExcludedTags(jobs []*Job, excluded []string) []*Job {
	excluded = normalizeTags(excluded)
	if len(excluded) == 0 {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		skip := false
		for _, tag := range excluded {
			if job.HasTag(tag) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

// FilterJobsByHosts keeps jobs whose host is in the provided list.
func FilterJobsByHosts(jobs []*Job, hosts []string) []*Job {
	if len(hosts) == 0 {
		return jobs
	}
	hostSet := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		hostSet[host] = struct{}{}
	}
	if len(hostSet) == 0 {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		if _, ok := hostSet[job.Host]; ok {
			filtered = append(filtered, job)
		}
	}
	return filtered
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
		var backend sql.NullString
		var remoteID sql.NullString
		var remoteState sql.NullString
		var failureReason sql.NullString
		var queueName sql.NullString
		var gpu sql.NullString
		var gpuClass sql.NullString
		var cpuAllotment sql.NullInt64
		var gpuMemGB sql.NullInt64
		var envVars sql.NullString
		var tags sql.NullString
		var depSpec sql.NullString
		var inputs sql.NullString
		var outputs sql.NullString
		var outputDirs sql.NullString
		var produces sql.NullString
		var needs sql.NullString
		var project sql.NullString
		var createdAt sql.NullInt64
		var queuedAt sql.NullInt64
		var startTime sql.NullInt64
		var endTime sql.NullInt64
		var exitCode sql.NullInt64
		var tombstoned sql.NullInt64
		var lastSyncedStatus sql.NullString
		var pendingStatus sql.NullString
		var pendingAt sql.NullInt64
		var jobMetadata sql.NullString
		var cost sql.NullFloat64
		var vastaiInstanceID sql.NullInt64
		var errorDiagnosis sql.NullString
		var retryCount sql.NullInt64
		var placementMeta sql.NullString
		var placementReasons sql.NullString
		var cloudInstanceID sql.NullInt64
		var campaignJobIndex sql.NullInt64
		var latestRunID sql.NullInt64

		err := rows.Scan(&j.ID, &j.Host, &sessionName, &j.WorkingDir, &j.Command, &desc, &generatedDesc, &generationHash, &createdAt, &queuedAt, &startTime, &endTime, &exitCode, &j.Status, &errorMsg, &backend, &remoteID, &remoteState, &failureReason, &queueName, &gpu, &gpuClass, &cpuAllotment, &gpuMemGB, &envVars, &tags, &depSpec, &inputs, &outputs, &outputDirs, &produces, &needs, &project, &tombstoned, &lastSyncedStatus, &pendingStatus, &pendingAt, &jobMetadata, &cost, &vastaiInstanceID, &errorDiagnosis, &retryCount, &placementMeta, &placementReasons, &cloudInstanceID, &campaignJobIndex, &latestRunID)
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
		if backend.Valid {
			j.Backend = backend.String
		}
		if remoteID.Valid {
			j.RemoteID = remoteID.String
		}
		if remoteState.Valid {
			j.RemoteState = remoteState.String
		}
		if failureReason.Valid {
			j.FailureReason = failureReason.String
		}
		if queueName.Valid {
			j.QueueName = queueName.String
		}
		if gpu.Valid {
			j.GPU = gpu.String
		}
		if gpuClass.Valid {
			j.GPUClass = gpuClass.String
		}
		if cpuAllotment.Valid {
			val := int(cpuAllotment.Int64)
			j.CPUAllotment = &val
		}
		if gpuMemGB.Valid {
			val := int(gpuMemGB.Int64)
			j.GPUMemGB = &val
		}
		j.EnvVars = decodeEnvVars(envVars)
		j.Tags = decodeTags(tags)
		if depSpec.Valid {
			j.DepSpec = depSpec.String
		}
		j.Inputs = decodeStringSlice(inputs)
		j.Outputs = decodeStringSlice(outputs)
		j.OutputDirs = decodeStringSlice(outputDirs)
		j.Produces = decodeStringSlice(produces)
		j.Needs = decodeStringSlice(needs)
		if project.Valid {
			j.Project = project.String
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
		j.Metadata = decodeJobMetadata(jobMetadata)
		if cost.Valid {
			j.Cost = &cost.Float64
		}
		if vastaiInstanceID.Valid {
			val := int(vastaiInstanceID.Int64)
			j.VastaiInstanceID = &val
		}
		if errorDiagnosis.Valid {
			j.ErrorDiagnosis = errorDiagnosis.String
		}
		if retryCount.Valid {
			j.RetryCount = int(retryCount.Int64)
		}
		j.PlacementMeta = decodePlacementMeta(placementMeta)
		j.PlacementReasons = decodeStringSlice(placementReasons)
		if cloudInstanceID.Valid {
			j.CloudInstanceID = &cloudInstanceID.Int64
		}
		if campaignJobIndex.Valid {
			v := int(campaignJobIndex.Int64)
			j.CampaignJobIndex = &v
		}
		if latestRunID.Valid {
			j.LatestRunID = &latestRunID.Int64
		}
		if j.Backend == "" {
			j.Backend = BackendQueueRunner
		}

		jobs = append(jobs, &j)
	}

	return jobs, rows.Err()
}

// ListJobs returns jobs matching the given filters
func ListJobs(db *sql.DB, status, host string, limit int, tags []string, processedFilter string) ([]*Job, error) {
	return ListJobsWithMaxAge(db, status, host, limit, 0, tags, processedFilter)
}

// ListJobsWithMaxAge returns jobs, optionally filtered by status, host, and age.
// maxAgeDays of 0 means no age limit.
func ListJobsWithMaxAge(db *sql.DB, status, host string, limit, maxAgeDays int, tags []string, processedFilter string) ([]*Job, error) {
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
		query += ` AND (start_time > ? OR start_time IS NULL OR start_time = 0)`
		args = append(args, cutoff)
	}

	// Order by running jobs first, then by job ID descending
	query += ` ORDER BY CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END, id DESC`
	applyLimit := limit > 0 && len(normalizeTags(tags)) == 0 && processedFilter == ""
	if applyLimit {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	jobs, err := queryJobs(db, query, args...)
	if err != nil {
		return nil, err
	}
	jobs = FilterJobsByTags(jobs, tags, processedFilter)
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// ListJobsWithMaxAgeForHosts returns jobs filtered by a host list.
func ListJobsWithMaxAgeForHosts(db *sql.DB, status string, hosts []string, limit, maxAgeDays int, tags []string, processedFilter string) ([]*Job, error) {
	if len(hosts) == 0 {
		return ListJobsWithMaxAge(db, status, "", limit, maxAgeDays, tags, processedFilter)
	}

	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 0`, jobSelectColumns)
	args := []interface{}{}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	placeholders := make([]string, 0, len(hosts))
	for _, host := range hosts {
		placeholders = append(placeholders, "?")
		args = append(args, host)
	}
	query += fmt.Sprintf(` AND host IN (%s)`, strings.Join(placeholders, ", "))
	if maxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -maxAgeDays).Unix()
		query += ` AND (start_time > ? OR start_time IS NULL OR start_time = 0)`
		args = append(args, cutoff)
	}

	// Order by running jobs first, then by job ID descending
	query += ` ORDER BY CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END, id DESC`
	applyLimit := limit > 0 && len(normalizeTags(tags)) == 0 && processedFilter == ""
	if applyLimit {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	jobs, err := queryJobs(db, query, args...)
	if err != nil {
		return nil, err
	}
	jobs = FilterJobsByTags(jobs, tags, processedFilter)
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
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

// ListUnprocessedJobs returns non-draft, non-tombstoned jobs without the
// reserved processed tag.
func ListUnprocessedJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs
		WHERE tombstoned = 0 AND status != ?
		ORDER BY CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END, id DESC`, jobSelectColumns)
	jobs, err := queryJobs(db, query, StatusDraft)
	if err != nil {
		return nil, err
	}
	return FilterJobsByTags(jobs, nil, "unprocessed"), nil
}

// ListRecentTerminalJobs returns terminal jobs whose end time is at or after
// the provided Unix timestamp.
func ListRecentTerminalJobs(db *sql.DB, sinceUnix int64) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs
		WHERE tombstoned = 0
		AND end_time IS NOT NULL
		AND end_time >= ?
		AND status IN (?, ?, ?, ?, ?)
		ORDER BY end_time DESC, id DESC`, jobSelectColumns)
	return queryJobs(db, query, sinceUnix, StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled)
}

// UpdateErrorDiagnosis stores an error diagnosis and increments retry count for a job.
func UpdateErrorDiagnosis(db *sql.DB, id int64, diagnosis string, retryCount int) error {
	_, err := db.Exec(
		`UPDATE jobs SET error_diagnosis = ?, retry_count = ? WHERE id = ?`,
		diagnosis, retryCount, id,
	)
	return err
}

// ListRecentFailedUndiagnosed returns recently failed jobs that have not been diagnosed yet.
// These are completed jobs with non-zero exit code, retry_count == 0, and no error_diagnosis.
func ListRecentFailedUndiagnosed(db *sql.DB, limit int) ([]*Job, error) {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	query := fmt.Sprintf(`SELECT %s FROM jobs
		WHERE tombstoned = 0
		AND end_time > ?
		AND status = ?
		AND exit_code IS NOT NULL AND exit_code != 0
		AND (retry_count IS NULL OR retry_count = 0)
		AND (error_diagnosis IS NULL OR error_diagnosis = '')
		ORDER BY end_time DESC
		LIMIT ?`, jobSelectColumns)
	return queryJobs(db, query, cutoff, StatusCompleted, limit)
}

// ListUniqueRunningHosts returns all unique hosts with running jobs
func ListUniqueRunningHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE status IN (?, ?, ?) AND tombstoned = 0`, StatusRunning, StatusStarting, StatusPaused)
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
	rows, err := db.Query(`
		SELECT DISTINCT host FROM (
			SELECT host FROM jobs
			WHERE tombstoned = 0
			AND (
				status IN (?, ?, ?, ?)
				OR (status = ? AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?))
			)
			UNION
			SELECT host FROM deferred_operations WHERE host != ''
		)
		WHERE host != ''`,
		StatusRunning, StatusStarting, StatusPaused, StatusQueued, StatusDraft, StatusDraft, StatusDraft)
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

// ListHostsWithQueueRunnerJobs returns unique hosts that have queued/running queue-runner jobs.
func ListHostsWithQueueRunnerJobs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs
		WHERE (backend IS NULL OR backend = ?)
		AND status IN (?, ?, ?, ?)
		AND tombstoned = 0`,
		BackendQueueRunner, StatusQueued, StatusRunning, StatusStarting, StatusPaused)
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
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status IN (?, ?, ?, ?) AND tombstoned = 0 ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusRunning, StatusStarting, StatusPaused, StatusQueued)
}

// ListActiveOnPremJobs returns all non-cloud active jobs with host assignments.
func ListActiveOnPremJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_effective_state
		WHERE effective_target_kind = ?
		AND status IN (?, ?, ?, ?) AND tombstoned = 0
		ORDER BY host ASC,
			CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END,
			id ASC`, qualifiedJobSelectColumns("job_effective_state"))
	return queryJobs(db, query, string(JobTargetInventoryHost), StatusRunning, StatusStarting, StatusPaused, StatusQueued)
}

// ListUnsyncedQueuedJobs returns queued jobs on a host that haven't been pushed
// to the remote queue yet (last_synced_status is not 'queued' and no pending operation).
func ListUnsyncedQueuedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? AND (last_synced_status IS NULL OR last_synced_status != ?) AND pending_status IS NULL AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusQueued, StatusQueued)
}

// ListSyncedQueuedJobs returns queued jobs on a host that were already pushed to
// the remote queue (last_synced_status = 'queued') but may be missing from the
// runner's live state (e.g. runner crashed before processing the command).
func ListSyncedQueuedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? AND last_synced_status = ? AND pending_status IS NULL AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusQueued, StatusQueued)
}

// ListDraftJobsPendingSync returns draft jobs that still need remote cleanup.
func ListDraftJobsPendingSync(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND status = ? AND tombstoned = 0 AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?) ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, StatusDraft, StatusDraft, StatusDraft)
}

// ListJobsPendingReconciliation returns jobs that have unresolved pending operations.
// These are jobs where the user requested a status change (kill, cancel, etc.) that
// may not have been applied to the remote yet.
func ListJobsPendingReconciliation(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND pending_status IS NOT NULL AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host)
}

// ListPotentiallyRestartedJobs returns jobs that may have been restarted by the queue runner.
// These are queue runner jobs (no session name) that are in terminal status (failed, dead)
// but may have been re-queued and started again.
func ListPotentiallyRestartedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE host = ? AND (backend IS NULL OR backend = ?) AND status IN (?, ?) AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, BackendQueueRunner, StatusFailed, StatusDead)
}

// ListAllQueued returns all queued jobs across all hosts
func ListAllQueued(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM jobs WHERE status = ? AND tombstoned = 0 ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued)
}

// ListUniqueHosts returns all unique hosts from all jobs
func ListUniqueHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM jobs WHERE tombstoned = 0 AND host != '' ORDER BY host`)
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
	stmt := fmt.Sprintf(`SELECT %s FROM jobs WHERE tombstoned = 0 AND (description LIKE ? OR command LIKE ?) ORDER BY start_time DESC`, jobSelectColumns)
	args := []interface{}{pattern, pattern}
	if limit > 0 {
		stmt += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryJobs(db, stmt, args...)
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

func queryJobsTx(tx *sql.Tx, query string, args ...interface{}) ([]*Job, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

func queryJobTx(tx *sql.Tx, query string, args ...interface{}) (*Job, error) {
	jobs, err := queryJobsTx(tx, query, args...)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return jobs[0], nil
}

func shouldArchiveJobRun(job *Job) bool {
	if job == nil {
		return false
	}
	if job.CloudInstanceID != nil {
		return true
	}
	if IsTerminalStatus(job.Status) {
		return true
	}
	switch job.Status {
	case StatusRunning, StatusStarting, StatusPaused:
		return true
	case StatusQueued:
		return job.StartTime != 0 || job.EndTime != nil || job.ExitCode != nil ||
			job.ErrorMessage != "" || job.FailureReason != "" || job.SessionName != ""
	}
	return job.StartTime != 0 || job.EndTime != nil || job.ExitCode != nil ||
		job.ErrorMessage != "" || job.FailureReason != "" || job.SessionName != ""
}

func backfillLegacyJobRuns(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	jobs, err := queryJobsTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE COALESCE(latest_run_id, 0) = 0 ORDER BY id ASC`, jobSelectColumns))
	if err != nil {
		return err
	}

	for _, job := range jobs {
		if !shouldArchiveJobRun(job) {
			continue
		}

		runID, err := latestArchivedRunIDTx(tx, job.ID)
		if err != nil {
			return err
		}
		if runID == nil {
			backfilledRunID, err := insertJobRunSnapshotAtTx(tx, job, "legacy_migration", legacyJobArchivedAt(job))
			if err != nil {
				return err
			}
			runID = &backfilledRunID
		}

		if err := setJobLatestRunIDTx(tx, job.ID, *runID); err != nil {
			return err
		}
		if err := attachLegacyTimeseriesToRunTx(tx, job.ID, *runID); err != nil {
			return err
		}
		if err := attachLegacyArtifactsToRunTx(tx, job.ID, *runID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func latestArchivedRunIDTx(tx *sql.Tx, jobID int64) (*int64, error) {
	var runID sql.NullInt64
	if err := tx.QueryRow(`SELECT id FROM job_runs WHERE job_id = ? ORDER BY archived_at DESC, id DESC LIMIT 1`, jobID).Scan(&runID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !runID.Valid {
		return nil, nil
	}
	return &runID.Int64, nil
}

func legacyJobArchivedAt(job *Job) int64 {
	if job == nil {
		return time.Now().Unix()
	}
	if job.EndTime != nil && *job.EndTime > 0 {
		return *job.EndTime
	}
	if job.StartTime > 0 {
		return job.StartTime
	}
	if job.QueuedAt > 0 {
		return job.QueuedAt
	}
	if job.CreatedAt > 0 {
		return job.CreatedAt
	}
	return time.Now().Unix()
}

func insertJobRunSnapshotTx(tx *sql.Tx, job *Job, reason string) (int64, error) {
	return insertJobRunSnapshotAtTx(tx, job, reason, time.Now().Unix())
}

func insertJobRunSnapshotAtTx(tx *sql.Tx, job *Job, reason string, archivedAt int64) (int64, error) {
	jobMetadata, err := encodeJobMetadata(job.Metadata)
	if err != nil {
		return 0, err
	}
	placementMeta, err := encodePlacementMeta(job.PlacementMeta)
	if err != nil {
		return 0, err
	}
	project, err := NormalizeProjectName(job.Project, job.WorkingDir, job.Command)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(
		`INSERT INTO job_runs (
			job_id, archived_at, archive_reason, status, host, working_dir, command, description,
			session_name, queue_name, backend, remote_id, remote_state,
			gpu, gpu_class, cpu_allotment, gpu_mem_gb, env_vars, tags, dep_spec,
			inputs, outputs, output_dirs, produces, needs, project,
			start_time, end_time, exit_code, error_message, failure_reason, error_diagnosis,
			job_metadata, placement_meta, cost, vastai_instance_id, retry_count, cloud_instance_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, archivedAt, reason, job.Status, job.Host, job.WorkingDir, job.Command, nullIfEmpty(job.Description),
		nullIfEmpty(job.SessionName), nullIfEmpty(job.QueueName), nullIfEmpty(job.Backend),
		nullIfEmpty(job.RemoteID), nullIfEmpty(job.RemoteState),
		nullIfEmpty(job.GPU), nullIfEmpty(job.GPUClass), job.CPUAllotment, job.GPUMemGB,
		encodeStringSlice(job.EnvVars), encodeTagsForRun(job.Tags), nullIfEmpty(job.DepSpec),
		encodeStringSlice(job.Inputs), encodeStringSlice(job.Outputs), encodeStringSlice(job.OutputDirs),
		encodeStringSlice(job.Produces), encodeStringSlice(job.Needs), nullIfEmpty(project),
		nullableUnix(job.StartTime), job.EndTime, job.ExitCode, nullIfEmpty(job.ErrorMessage),
		nullIfEmpty(job.FailureReason), nullIfEmpty(job.ErrorDiagnosis), jobMetadata, placementMeta,
		job.Cost, nullableInt(job.VastaiInstanceID), job.RetryCount, job.CloudInstanceID,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func attachLegacyTimeseriesToRunTx(tx *sql.Tx, jobID, runID int64) error {
	_, err := tx.Exec(`UPDATE job_timeseries SET job_run_id = ? WHERE job_id = ? AND job_run_id IS NULL`, runID, jobID)
	return err
}

func attachLegacyArtifactsToRunTx(tx *sql.Tx, jobID, runID int64) error {
	rows, err := tx.Query(`SELECT id, name, path FROM artifacts WHERE job_id = ? AND job_run_id IS NULL ORDER BY id ASC`, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type legacyArtifactRow struct {
		id   int64
		name sql.NullString
		path string
	}

	var legacyRows []legacyArtifactRow
	for rows.Next() {
		var row legacyArtifactRow
		if err := rows.Scan(&row.id, &row.name, &row.path); err != nil {
			return err
		}
		legacyRows = append(legacyRows, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, row := range legacyRows {
		var existingID int64
		var err error
		if row.name.Valid {
			err = tx.QueryRow(
				`SELECT id FROM artifacts WHERE job_run_id = ? AND path = ? AND name = ? LIMIT 1`,
				runID, row.path, row.name.String,
			).Scan(&existingID)
		} else {
			err = tx.QueryRow(
				`SELECT id FROM artifacts WHERE job_run_id = ? AND path = ? AND name IS NULL LIMIT 1`,
				runID, row.path,
			).Scan(&existingID)
		}
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(`UPDATE artifacts SET job_run_id = ? WHERE id = ?`, runID, row.id); err != nil {
			return err
		}
	}

	return nil
}

func setJobLatestRunIDTx(tx *sql.Tx, jobID, runID int64) error {
	_, err := tx.Exec(`UPDATE jobs SET latest_run_id = ? WHERE id = ?`, runID, jobID)
	return err
}

// PersistLatestRunSnapshot updates the current run row for a job from the live
// jobs table. If the job has no run yet, this creates one and points
// jobs.latest_run_id at it.
func PersistLatestRunSnapshot(db *sql.DB, jobID int64, reason string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	job, err := queryJobTx(tx, fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobSelectColumns), jobID)
	if err != nil {
		tx.Rollback()
		return err
	}
	if job == nil {
		return tx.Commit()
	}
	if err := persistLatestRunSnapshotTx(tx, job, reason); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// PersistLatestRunSnapshotIfExists updates the current run row only when the job
// already has a tracked run. This avoids creating placeholder runs for queued
// jobs whose mutable job row is being edited before execution starts.
func PersistLatestRunSnapshotIfExists(db *sql.DB, jobID int64, reason string) error {
	var latestRunID sql.NullInt64
	if err := db.QueryRow(`SELECT latest_run_id FROM jobs WHERE id = ?`, jobID).Scan(&latestRunID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if !latestRunID.Valid || latestRunID.Int64 == 0 {
		return nil
	}
	return PersistLatestRunSnapshot(db, jobID, reason)
}

func persistLatestRunSnapshotTx(tx *sql.Tx, job *Job, reason string) error {
	if job == nil {
		return nil
	}
	if job.LatestRunID == nil || *job.LatestRunID == 0 {
		runID, err := insertJobRunSnapshotTx(tx, job, reason)
		if err != nil {
			return err
		}
		job.LatestRunID = &runID
		return setJobLatestRunIDTx(tx, job.ID, runID)
	}

	now := time.Now().Unix()
	jobMetadata, err := encodeJobMetadata(job.Metadata)
	if err != nil {
		return err
	}
	placementMeta, err := encodePlacementMeta(job.PlacementMeta)
	if err != nil {
		return err
	}
	project, err := NormalizeProjectName(job.Project, job.WorkingDir, job.Command)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`UPDATE job_runs SET
			archived_at = ?,
			archive_reason = CASE WHEN ? <> '' THEN ? ELSE archive_reason END,
			status = ?,
			session_name = ?, remote_id = ?, remote_state = ?,
			project = ?,
			start_time = ?, end_time = ?, exit_code = ?, error_message = ?, failure_reason = ?,
			error_diagnosis = ?, job_metadata = ?, placement_meta = ?, cost = ?,
			vastai_instance_id = ?, retry_count = ?, cloud_instance_id = ?
		 WHERE id = ?`,
		now, reason, reason, job.Status,
		nullIfEmpty(job.SessionName), nullIfEmpty(job.RemoteID), nullIfEmpty(job.RemoteState),
		nullIfEmpty(project),
		nullableUnix(job.StartTime), job.EndTime, job.ExitCode, nullIfEmpty(job.ErrorMessage),
		nullIfEmpty(job.FailureReason), nullIfEmpty(job.ErrorDiagnosis), jobMetadata, placementMeta, job.Cost,
		nullableInt(job.VastaiInstanceID), job.RetryCount, job.CloudInstanceID,
		*job.LatestRunID,
	)
	return err
}

func startNewLatestRunTx(tx *sql.Tx, job *Job, reason string) error {
	if job == nil {
		return nil
	}
	runID, err := insertJobRunSnapshotTx(tx, job, reason)
	if err != nil {
		return err
	}
	job.LatestRunID = &runID
	return setJobLatestRunIDTx(tx, job.ID, runID)
}

func archiveJobRunTx(tx *sql.Tx, job *Job, reason string) error {
	if !shouldArchiveJobRun(job) {
		return nil
	}
	return persistLatestRunSnapshotTx(tx, job, reason)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableInt(v *int) *int {
	return v
}

func nullableUnix(ts int64) *int64 {
	if ts == 0 {
		return nil
	}
	return &ts
}

func encodeStringSlice(values []string) any {
	if len(values) == 0 {
		return nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return string(data)
}

func encodeTagsForRun(tags []string) any {
	value, err := encodeTags(tags)
	if err != nil {
		return nil
	}
	return value
}

func scanJobRuns(rows *sql.Rows) ([]JobRun, error) {
	var runs []JobRun
	for rows.Next() {
		var run JobRun
		var description, sessionName, queueName, backend, remoteID, remoteState sql.NullString
		var gpu, gpuClass, envVars, tags, depSpec, inputs, outputs, outputDirs, produces, needs, project sql.NullString
		var errorMessage, failureReason, errorDiagnosis, jobMetadata, placementMeta sql.NullString
		var startTime, endTime sql.NullInt64
		var exitCode, cpuAllotment, gpuMemGB, vastaiInstanceID, retryCount sql.NullInt64
		var cost sql.NullFloat64
		var cloudInstanceID sql.NullInt64

		if err := rows.Scan(
			&run.ID, &run.JobID, &run.ArchivedAt, &run.ArchiveReason, &run.Status, &run.Host,
			&run.WorkingDir, &run.Command, &description, &sessionName, &queueName, &backend, &remoteID,
			&remoteState, &gpu, &gpuClass, &cpuAllotment, &gpuMemGB, &envVars, &tags, &depSpec,
			&inputs, &outputs, &outputDirs, &produces, &needs, &project,
			&startTime, &endTime, &exitCode, &errorMessage, &failureReason, &errorDiagnosis,
			&jobMetadata, &placementMeta, &cost, &vastaiInstanceID, &retryCount,
			&cloudInstanceID,
		); err != nil {
			return nil, err
		}
		if description.Valid {
			run.Description = description.String
		}
		if sessionName.Valid {
			run.SessionName = sessionName.String
		}
		if queueName.Valid {
			run.QueueName = queueName.String
		}
		if backend.Valid {
			run.Backend = backend.String
		}
		if remoteID.Valid {
			run.RemoteID = remoteID.String
		}
		if remoteState.Valid {
			run.RemoteState = remoteState.String
		}
		if gpu.Valid {
			run.GPU = gpu.String
		}
		if gpuClass.Valid {
			run.GPUClass = gpuClass.String
		}
		if cpuAllotment.Valid {
			v := int(cpuAllotment.Int64)
			run.CPUAllotment = &v
		}
		if gpuMemGB.Valid {
			v := int(gpuMemGB.Int64)
			run.GPUMemGB = &v
		}
		run.EnvVars = decodeEnvVars(envVars)
		run.Tags = decodeTags(tags)
		if depSpec.Valid {
			run.DepSpec = depSpec.String
		}
		run.Inputs = decodeStringSlice(inputs)
		run.Outputs = decodeStringSlice(outputs)
		run.OutputDirs = decodeStringSlice(outputDirs)
		run.Produces = decodeStringSlice(produces)
		run.Needs = decodeStringSlice(needs)
		if project.Valid {
			run.Project = project.String
		}
		if startTime.Valid {
			run.StartTime = startTime.Int64
		}
		if endTime.Valid {
			run.EndTime = &endTime.Int64
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			run.ExitCode = &v
		}
		if errorMessage.Valid {
			run.ErrorMessage = errorMessage.String
		}
		if failureReason.Valid {
			run.FailureReason = failureReason.String
		}
		if errorDiagnosis.Valid {
			run.ErrorDiagnosis = errorDiagnosis.String
		}
		run.Metadata = decodeJobMetadata(jobMetadata)
		run.PlacementMeta = decodePlacementMeta(placementMeta)
		if cost.Valid {
			run.Cost = &cost.Float64
		}
		if vastaiInstanceID.Valid {
			v := int(vastaiInstanceID.Int64)
			run.VastaiInstanceID = &v
		}
		if retryCount.Valid {
			run.RetryCount = int(retryCount.Int64)
		}
		if cloudInstanceID.Valid {
			run.CloudInstanceID = &cloudInstanceID.Int64
		}
		if run.Backend == "" {
			run.Backend = BackendQueueRunner
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// GetJobRunByID returns a single run row by ID.
func GetJobRunByID(db *sql.DB, runID int64) (*JobRun, error) {
	rows, err := db.Query(`SELECT `+jobRunSelectColumns+` FROM job_runs WHERE id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs, err := scanJobRuns(rows)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, sql.ErrNoRows
	}
	return &runs[0], nil
}

// ListJobRuns returns archived execution snapshots for a logical job, ordered
// from oldest to newest archive.
func ListJobRuns(db *sql.DB, jobID int64) ([]JobRun, error) {
	rows, err := db.Query(
		`SELECT `+jobRunSelectColumns+` FROM job_runs WHERE job_id = ? ORDER BY archived_at ASC, id ASC`,
		jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobRuns(rows)
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

// DirectoryTailDisplay returns the trailing path component of the effective
// working directory for compact job-list displays.
func (j *Job) DirectoryTailDisplay() string {
	dir := strings.TrimSpace(j.EffectiveWorkingDir())
	if dir == "" {
		return "—"
	}
	tail := filepath.Base(dir)
	if tail == "" || tail == "." {
		return "—"
	}
	return tail
}

// HasAssignedHost reports whether the job currently has a concrete host target.
func (j *Job) HasAssignedHost() bool {
	return j.HasInventoryHost()
}

// EffectiveStatus returns the status to use for UI decisions.
// Returns PendingStatus if set (the desired/target state), otherwise Status.
// A job without a host cannot actually be running, starting, or paused; treat
// those impossible states as queued so the UI does not report them as active on
// a nonexistent host.
func (j *Job) EffectiveStatus() string {
	status := j.Status
	if j.PendingStatus != nil {
		status = *j.PendingStatus
	}
	if j.TargetKind() == JobTargetUnplaced {
		switch status {
		case StatusRunning, StatusStarting, StatusPaused:
			return StatusQueued
		}
	}
	return status
}

// HasTag returns true if the job has the given tag.
func (j *Job) HasTag(tag string) bool {
	tag = CanonicalizeTag(tag)
	if tag == "" {
		return false
	}
	for _, existing := range j.Tags {
		if CanonicalizeTag(existing) == tag {
			return true
		}
	}
	return false
}

// DisplayTags returns normalized user-facing tags for a job.
func (j *Job) DisplayTags() []string {
	if j == nil {
		return nil
	}
	return DisplayTags(j.Tags)
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

const cudaVisibleDevicesPrefix = "CUDA_VISIBLE_DEVICES="

// GetGPU returns the GPU (CUDA_VISIBLE_DEVICES) value for this job.
// First checks the database GPU field, then falls back to parsing the command.
func (j *Job) GetGPU() string {
	// Env vars (set via --env flag) take precedence as the most recent user intent
	for _, ev := range j.EnvVars {
		if val, ok := strings.CutPrefix(ev, cudaVisibleDevicesPrefix); ok {
			return val
		}
	}

	// Then check the database field
	if j.GPU != "" {
		return j.GPU
	}

	// Fall back to parsing from command for backwards compatibility
	return j.parseGPUFromCommand()
}

// GPUDevice returns the resolved GPU device indices for this job.
// Checks metadata (synced from runner) first, then falls back to the
// explicit GPU field / command parsing via GetGPU.
func (j *Job) GPUDevice() string {
	if j.Metadata != nil && j.Metadata.Resource != nil && j.Metadata.Resource.GPUDevices != "" {
		return j.Metadata.Resource.GPUDevices
	}
	return j.GetGPU()
}

// HostWithGPU returns "host:gpu" if GPU devices are known, otherwise just the host.
func (j *Job) HostWithGPU() string {
	target := j.TargetDisplay()
	if gpuDev := j.GPUDevice(); gpuDev != "" {
		return fmt.Sprintf("%s:%s", target, gpuDev)
	}
	return target
}

// ParseGPUFromCommandString extracts CUDA_VISIBLE_DEVICES from a command string.
// This standalone function works without a Job struct and is used at job insertion
// time to auto-populate the gpu column.
func ParseGPUFromCommandString(command string) string {
	// Check for env prefix: "env CUDA_VISIBLE_DEVICES=0 ..."
	_, envVars := ParseEnvPrefix(command)
	for _, ev := range envVars {
		if val, ok := strings.CutPrefix(ev, cudaVisibleDevicesPrefix); ok {
			return val
		}
	}

	// Check inline assignment: "CUDA_VISIBLE_DEVICES=0 python ..."
	for _, part := range strings.Fields(command) {
		if val, ok := strings.CutPrefix(part, cudaVisibleDevicesPrefix); ok {
			return val
		}
		if !strings.Contains(part, "=") {
			break
		}
	}

	return ""
}

// parseGPUFromCommand extracts CUDA_VISIBLE_DEVICES from the job's command.
// Returns empty string if not found.
func (j *Job) parseGPUFromCommand() string {
	// Get command after cd prefix if present
	cmd := j.Command
	if afterCd, _ := j.ParseCdCommand(); afterCd != "" {
		cmd = afterCd
	}

	// Try the standalone parser first (handles env prefix and inline assignment)
	if gpu := ParseGPUFromCommandString(cmd); gpu != "" {
		return gpu
	}

	// Then check exports (handles cd prefix first) — requires Job struct
	for _, ev := range j.ParseExportVars() {
		if val, ok := strings.CutPrefix(ev, cudaVisibleDevicesPrefix); ok {
			return val
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

// RecordHostSync stores the latest successful sync timestamp for a host.
func RecordHostSync(db *sql.DB, host string, syncedAt time.Time) error {
	if host == "" {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO host_syncs (name, last_synced)
		VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET last_synced = excluded.last_synced`,
		host, syncedAt.Unix(),
	)
	return err
}

// GetLastRestartCheck returns the timestamp when we last checked for restarted jobs on this host.
// Returns zero time if never checked.
func GetLastRestartCheck(db *sql.DB, host string) time.Time {
	var ts int64
	err := db.QueryRow(`SELECT COALESCE(last_restart_check, 0) FROM host_syncs WHERE name = ?`, host).Scan(&ts)
	if err != nil || ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// UpdateLastRestartCheck records when we last checked for restarted jobs on this host.
func UpdateLastRestartCheck(db *sql.DB, host string, checkedAt time.Time) error {
	if host == "" {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO host_syncs (name, last_synced, last_restart_check)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET last_restart_check = excluded.last_restart_check`,
		host, checkedAt.Unix(), checkedAt.Unix(),
	)
	return err
}

// LoadHostSyncTimes returns last sync timestamps for all hosts.
func LoadHostSyncTimes(db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.Query(`SELECT name, last_synced FROM host_syncs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	times := make(map[string]time.Time)
	for rows.Next() {
		var name string
		var lastSynced int64
		if err := rows.Scan(&name, &lastSynced); err != nil {
			return nil, err
		}
		if name != "" && lastSynced > 0 {
			times[name] = time.Unix(lastSynced, 0)
		}
	}
	return times, rows.Err()
}

// ListHostsSyncedSince returns hosts synced since the provided timestamp.
func ListHostsSyncedSince(db *sql.DB, since time.Time) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM host_syncs WHERE last_synced >= ? ORDER BY name`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name != "" {
			hosts = append(hosts, name)
		}
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
	OpMoveToFront     = "move_to_front"
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

func IsRelayRequestProcessed(db *sql.DB, requestID string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM processed_relay_requests WHERE request_id = ?`,
		requestID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func RecordProcessedRelayRequest(db *sql.DB, requestID, op string, jobID int64) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO processed_relay_requests (request_id, op, job_id, processed_at)
		 VALUES (?, ?, ?, ?)`,
		requestID, op, jobID, time.Now().Unix(),
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
