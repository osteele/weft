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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db/migrations"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/status"
	"github.com/osteele/weft/internal/util"
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
	GPU                  string // CUDA_VISIBLE_DEVICES value (e.g., "0", "0,1")
	GPUClass             string // GPU class name (e.g., "A100") — resolved to device at runtime
	CPUAllotment         *int   // Requested CPU allotment percent (nil = default)
	GPUMemGB             *int   // GPU memory reservation in GB per device (nil = use default)
	GPUMemMaxGB          *int   // Legacy GPU memory upper metadata; ignored by placement
	MaxComputeCap        string // CUDA compute-capability cap: "" = unresolved, placement.MaxComputeCapAny = unbounded, "X.Y" = numeric
	Metadata             *JobMetadata
	EnvVars              []string
	Tags                 []string
	DepSpec              string   // Dependency specification (e.g., "42" or "42+" for after-any)
	Inputs               []string // Data asset refs consumed by this job (e.g., "hf:meta-llama/Llama-3-8B")
	BestEffortInputs     []string // Inputs staged best-effort because they were auto-detected only
	ObservedInputs       []string // Data inputs discovered post-mortem (e.g., from disk-full HF cache scan)
	Outputs              []string // Data asset refs produced by this job
	OutputDirs           []string // Convention-based output directories from .weft.toml
	Produces             []string // Artifact specs this job produces (e.g., "output/model.pt" or "output/model.pt:100")
	Needs                []string // Artifact specs this job needs (e.g., "output/model.pt:100")
	Project              string   // Basename of working directory (stored at creation time)
	Priority             int      // Scheduling priority; 0 is normal, higher values run first
	CreatedAt            int64    // When the job was created/queued (0 for legacy jobs)
	QueuedAt             int64    // When job was added to remote queue (for queue ordering)
	StartTime            int64
	EndTime              *int64
	ExitCode             *int
	Status               string
	Tombstoned           bool
	Cost                 *float64              // Actual cost in dollars (for cloud-run jobs)
	ErrorDiagnosis       string                // JSON-encoded remediation diagnosis (see remediation.ErrorDiagnosis)
	RetryCount           int                   // Number of auto-remediation retries attempted
	PlacementMeta        *PlacementMeta        // Placement telemetry (predictions, scores)
	CLIResourceOverrides *CLIResourceOverrides // Current explicit CLI resource intent, replayed on retry
	PlacementReasons     []string              // Why the job is currently unplaced
	PlacementBlockedJSON string                // JSON blockreason.Structured: launch/reuse breakdown for an unplaced job
	// SubmitterSession identifies the agent session that ran `weft run`, as an
	// opaque string. Empty means the submission was not attributable to a
	// session, which is a normal value rather than an error.
	SubmitterSession     string
	LaunchID             *int64 // Cloud instance ID if this job is part of a cloud instance
	CampaignJobIndex     *int   // Position within a cloud campaign sequence, if assigned
	LatestRunID          *int64 // Latest execution attempt row for this logical job
	QueueBlockedReason   string // Transient UI-only queue gate reason; not persisted
	DisplayAttemptID     int64  `json:"-"` // Transient UI-only attempt row override; 0 means authoritative job row
	DisplayAttemptNumber int    `json:"-"` // Transient UI-only attempt number for expanded move rows
	DisplayMoveSource    string `json:"-"` // Transient UI-only move source label
	DisplayMoveTarget    string `json:"-"` // Transient UI-only move target label
	DisplayMovePhase     string `json:"-"` // Transient UI-only move phase for list/detail views
	DisplayMoveDim       bool   `json:"-"` // Transient UI-only marker for non-authoritative attempts

	// Three-way merge state for reconciliation
	LastSyncedStatus string  // Base: what remote was at last successful sync
	PendingStatus    *string // Local: what user wants (nil = no pending change)
	PendingAt        *int64  // When pending state was set
}

// JobTargetKind describes how a job is currently targeted.
type JobTargetKind string

const (
	JobTargetUnplaced       JobTargetKind = "unplaced"
	JobTargetInventoryHost  JobTargetKind = "inventory_host"
	JobTargetRentalInstance JobTargetKind = "rental_instance"
	JobTargetExternal       JobTargetKind = "external_executor"
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

// IsLaunchJob reports whether this job is associated with a cloud instance.
func (j *Job) IsLaunchJob() bool {
	return j.IsRentalJob()
}

// TargetKind reports whether the job is unplaced, assigned to an inventory
// host, or assigned to a rental instance. Legacy synthetic rental host strings
// are still recognized for compatibility with older rows.
func (j *Job) TargetKind() JobTargetKind {
	if j == nil {
		return JobTargetUnplaced
	}
	if j.Backend == BackendSkyPilot {
		return JobTargetExternal
	}
	if j.LaunchID != nil && *j.LaunchID > 0 {
		return JobTargetRentalInstance
	}
	host := strings.TrimSpace(j.Host)
	switch {
	case host == "":
		return JobTargetUnplaced
	case IsLaunchHost(host):
		return JobTargetRentalInstance
	default:
		return JobTargetInventoryHost
	}
}

// IsRentalJob reports whether the job is assigned to a rental instance.
func (j *Job) IsRentalJob() bool {
	return j != nil && j.TargetKind() == JobTargetRentalInstance
}

// IsUnplacedQueued reports whether the job is queued and not yet assigned to
// any host or rental instance. This is the canonical predicate for "work the
// auto-pilot still needs to act on".
func (j *Job) IsUnplacedQueued() bool {
	return j != nil && j.EffectiveStatus() == StatusQueued && j.TargetKind() == JobTargetUnplaced
}

// IsUnplacedAwaitingPlacement is like IsUnplacedQueued but also accepts
// pending_placement, the transient state used while a relaunch pass is
// processing the job. Use this for read paths (UI, blocked-reason hydration,
// auto-pilot planner input) that must not lose sight of jobs mid-placement.
func (j *Job) IsUnplacedAwaitingPlacement() bool {
	if j == nil || j.TargetKind() != JobTargetUnplaced {
		return false
	}
	es := j.EffectiveStatus()
	return es == StatusQueued || es == StatusPendingPlacement
}

// HasInventoryHost reports whether the job is currently assigned to an
// inventory host.
func (j *Job) HasInventoryHost() bool {
	return j != nil && j.TargetKind() == JobTargetInventoryHost
}

// HasFreshStatus reports whether this job's status can be trusted without
// a recent host sync. Inventory jobs require their host to appear in
// freshHosts; cloud and unplaced jobs are always fresh because their
// status is tracked locally.
func (j *Job) HasFreshStatus(freshHosts map[string]struct{}) bool {
	if j.TargetKind() != JobTargetInventoryHost {
		return true
	}
	_, ok := freshHosts[j.Host]
	return ok
}

// ProviderName returns the normalised cloud provider name ("vastai", "runpod")
// extracted from the job's "provider:<name>" tag, or "" if no provider tag is
// present or the value is unknown.
func (j *Job) ProviderName() string {
	if j == nil {
		return ""
	}
	for _, tag := range j.Tags {
		if name, ok := providerFromTag(tag); ok {
			return name
		}
	}
	return ""
}

// TargetDisplay returns a user-facing label for the current target.
func (j *Job) TargetDisplay() string {
	switch j.TargetKind() {
	case JobTargetExternal:
		return "SkyPilot"
	case JobTargetRentalInstance:
		if j != nil && j.LaunchID != nil && *j.LaunchID > 0 {
			return ids.FormatInstanceID(*j.LaunchID)
		}
		if j != nil {
			host := strings.TrimSpace(j.Host)
			if host != "" {
				if instanceID, ok := parseLegacyLaunchHostInstanceID(host); ok {
					return ids.FormatInstanceID(instanceID)
				}
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

// UsesPreemptiblePlacement reports whether a job explicitly allows running on
// interruptible/preemptible instances.
func (j *Job) UsesPreemptiblePlacement() bool {
	if j == nil {
		return false
	}
	return j.HasTag(TagPreemptible)
}

// JobIsTerminal reports whether jobID currently resolves to a terminal
// status. Lookup failures are treated as non-terminal so callers preserve
// existing best-effort notification behavior.
func JobIsTerminal(database *sql.DB, jobID int64) bool {
	job, err := GetJobByID(database, jobID)
	return err == nil && job != nil && IsTerminalStatus(job.Status)
}

// RequestedGPUCount returns the exact single-host GPU count requested for the
// job. A zero or missing override means one GPU for GPU-shaped jobs.
func (j *Job) RequestedGPUCount() int {
	if j == nil || j.CLIResourceOverrides == nil || j.CLIResourceOverrides.GPUCount == nil {
		return 1
	}
	if *j.CLIResourceOverrides.GPUCount < 1 {
		return 1
	}
	return *j.CLIResourceOverrides.GPUCount
}

// RequestedInterconnect returns the requested intra-host GPU interconnect.
func (j *Job) RequestedInterconnect() string {
	if j == nil || j.CLIResourceOverrides == nil {
		return ""
	}
	return strings.TrimSpace(j.CLIResourceOverrides.Interconnect)
}

// RequestedCPUCores returns the explicit minimum CPU core/vCPU floor.
func (j *Job) RequestedCPUCores() int {
	if j == nil || j.CLIResourceOverrides == nil || j.CLIResourceOverrides.CPUCores == nil {
		return 0
	}
	if *j.CLIResourceOverrides.CPUCores < 0 {
		return 0
	}
	return *j.CLIResourceOverrides.CPUCores
}

// CPUMemHeadroomGB is the safety margin added to a non-strict --cpu-mem
// declaration: host-RAM transients (e.g. multi-copy weight processing during
// model load) spike above steady-state, so the placement floor sits a little
// above the declared value unless the user requests exact matching.
const CPUMemHeadroomGB = 2

// EffectiveCPUMemGB resolves a raw --cpu-mem declaration to the placement floor:
// the raw value plus CPUMemHeadroomGB unless strict matching was requested.
// Shared by the submit path and RequestedCPUMemGB so headroom lives in one place.
func EffectiveCPUMemGB(rawGB int, strict bool) int {
	if rawGB <= 0 {
		return 0
	}
	if strict {
		return rawGB
	}
	return rawGB + CPUMemHeadroomGB
}

// RequestedCPUMemGB returns the effective host/system-RAM floor in GB. The raw
// --cpu-mem value is stored (pre-headroom) so this resolves idempotently on retry.
func (j *Job) RequestedCPUMemGB() int {
	if j == nil || j.CLIResourceOverrides == nil || j.CLIResourceOverrides.CPUMemGB == nil {
		return 0
	}
	strict := j.CLIResourceOverrides.CPUMemStrict != nil && *j.CLIResourceOverrides.CPUMemStrict
	return EffectiveCPUMemGB(*j.CLIResourceOverrides.CPUMemGB, strict)
}

// CLIResourceOverrides records explicit resource choices from submission or
// later edit/retry commands. Only fields the user explicitly set are
// populated. On retry, these are re-applied on top of the current script
// metadata to produce effective values — matching the merge `weft run`
// performs for a fresh submission.
//
// Submission-time gpu-mem is stored pre-headroom so the retry merge can match
// a fresh `weft run`. Later edit/retry commands store the effective
// reservation with GPUMemStrict set so the value is replayed exactly.
type CLIResourceOverrides struct {
	Host               string   `json:"host,omitempty"`
	GPU                string   `json:"gpu,omitempty"`
	GPUClass           string   `json:"gpu_class,omitempty"`
	GPUCount           *int     `json:"gpu_count,omitempty"`
	GPUMemGB           *int     `json:"gpu_mem_gb,omitempty"`
	GPUMemStrict       *bool    `json:"gpu_mem_strict,omitempty"`
	Interconnect       string   `json:"interconnect,omitempty"`
	CPUCores           *int     `json:"cpu_cores,omitempty"`
	CPUMemGB           *int     `json:"cpu_mem_gb,omitempty"`
	CPUMemStrict       *bool    `json:"cpu_mem_strict,omitempty"`
	DiskGB             *int     `json:"disk_gb,omitempty"`
	DiskMaxGB          *int     `json:"disk_max_gb,omitempty"`
	RuntimeDiskGB      *int     `json:"runtime_disk_gb,omitempty"`
	MaxHourlyRateCents *int     `json:"max_hourly_rate_cents,omitempty"`
	MaxSpendCents      *int     `json:"max_spend_cents,omitempty"`
	MaxTimeSeconds     *int     `json:"max_time_seconds,omitempty"`
	GracePeriodSeconds *int     `json:"grace_period_seconds,omitempty"`
	MinSurvival        *float64 `json:"min_survival,omitempty"`
	// MachineAffinity pins the job to specific provider physical machines,
	// stored as resolved machine refs. Carried on the job rather than on a
	// launch because the need is "run this next to that earlier run", which the
	// user expresses at submit time and which must survive until placement.
	MachineAffinity []string `json:"machine_affinity,omitempty"`
	MinCUDAVersion  string   `json:"min_cuda_version,omitempty"`
	RunpodCloudType string   `json:"runpod_cloud_type,omitempty"`
}

// RequestedRunpodCloudType returns the per-job RunPod cloud class override.
// Empty means the provider/config default should be used.
func (j *Job) RequestedRunpodCloudType() string {
	if j == nil || j.CLIResourceOverrides == nil {
		return ""
	}
	return strings.TrimSpace(j.CLIResourceOverrides.RunpodCloudType)
}

// RequestedMinSurvival returns the per-job cloud offer survival floor. A nil
// override keeps the caller's default; explicit values, including 0, replace it.
func (j *Job) RequestedMinSurvival(defaultFloor float64) float64 {
	defaultFloor = clampProbability(defaultFloor)
	if j == nil || j.CLIResourceOverrides == nil || j.CLIResourceOverrides.MinSurvival == nil {
		return defaultFloor
	}
	return clampProbability(*j.CLIResourceOverrides.MinSurvival)
}

// RequestedMinSurvivalForJobs returns the floor that satisfies every job in a
// grouped launch. Each job uses its explicit override or the supplied default,
// and the group must satisfy the most restrictive member.
func RequestedMinSurvivalForJobs(jobs []*Job, defaultFloor float64) float64 {
	if len(jobs) == 0 {
		return clampProbability(defaultFloor)
	}
	var floor float64
	for i, job := range jobs {
		effective := clampProbability(defaultFloor)
		if job != nil {
			effective = job.RequestedMinSurvival(defaultFloor)
		}
		if i == 0 || effective > floor {
			floor = effective
		}
	}
	return floor
}

// RequestedMaxHourlyRateCentsForJobs returns the strictest explicit hourly
// rate cap in a launch group. allExplicit is true only when every job carries
// a positive cap, which authorizes the group to bypass the global run-rate
// soft target after offer filtering has enforced that cap.
func RequestedMaxHourlyRateCentsForJobs(jobs []*Job) (cap int, allExplicit bool) {
	if len(jobs) == 0 {
		return 0, false
	}
	allExplicit = true
	for _, job := range jobs {
		if job == nil || job.CLIResourceOverrides == nil ||
			job.CLIResourceOverrides.MaxHourlyRateCents == nil ||
			*job.CLIResourceOverrides.MaxHourlyRateCents <= 0 {
			allExplicit = false
			continue
		}
		value := *job.CLIResourceOverrides.MaxHourlyRateCents
		if cap == 0 || value < cap {
			cap = value
		}
	}
	return cap, allExplicit
}

// RequestedMachineAffinityForJobs returns the union of machine pins across a
// group's jobs.
//
// Union, not intersection: each job names machines it will accept, so a group
// can run anywhere one of its members accepts. An intersection would make two
// jobs pinned to different machines unplaceable together, when the right
// outcome is that they are placed separately — a grouping decision, not an
// eligibility one.
func RequestedMachineAffinityForJobs(jobs []*Job) map[string]struct{} {
	var out map[string]struct{}
	for _, job := range jobs {
		if job == nil || job.CLIResourceOverrides == nil {
			continue
		}
		for _, ref := range job.CLIResourceOverrides.MachineAffinity {
			if ref == "" {
				continue
			}
			if out == nil {
				out = map[string]struct{}{}
			}
			out[ref] = struct{}{}
		}
	}
	return out
}

func clampProbability(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
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

const jobSelectColumns = `id, host, session_name, working_dir, command, description, generated_description, generation_hash, priority, created_at, queued_at, start_time, end_time, exit_code, status, error_message, backend, remote_id, remote_state, failure_reason, gpu, gpu_class, cpu_allotment, gpu_mem_gb, gpu_mem_max_gb, max_compute_cap, env_vars, tags, dep_spec, inputs, observed_inputs, outputs, output_dirs, produces, needs, project, tombstoned, last_synced_status, pending_status, pending_at, job_metadata, cost, error_diagnosis, retry_count, placement_meta, placement_reasons, cli_overrides, launch_id, campaign_job_index, latest_run_id, placement_blocked`

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
		LaunchStatusPlanned,
		LaunchStatusLaunching,
		LaunchStatusRunning,
		LaunchStatusPaused,
		LaunchStatusGrace,
		LaunchStatusCompleted,
		LaunchStatusFailed,
		LaunchStatusCancelled,
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

// terminationReasonValues lists the constants Go code may write to
// launches.termination_reason. Used by validateEnumAndRelationshipConstraints
// to surface stray DB values introduced outside Go (manual SQL, old
// binaries) — not enforced as a SQL CHECK constraint, which was retired
// in v24 because schema-rebuild-to-extend-enum is fragile (see
// dropLaunchesTerminationReasonCheck).
func terminationReasonValues() []string {
	return []string{
		TerminationReasonCompleted,
		TerminationReasonProviderFailure,
		TerminationReasonJobFailure,
		TerminationReasonDiskFull,
		TerminationReasonInfraFailure,
		TerminationReasonBootstrapTimeout,
		TerminationReasonPhaseStall,
		TerminationReasonPreempted,
		TerminationReasonCancelled,
		TerminationReasonUnknown,
		TerminationReasonWeftBug,
	}
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

// dropLegacyRunsTable clears stale references to job_runs IDs, then drops the table.
func dropLegacyRunsTable(db *sql.DB) error {
	// latest_run_id may already be removed from jobs; ignore errors
	db.Exec(`UPDATE jobs SET latest_run_id = NULL
		 WHERE latest_run_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = jobs.latest_run_id)`)
	for _, stmt := range []string{
		`UPDATE job_timeseries SET attempt_id = NULL
		 WHERE attempt_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = job_timeseries.attempt_id AND job_id = job_timeseries.job_id)`,
		`UPDATE artifacts SET attempt_id = NULL
		 WHERE attempt_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = artifacts.attempt_id AND job_id = artifacts.job_id)`,
		`DROP TABLE IF EXISTS job_runs`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func dropIntegrityViewsAndTriggers(db *sql.DB) error {
	for _, name := range []string{"launch_job_membership", "job_effective_state", "all_runs"} {
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
		"launch_live_state_orphan_open_queued_attempts_on_insert",
		"launch_live_state_orphan_open_queued_attempts_on_update",
		"launches_clear_pending_placement_on_terminal",
		"move_intents_open_new_requires_live_source_insert",
		"move_intents_open_new_requires_live_source_update",
		"job_attempts_prevent_mixed_host_launch_insert",
		"job_attempts_prevent_mixed_host_launch_update",
		"job_attempts_sync_target_shadow_insert",
		"job_attempts_sync_target_shadow_update",
		"job_attempts_reject_target_shadow_mismatch_insert",
		"job_attempts_reject_target_shadow_mismatch_update",
		"job_open_attempts_validate_insert",
		"job_open_attempts_validate_update",
		"job_attempts_open_state_insert",
		"job_attempts_open_state_close_or_move",
		"job_attempts_open_state_open_or_move",
		"job_attempts_open_state_delete",
	} {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return err
		}
	}
	return nil
}

func repairLegacyCloudPlacementHosts(_ *sql.DB) error {
	// No-op: cloud_instance_id column has been removed from the jobs table.
	// Legacy placement repair is handled by the job_attempts migration.
	return nil
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
	campaignStatusSQL := sqlStringList(campaignStatusValues())
	cloudStatusSQL := sqlStringList(cloudInstanceStatusValues())
	terminationReasonSQL := sqlStringList(terminationReasonValues())

	validations := []struct {
		query   string
		format  func(*sql.Rows) (string, error)
		message string
	}{
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
			query: fmt.Sprintf(`SELECT id, status FROM launches WHERE status NOT IN (%s) ORDER BY id ASC LIMIT 5`, cloudStatusSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var status string
				if err := rows.Scan(&id, &status); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, status), nil
			},
			message: "invalid launches.status values",
		},
		{
			query: fmt.Sprintf(`SELECT id, termination_reason FROM launches WHERE termination_reason IS NOT NULL AND termination_reason != '' AND termination_reason NOT IN (%s) ORDER BY id ASC LIMIT 5`, terminationReasonSQL),
			format: func(rows *sql.Rows) (string, error) {
				var id int64
				var reason string
				if err := rows.Scan(&id, &reason); err != nil {
					return "", err
				}
				return fmt.Sprintf("%d=%q", id, reason), nil
			},
			message: "invalid launches.termination_reason values",
		},
		{
			query: `SELECT a.id, a.job_id, a.attempt_id
				FROM artifacts a
				WHERE a.attempt_id IS NOT NULL
				  AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = a.attempt_id AND job_id = a.job_id)
				ORDER BY a.id ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var id, jobID, runID int64
				if err := rows.Scan(&id, &jobID, &runID); err != nil {
					return "", err
				}
				return fmt.Sprintf("artifact %d job=%d attempt=%d", id, jobID, runID), nil
			},
			message: "invalid artifacts attempt ownership",
		},
		{
			query: `SELECT jt.job_id, jt.ts, jt.attempt_id
				FROM job_timeseries jt
				WHERE jt.attempt_id IS NOT NULL
				  AND NOT EXISTS (SELECT 1 FROM job_attempts WHERE id = jt.attempt_id AND job_id = jt.job_id)
				ORDER BY jt.job_id ASC, jt.ts ASC LIMIT 5`,
			format: func(rows *sql.Rows) (string, error) {
				var jobID, ts, runID int64
				if err := rows.Scan(&jobID, &ts, &runID); err != nil {
					return "", err
				}
				return fmt.Sprintf("timeseries (%d,%d) attempt=%d", jobID, ts, runID), nil
			},
			message: "invalid job_timeseries attempt ownership",
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
		FROM job_attempts
		WHERE end_time IS NULL AND launch_id IS NOT NULL
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
	// Clear placement_reasons for jobs with live cloud attempts, since
	// execution state (launch_id, host) is now on job_attempts.
	if _, err := db.Exec(`
		UPDATE jobs
		SET placement_reasons = NULL
		WHERE EXISTS (
			SELECT 1
			FROM job_attempts ja
			JOIN launches ci ON ci.id = ja.launch_id
			WHERE ja.job_id = jobs.id
			  AND ja.end_time IS NULL
			  AND ci.status IN ('running', 'launching', 'grace')
		)
		AND placement_reasons IS NOT NULL`); err != nil {
		return err
	}
	return nil
}

// Special job tags that affect scheduling and execution behavior.
const (
	ProcessedTag      = "processed"
	TagExclusive      = "exclusive"
	TagBenchmark      = "benchmark-isolation"
	TagRental         = "rental"
	TagInventory      = "inventory"
	TagInterruptible  = "interruptible"
	TagCPUIntensive   = "cpu-intensive"
	TagProviderPrefix = "provider:"
	TagProviderVastai = "provider:vastai"
	TagProviderRunpod = "provider:runpod"

	// Legacy tag aliases accepted on input and in existing database rows.
	TagCloudLegacy            = "cloud"
	TagOnPremLegacy           = "on-prem"
	TagPreemptibleLegacy      = "preemptible"
	TagComputeIntensiveLegacy = "compute-intensive"

	// Deprecated aliases kept for internal compatibility while the codebase
	// moves to the preferred rental/inventory terminology.
	TagCloud            = TagRental
	TagOnPrem           = TagInventory
	TagPreemptible      = TagInterruptible
	TagComputeIntensive = TagCPUIntensive
)

const BackendQueueRunner = "queue-runner"
const BackendSlurm = "slurm"
const BackendVastai = "vastai"
const BackendSkyPilot = "skypilot"

// Status constants re-exported from internal/status (canonical source of truth).
const (
	StatusStarting         = status.Starting
	StatusRunning          = status.Running
	StatusCompleted        = status.Completed
	StatusDead             = status.Dead
	StatusQueued           = status.Queued
	StatusFailed           = status.Failed
	StatusKilled           = status.Killed
	StatusCanceled         = status.Canceled
	StatusPaused           = status.Paused
	StatusDraft            = status.Draft
	StatusPendingPlacement = status.PendingPlacement
)

// statusNeedsRental is the legacy DB value for unplaced jobs. Migrated to StatusQueued with host="".
const statusNeedsRental = "needs_rental"

// currentSchemaVersion is defined in migrations.go (derived from
// versionedMigrations).

var dbPath string
var bugDBPath string

// sharedDBFile is the jobs database every installed weft on this machine uses,
// recorded even when this process is redirected elsewhere. checkDevBuildMayMigrate
// compares against it, so it must be derived from the same place dbPath is.
var sharedDBFile string

var startupRepairFn = startupRepair

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	configDir := filepath.Join(home, ".config", "weft")
	sharedDBFile = filepath.Join(configDir, "jobs.db")

	if runningUnderGoTest() {
		// A test binary must never resolve to the shared database. Opening it
		// applies pending migrations, so a test in a tree carrying an
		// unreleased migration advances the schema past what the installed
		// binary understands, and every weft command on the machine fails
		// until it is rebuilt (wb75). The directory is per-process because
		// packages test in parallel; clearing it first keeps a recycled pid
		// from inheriting a database left at another tree's schema version.
		configDir = filepath.Join(os.TempDir(), fmt.Sprintf("weft-test-db-%d", os.Getpid()))
		_ = os.RemoveAll(configDir)
	}

	dbPath = filepath.Join(configDir, "jobs.db")
	bugDBPath = filepath.Join(configDir, "bugs.db")
}

// runningUnderGoTest reports whether this process is a Go test binary.
//
// This cannot use testing.Testing(): that is set while the test binary starts
// up, which is after package initialization, so it reads false from an init
// function. The executable name and the -test.* flags are both available
// immediately.
func runningUnderGoTest() bool {
	if len(os.Args) == 0 {
		return false
	}
	return looksLikeTestBinary(os.Args[0], os.Args[1:])
}

// looksLikeTestBinary reports whether an executable name and its arguments are
// those of a Go test binary. The name covers `go test`, which always builds a
// `.test` executable; the arguments cover a binary from `go test -c -o name`,
// which carries the harness flags under any name.
func looksLikeTestBinary(exe string, args []string) bool {
	if strings.HasSuffix(exe, ".test") || strings.HasSuffix(exe, ".test.exe") {
		return true
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-test.") || strings.HasPrefix(arg, "--test.") {
			return true
		}
	}
	return false
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
	// - foreign_keys(ON): enforce FK constraints
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(ON)&_txlock=immediate", dbPath)
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	configureSQLitePool(db)

	migrated, err := initSchema(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	if migrated {
		if err := startupRepairFn(db); err != nil {
			if IsDatabaseLocked(err) {
				slog.Warn("startup repair deferred due database lock; continuing with existing schema/state", "error", err)
			} else if IsDatabaseReadOnly(err) {
				slog.Debug("startup repair deferred due read-only database; continuing with existing schema/state", "error", err)
			} else {
				db.Close()
				return nil, fmt.Errorf("startup repair: %w", err)
			}
		}
	}

	return db, nil
}

// OpenForReading opens the database for display commands. On a current schema
// this does not run startup repair, so routine status/list commands do not take
// the repair writer path. The returned handle is still writable because many
// display commands opportunistically run a fast sync before rendering.
//
// If the writable open path is unavailable because another process holds the
// writer lock, OpenForReading falls back to a read-only handle so commands can
// still display the last known state.
//
// After either path, the applied migration version is compared against this
// binary's current schema version. A stale schema returns ErrSchemaMismatch
// instead of letting later queries fail with missing-column errors.
func OpenForReading() (*sql.DB, error) {
	database, err := Open()
	if err != nil {
		if IsDatabaseLocked(err) || IsDatabaseReadOnly(err) {
			slog.Debug("database not writable, opening read-only", "error", err)
		} else {
			return nil, err
		}
		database, err = OpenReadOnly()
		if err != nil {
			return nil, err
		}
	}
	if verifyErr := verifySchemaVersion(database); verifyErr != nil {
		database.Close()
		return nil, verifyErr
	}
	return database, nil
}

// OpenReadOnly opens the database in read-only mode without running schema
// migrations. Use this as a fallback when Open() fails with SQLITE_BUSY or
// SQLITE_READONLY, so read-only commands can still display data.
func OpenReadOnly() (*sql.DB, error) {
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&mode=ro", dbPath)
	database, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	configureSQLitePool(database)
	// sql.Open is lazy; verify the connection works.
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	return database, nil
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

func configureSQLitePool(database *sql.DB) {
	// SQLite permits only one writer. Keep each process from opening dozens
	// of connections to the same DB, while still allowing existing code that
	// performs nested reads during a transaction to make progress.
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
}

// initSchema brings the database up to the current schema version. The whole
// schema is the squashed v1 baseline; later changes are versioned goose
// migrations. See internal/db/migrations.
func initSchema(db *sql.DB) (bool, error) {
	ctx := context.Background()

	pending, err := migrations.HasPending(ctx, db)
	if err != nil {
		return false, fmt.Errorf("check pending migrations: %w", err)
	}
	if !pending {
		return false, nil
	}

	if err := checkDevBuildMayMigrate(dbPath); err != nil {
		return false, err
	}

	// Snapshot a database that already holds data before migrating, so a bad
	// migration can be rolled back. A freshly created file has nothing to lose.
	hasSchema := databaseHasSchema(db)
	fromVersion := int(migrations.Version(ctx, db))
	toVersion := int(migrations.Target())
	if hasSchema {
		reportMigrationProgress(MigrationProgressEvent{Phase: "migration_start", FromVersion: fromVersion, ToVersion: toVersion})
		backupBeforeMigration(db, fromVersion)
	}

	if err := migrations.Up(ctx, db); err != nil {
		return false, err
	}
	if hasSchema {
		reportMigrationProgress(MigrationProgressEvent{Phase: "migration_done", FromVersion: fromVersion, ToVersion: toVersion})
	}
	return true, nil
}

// databaseHasSchema reports whether the database already contains weft's
// tables — used to decide whether a pre-migration backup is warranted.
func databaseHasSchema(db *sql.DB) bool {
	var n int
	_ = db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='jobs'`,
	).Scan(&n)
	return n > 0
}

// dedupeArtifactsForUniqueIndexes removes duplicate artifact rows that would
// violate the partial unique indexes on legacy and attempt-scoped artifact keys.
// Keep the newest row (highest id) for each duplicate key.
func dedupeArtifactsForUniqueIndexes(db *sql.DB) error {
	queries := []string{
		`DELETE FROM artifacts
		 WHERE attempt_id IS NULL
		   AND id NOT IN (
		     SELECT MAX(id)
		     FROM artifacts
		     WHERE attempt_id IS NULL
		     GROUP BY job_id, name, path
		   )`,
		`DELETE FROM artifacts
		 WHERE attempt_id IS NOT NULL
		   AND id NOT IN (
		     SELECT MAX(id)
		     FROM artifacts
		     WHERE attempt_id IS NOT NULL
		     GROUP BY attempt_id, name, path
		   )`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// startupRepair runs after schema migration. It performs data cleanup, repairs,
// and view validation that should happen when this binary has just brought an
// older database up to the current schema.
func startupRepair(db *sql.DB) error {
	// Clean up stale attempt data: close duplicate open attempts and
	// ensure orphaned cloud jobs have fresh unplaced attempts.
	if err := cleanupStaleAttempts(db); err != nil {
		return err
	}

	// Repair completed attempts that lost their launch association.
	if err := repairOrphanedCompletedAttempts(db); err != nil {
		return err
	}

	// Repair attempts where end_time was written as 0 instead of NULL.
	if err := repairAttemptEndTimeZero(db); err != nil {
		return err
	}

	// Repair synthetic completed cloud attempts that were closed without an exit code.
	if err := repairCompletedCloudAttemptsMissingExitCode(db); err != nil {
		return err
	}
	if err := repairCompletedCloudAttemptsWithoutEvidence(db); err != nil {
		return err
	}

	// Repair placeholder project values (e.g., ".") from older job submissions.
	if err := repairPlaceholderProjects(db); err != nil {
		return err
	}

	// Normalize stale pending_placement intents that no longer have launch
	// anchors; fresh move operations are preserved by an age threshold.
	if _, err := NormalizeStalePendingPlacementNoLaunch(db); err != nil {
		return err
	}
	if _, err := PruneMoveIntents(db, 5*time.Minute); err != nil {
		return err
	}

	// Verify every view compiles against the current table schema. A view
	// that references a missing column — typically a schema change that is
	// missing its versioned migration — fails here with a clear message
	// instead of surfacing later as a cryptic query error.
	if err := validateViews(db); err != nil {
		return err
	}

	return nil
}

// validateViews verifies that every view compiles against the current table
// schema. SQLite creates views lazily: CREATE VIEW accepts a body that
// references a missing column, and the error only surfaces when the view is
// queried. Probing each view with a zero-row SELECT forces SQLite to compile
// the full view body, so a column/view desync — typically a schema change
// that is missing its versioned migration — fails Open() with a clear message
// instead of deep in a later query.
func validateViews(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'view' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("list views: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scan view name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate views: %w", err)
	}
	rows.Close()

	for _, name := range names {
		// View names come from sqlite_master and are controlled by this
		// package, so direct interpolation is safe.
		probe, err := db.Query(`SELECT * FROM "` + name + `" LIMIT 0`)
		if err != nil {
			return fmt.Errorf("view %q is not queryable after schema init — "+
				"likely a missing column or a pending migration: %w", name, err)
		}
		probe.Close()
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

// RecordStart records a new job start and returns its ID
// Deprecated: Use RecordJobStarting + UpdateJobRunning for new jobs
func RecordStart(db *sql.DB, host, sessionName, workingDir, command string, startTime int64, description string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(
		`INSERT INTO jobs (working_dir, command, description, created_at, placement_host)
		 VALUES (?, ?, ?, ?, ?)`,
		workingDir, command, description, startTime, host,
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
	// Update the auto-created attempt with execution state
	if _, err := tx.Exec(
		`UPDATE job_attempts SET host = ?, session_name = ?, start_time = ?, status = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		host, sessionName, startTime, StatusRunning, jobID,
	); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return jobID, nil
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
		`INSERT INTO jobs (working_dir, command, description, created_at, placement_host)
		 VALUES (?, ?, ?, ?, ?)`,
		workingDir, command, description, now, host,
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
	// Create the attempt at placement time with execution state.
	if _, err := tx.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, status, queued_at, start_time)
		 VALUES (?, 1, ?, ?, ?, ?)`,
		jobID, host, StatusStarting, now, now,
	); err != nil {
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
	warnOpenTransition(db, id, StatusRunning, false, status.SourceSSHSync)
	_, err := db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL AND status = ?`,
		StatusRunning, id, StatusStarting)
	return err
}

// UpdateJobFailed marks a starting job as failed to start. Stamps
// cloud_outcome in the same UPDATE for cloud rows (launch_id NOT NULL),
// matching ClosedCloudAttemptHasOutcome (campaign-lifecycle.allium).
func UpdateJobFailed(db *sql.DB, id int64, errorMsg string) error {
	warnOpenTransition(db, id, StatusDead, false, status.SourceSSHSync)
	endTime := time.Now().Unix()
	_, err := db.Exec(`
		UPDATE job_attempts
		SET status = ?, end_time = ?, error_message = ?,
		    cloud_outcome = CASE
		        WHEN launch_id IS NULL THEN cloud_outcome
		        WHEN cloud_outcome IS NULL THEN ?
		        ELSE cloud_outcome
		    END
		WHERE job_id = ? AND end_time IS NULL AND status = ?`,
		StatusDead, endTime, errorMsg, AttemptOutcomeFailed, id, StatusStarting)
	return err
}

// UpdateJobStartingToQueued transitions a starting job to queued state and assigns a queue name.
func UpdateJobStartingToQueued(db *sql.DB, id int64) error {
	return MarkAttemptQueuedByID(db, id)
}

// UpdateJobRunningToQueued transitions a running job back to queued state.
func UpdateJobRunningToQueued(db *sql.DB, id int64) error {
	return MarkAttemptQueuedByID(db, id)
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		AND (description IS NULL OR description = '')
		AND (generated_description IS NULL OR generated_description = '')
		ORDER BY id DESC LIMIT ?`, jobSelectColumns)
	return queryJobs(db, query, limit)
}

// UpdateJobWorkingDir updates the working directory for a queued or draft job.
func UpdateJobWorkingDir(db *sql.DB, id int64, workingDir string) error {
	var err error
	workingDir, err = workdir.Normalize(workingDir)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`UPDATE jobs SET working_dir = ? WHERE id = ?
		 AND EXISTS (SELECT 1 FROM job_status js WHERE js.id = ? AND js.status IN (?, ?))`,
		workingDir, id, id, StatusQueued, StatusDraft,
	)
	return err
}

// UpdateJobCommand updates the command for a queued or draft job.
func UpdateJobCommand(db *sql.DB, id int64, command string) error {
	_, err := db.Exec(
		`UPDATE jobs SET command = ? WHERE id = ?
		 AND EXISTS (SELECT 1 FROM job_status js WHERE js.id = ? AND js.status IN (?, ?))`,
		command, id, id, StatusQueued, StatusDraft,
	)
	return err
}

// SetJobCommand updates the command for a job regardless of status.
func SetJobCommand(db dbExecer, id int64, command string) error {
	_, err := db.Exec(`UPDATE jobs SET command = ? WHERE id = ?`, command, id)
	return err
}

// UpdateJobHost updates the host for a job (only for queued jobs)
func UpdateJobHost(db *sql.DB, id int64, newHost string) error {
	return AssignQueuedJobToInventoryHost(db, id, newHost)
}

// RecordCompletionByID updates a job by ID with its exit code and end time.
// Clears session_name per spec: SessionImpliesRunning (session => status = running).
// Also updates last_synced_status since recording completion is a sync operation.
// Note: Also accepts failed/dead status because a status file appearing is authoritative
// evidence of completion, even if the job was previously marked as failed due to race conditions.
func RecordCompletionByID(db *sql.DB, id int64, exitCode int, endTime int64) error {
	_, err := RecordCompletionByIDWithTransition(db, id, exitCode, endTime)
	return err
}

// RecordCompletionByIDWithTransition is like RecordCompletionByID, and also
// reports whether this call moved a non-terminal attempt into terminal state.
// Notification senders must use this instead of a separate preflight status
// read, because the preflight read races across concurrent sync processes.
func RecordCompletionByIDWithTransition(db *sql.DB, id int64, exitCode int, endTime int64) (bool, error) {
	if err := checkTransition(db, id, StatusCompleted, true, status.SourceSSHSync); err != nil {
		return false, err
	}
	transitioned, err := UpdateAttemptCompletionIfNonTerminal(db, id, exitCode, endTime)
	if err != nil {
		return false, err
	}
	if transitioned {
		return true, nil
	}
	if err := checkTransition(db, id, StatusCompleted, true, status.SourceSSHSync); err != nil {
		return false, err
	}
	return false, UpdateAttemptCompletion(db, id, exitCode, endTime)
}

// MarkDeadByID marks a running or queued job as failed (unexpected termination) by ID.
// Clears session_name per spec: SessionImpliesRunning (session => status = running).
// Also updates last_synced_status since this is detecting remote state.
func MarkDeadByID(db *sql.DB, id int64) error {
	if err := checkOpenTransition(db, id, StatusFailed, false, status.SourceSSHSync); err != nil {
		return err
	}
	return UpdateAttemptDead(db, id)
}

// SetJobVastaiInstance stores the Vast.ai instance ID and backend on a job record.
// This should be called immediately after creating the instance, before any other work.
func SetJobVastaiInstance(db *sql.DB, jobID int64, instanceID int) error {
	return SetAttemptVastaiInstance(db, jobID, instanceID)
}

// SetJobCost updates the actual cost for a cloud-run job.
func SetJobCost(db *sql.DB, jobID int64, cost float64) error {
	return SetAttemptCost(db, jobID, cost)
}

// SetJobPlacementMeta stores placement telemetry on a job record.
func SetJobPlacementMeta(db *sql.DB, jobID int64, meta *PlacementMeta) error {
	return SetAttemptPlacementMeta(db, jobID, meta)
}

// SetJobPlacementReasons stores why a job is currently unplaced.
func SetJobPlacementReasons(db dbExecer, jobID int64, reasons []string) error {
	encoded := encodeStringSlice(reasons)
	_, err := db.Exec(`UPDATE jobs SET placement_reasons = ? WHERE id = ?`, encoded, jobID)
	return err
}

// SetJobPlacementBlocked stores the structured launch/reuse breakdown for an
// unplaced job (JSON-encoded blockreason.Structured). An empty string clears
// the column.
func SetJobPlacementBlocked(db dbExecer, jobID int64, encoded string) error {
	if strings.TrimSpace(encoded) == "" {
		_, err := db.Exec(`UPDATE jobs SET placement_blocked = NULL WHERE id = ?`, jobID)
		return err
	}
	_, err := db.Exec(`UPDATE jobs SET placement_blocked = ? WHERE id = ?`, encoded, jobID)
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

func decodeCLIResourceOverrides(value sql.NullString) *CLIResourceOverrides {
	if !value.Valid || value.String == "" {
		return nil
	}
	var snap CLIResourceOverrides
	if err := json.Unmarshal([]byte(value.String), &snap); err != nil {
		return nil
	}
	return &snap
}

// SetJobCLIResourceOverrides updates the stored CLI-submission overrides.
// nil or a struct with no fields set clears the column.
func SetJobCLIResourceOverrides(db dbExecer, jobID int64, snap *CLIResourceOverrides) error {
	var value interface{}
	if snap != nil && !snap.IsEmpty() {
		data, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("encode cli overrides: %w", err)
		}
		value = string(data)
	}
	_, err := db.Exec(`UPDATE jobs SET cli_overrides = ? WHERE id = ?`, value, jobID)
	return err
}

// IsEmpty reports whether no fields are populated.
func (o *CLIResourceOverrides) IsEmpty() bool {
	return o.Host == "" && o.GPU == "" && o.GPUClass == "" && o.GPUCount == nil &&
		o.GPUMemGB == nil && o.GPUMemStrict == nil && o.Interconnect == "" && o.CPUCores == nil &&
		o.CPUMemGB == nil && o.CPUMemStrict == nil &&
		o.DiskGB == nil && o.DiskMaxGB == nil && o.RuntimeDiskGB == nil &&
		o.MaxHourlyRateCents == nil && o.MaxSpendCents == nil && o.MaxTimeSeconds == nil &&
		o.GracePeriodSeconds == nil && o.MinSurvival == nil &&
		len(o.MachineAffinity) == 0 &&
		o.MinCUDAVersion == "" && o.RunpodCloudType == ""
}

// ListActiveVastaiJobs returns jobs with backend=vastai that have an instance ID
// and are in a non-terminal status.
func ListActiveVastaiJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE backend = ? AND launch_id IS NOT NULL AND status NOT IN (?, ?, ?, ?) AND tombstoned = 0 ORDER BY id`, jobSelectColumns)
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE effective_target_kind = ?
		  AND status NOT IN (?, ?, ?, ?)
		  AND tombstoned = 0
		ORDER BY id`, jobSelectColumns)
	rows, err := db.Query(query, string(JobTargetRentalInstance), StatusCompleted, StatusFailed, StatusKilled, StatusCanceled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// MarkJobDraftPending updates a job to draft status locally and records pending cleanup.
func MarkJobDraftPending(db *sql.DB, id int64) error {
	warnOpenTransition(db, id, StatusDraft, false, status.SourceUserAction)
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE job_attempts SET status = ?, pending_status = ?, pending_at = ?
		 WHERE id = `+latestOpenAttemptSubquery,
		StatusDraft, StatusDraft, now, id,
	)
	return err
}

// MarkRunningByID transitions a job from starting to running
func MarkRunningByID(db *sql.DB, id int64) error {
	warnOpenTransition(db, id, StatusRunning, false, status.SourceSSHSync)
	_, err := db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL AND status = ?`,
		StatusRunning, id, StatusStarting)
	return err
}

// MarkPausedByID transitions a job to paused from queued/starting/running.
func MarkPausedByID(db *sql.DB, id int64) error {
	warnOpenTransition(db, id, StatusPaused, false, status.SourceSSHSync)
	_, err := db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL AND status IN (?, ?, ?)`,
		StatusPaused, id, StatusQueued, StatusStarting, StatusRunning)
	return err
}

// MarkPausedFromTerminal transitions a job from a terminal status back to paused.
func MarkPausedFromTerminal(db *sql.DB, id int64) error {
	warnTransition(db, id, StatusPaused, false, status.SourceSSHSync)
	_, err := db.Exec(`UPDATE job_attempts SET status = ?, end_time = NULL, exit_code = NULL, error_message = NULL WHERE job_id = ? AND status IN (?, ?, ?, ?)`,
		StatusPaused, id, StatusFailed, StatusDead, StatusKilled, StatusCanceled)
	return err
}

// MarkRunningFromPaused transitions a job from paused back to running.
func MarkRunningFromPaused(db *sql.DB, id int64) error {
	warnOpenTransition(db, id, StatusRunning, false, status.SourceSSHSync)
	_, err := db.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL AND status = ?`,
		StatusRunning, id, StatusPaused)
	return err
}

// MarkRunningFromTerminal transitions a job from a terminal status (failed, dead, etc.) back to running.
// This handles jobs that were restarted by the queue runner after previously failing.
func MarkRunningFromTerminal(db *sql.DB, id int64) error {
	warnTransition(db, id, StatusRunning, false, status.SourceQueueRunner)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// Close current attempt, create new running attempt
	now := time.Now().Unix()
	if err := closeOpenAttempts(tx, id, now); err != nil {
		tx.Rollback()
		return err
	}
	attemptID, err := createAttemptTx(tx, id, "", nil, StatusRunning)
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE job_attempts SET last_synced_status = ? WHERE id = ?`, StatusRunning, attemptID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MarkQueuedJobRunning transitions a queued job to running without touching start_time.
// Called from sync when detecting a job has started running remotely.
// Updates last_synced_status since this is a sync operation.
//
// Because the remote agent has been observed running the attempt, any
// non-terminal pending_status intent (queued, pending_placement, running,
// starting) is now satisfied and is cleared atomically with the status flip.
// Stop intents (canceled, killed, etc.) are preserved so they can still
// propagate. Set by RequeueFreshAttemptByID; if not cleared,
// EffectiveStatus() keeps reporting "queued" while the job is actually
// running.
func MarkQueuedJobRunning(database *sql.DB, id int64) error {
	warnOpenTransition(database, id, StatusRunning, false, status.SourceR2Phase)
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := UpdateAttemptRunning(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE job_attempts
		SET pending_status = NULL, pending_at = NULL
		WHERE id = `+latestOpenAttemptSubquery+`
		  AND pending_status IN (?, ?, ?, ?)`,
		id, StatusQueued, StatusPendingPlacement, StatusRunning, StatusStarting,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkQueuedJobStarting transitions a queued job to starting without
// overwriting an existing start_time. Called from sync when R2 reports a
// job-attributed setup phase before the user command has begun.
func MarkQueuedJobStarting(database *sql.DB, id int64) error {
	warnOpenTransition(database, id, StatusStarting, false, status.SourceR2Phase)
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := UpdateAttemptStarting(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE job_attempts
		SET pending_status = NULL, pending_at = NULL
		WHERE id = `+latestOpenAttemptSubquery+`
		  AND pending_status IN (?, ?, ?, ?)`,
		id, StatusQueued, StatusPendingPlacement, StatusRunning, StatusStarting,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkQueuedByID resets a job back to queued status (e.g., when sync finds it's still in queue)
// Updates last_synced_status since this is a sync operation.
func MarkQueuedByID(db *sql.DB, id int64) error {
	warnTransition(db, id, StatusQueued, false, status.SourceSSHSync)
	return MarkAttemptQueuedByID(db, id)
}

// SetPendingStatus sets the pending (target) status for a job.
// This represents what the user wants the job state to become.
// For jobs without an attempt (unplaced), it sets requested_status directly
// on the jobs table since there's no attempt row to hold pending_status.
func SetPendingStatus(database *sql.DB, jobID int64, s string) error {
	return RetryOnDatabaseLocked(context.Background(), "set pending status", func() error {
		attemptID, err := GetLatestAttemptID(database, jobID)
		if err != nil {
			return err
		}
		if attemptID == 0 {
			// No attempt exists: set requested_status directly on the job.
			_, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, s, jobID)
			return err
		}
		return SetAttemptPendingStatus(database, jobID, s)
	})
}

// SetRequestedStatus sets jobs.requested_status directly.
// This is used for hostless jobs where status should be derived locally
// without remote reconciliation.
//
// Requesting a cancel also stamps cancel_requested_at, which is what lets a
// later attempt be recognised as one the user had already asked not to
// happen. The stamp is only ever set, never cleared here: a requeue clears it
// explicitly alongside requested_status, so a stale cancel time cannot
// outlive the intent it records.
func SetRequestedStatus(database *sql.DB, jobID int64, s string) error {
	if s == StatusCanceled {
		_, err := database.Exec(
			`UPDATE jobs SET requested_status = ?, cancel_requested_at = ? WHERE id = ?`,
			s, time.Now().Unix(), jobID)
		return err
	}
	_, err := database.Exec(
		`UPDATE jobs SET requested_status = ?, cancel_requested_at = NULL WHERE id = ?`, s, jobID)
	return err
}

// CancelRequestedAt reports when a cancel was requested for the job, and
// whether one is recorded at all. Absent means no evidence — either no cancel,
// or one predating the column — and callers must treat it as "do not act"
// rather than "cancelled long ago".
func CancelRequestedAt(database *sql.DB, jobID int64) (time.Time, bool, error) {
	var at sql.NullInt64
	err := database.QueryRow(`SELECT cancel_requested_at FROM jobs WHERE id = ?`, jobID).Scan(&at)
	if err != nil {
		return time.Time{}, false, err
	}
	if !at.Valid || at.Int64 <= 0 {
		return time.Time{}, false, nil
	}
	return time.Unix(at.Int64, 0), true, nil
}

// ClearPendingStatus clears the pending status after reconciliation succeeds.
func ClearPendingStatus(db *sql.DB, jobID int64) error {
	return ClearAttemptPendingStatus(db, jobID)
}

// UpdateLastSyncedStatus updates the base status (what remote was at last sync).
func UpdateLastSyncedStatus(db *sql.DB, jobID int64, status string) error {
	_, err := db.Exec(`
		UPDATE job_attempts SET last_synced_status = ?
		WHERE id = `+latestOpenAttemptSubquery+`
		  AND status NOT IN (?, ?, ?, ?)`,
		status, jobID, StatusFailed, StatusDead, StatusKilled, StatusCanceled)
	return err
}

// ResetLastSyncedStatus clears last_synced_status for a job, marking it as
// needing re-dispatch. Used when the runner state shows a job is missing despite
// the DB believing it was already dispatched.
func ResetLastSyncedStatus(db *sql.DB, jobID int64) error {
	_, err := db.Exec(`UPDATE job_attempts SET last_synced_status = NULL WHERE job_id = ? AND end_time IS NULL`, jobID)
	return err
}

// UpdateStatusAndLastSynced updates both the current status and last synced status together.
// Used when sync confirms the remote state.
func UpdateStatusAndLastSynced(db *sql.DB, jobID int64, newStatus string) error {
	if err := checkOpenTransition(db, jobID, newStatus, false, status.SourceReconcile); err != nil {
		return err
	}
	return UpdateAttemptStatusAndLastSynced(db, jobID, newStatus)
}

// ClearPendingAndUpdateStatus clears pending status and updates both status fields.
// Used when reconciliation succeeds or when accepting remote state.
// Clears session_name for non-running states per spec: SessionImpliesRunning.
func ClearPendingAndUpdateStatus(db *sql.DB, jobID int64, newStatus string) error {
	if err := checkTransition(db, jobID, newStatus, false, status.SourceReconcile); err != nil {
		return err
	}
	return ClearAttemptPendingAndUpdateStatus(db, jobID, newStatus)
}

// RequeueByID resets a job back to queued status for user-initiated requeue.
// Unlike MarkQueuedByID, this does NOT set last_synced_status to queued,
// so the sync path will re-append the job to the remote queue if the immediate
// append fails.
func RequeueByID(database *sql.DB, id int64) error {
	var backend string
	if err := database.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, id).Scan(&backend); err != nil {
		return err
	}
	if backend == BackendSkyPilot {
		return fmt.Errorf("SkyPilot job %d cannot be requeued through Weft", id)
	}
	warnTransition(database, id, StatusQueued, false, status.SourceUserAction)

	// Cloud retries need a fresh attempt, not just requested_status='queued':
	// job_status derives cloud status from the latest attempt, so a terminal
	// attempt with exit_code != 0 would keep the job visible as 'failed'. See
	// UserRequeuesJob in specs/job-lifecycle.allium.
	var launchID sql.NullInt64
	_ = database.QueryRow(`SELECT launch_id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`, id).Scan(&launchID)
	if launchID.Valid {
		return RequeueFreshAttemptByID(database, id, "")
	}

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := RequeueByIDTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueByIDTx is RequeueByID's on-prem branch inside a caller-owned
// transaction. The caller is responsible for transition validation/logging.
func RequeueByIDTx(tx *sql.Tx, id int64) error {
	var backend string
	if err := tx.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, id).Scan(&backend); err != nil {
		return err
	}
	if backend == BackendSkyPilot {
		return fmt.Errorf("SkyPilot job %d cannot be requeued through Weft", id)
	}
	// On-prem jobs: close+recreate attempt with pending_status for three-way merge.
	now := time.Now().Unix()
	// The requeue closes every attempt, so any open move intent no longer
	// describes an executable move (RequeueAbandonsOpenMoveIntent in
	// specs/job-move.allium).
	if err := resolveOpenMoveIntentAbandonedTx(tx, id, now, "job requeued; move abandoned"); err != nil {
		return err
	}
	if err := closeOpenAttempts(tx, id, now); err != nil {
		return err
	}
	attemptID, err := createAttemptTx(tx, id, "", nil, StatusQueued)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE job_attempts SET pending_status = ? WHERE id = ?`, StatusQueued, attemptID); err != nil {
		return err
	}
	// Clear any prior user-cancel/kill intent so the job_status view derives
	// status from the new attempt instead of overriding it. Without this, a
	// job with requested_status='canceled' (or 'killed') silently flips back
	// to that state on the next view query — the cloud branch
	// (RequeueFreshAttemptByID) handles this; the on-prem branch did not.
	// See specs/job-lifecycle.allium UserRequeuesJob.
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusQueued, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET placement_blocked = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	return nil
}

// RequeueFreshAttemptByID creates a fresh queued attempt for manual retries.
// Unlike RequeueByID's cloud path, this always creates a new attempt number so
// retry budgeting and lifecycle accounting are reset from a clean attempt.
// The caller controls inventory placement by passing host="" for unplaced jobs
// or an inventory host name for pinned on-prem jobs.
func RequeueFreshAttemptByID(database *sql.DB, id int64, host string) error {
	return RequeueFreshAttemptByTarget(database, id, host, nil)
}

// RequeueFreshAttemptByTarget creates a fresh queued retry attempt and preserves
// a concrete target when one is known. Pass an inventory host, a live launch ID,
// or neither for an unplaced retry.
func RequeueFreshAttemptByTarget(database *sql.DB, id int64, host string, launchID *int64) error {
	var backend string
	if err := database.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, id).Scan(&backend); err != nil {
		return err
	}
	if backend == BackendSkyPilot {
		return fmt.Errorf("SkyPilot job %d cannot create a local retry attempt", id)
	}
	warnOpenTransition(database, id, StatusQueued, false, status.SourceUserAction)

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := RequeueFreshAttemptByTargetTx(tx, id, host, launchID); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueFreshAttemptByTargetTx creates a fresh queued retry attempt inside a
// caller-owned transaction.
func RequeueFreshAttemptByTargetTx(tx *sql.Tx, id int64, host string, launchID *int64) error {
	var backend string
	if err := tx.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, id).Scan(&backend); err != nil {
		return err
	}
	if backend == BackendSkyPilot {
		return fmt.Errorf("SkyPilot job %d cannot create a local retry attempt", id)
	}
	now := time.Now().Unix()
	// The requeue closes every attempt, so any open move intent no longer
	// describes an executable move (RequeueAbandonsOpenMoveIntent in
	// specs/job-move.allium).
	if err := resolveOpenMoveIntentAbandonedTx(tx, id, now, "job requeued; move abandoned"); err != nil {
		return err
	}
	if err := closeOpenAttempts(tx, id, now); err != nil {
		return err
	}
	attemptID, err := createAttemptTx(tx, id, host, launchID, StatusQueued)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE job_attempts SET pending_status = ?, pending_at = ?, last_synced_status = NULL WHERE id = ?`,
		StatusQueued, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusQueued, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET placement_blocked = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	// Manual retry starts a new launch chain for budgeting purposes.
	// Mark prior cloud attempts as superseded so relaunch logic does not keep
	// charging elapsed/spend from earlier failed chains.
	if _, err := tx.Exec(
		`UPDATE job_attempts
		 SET cloud_outcome = ?
		 WHERE job_id = ?
		   AND launch_id IS NOT NULL
		   AND end_time IS NOT NULL
		   AND COALESCE(cloud_outcome, '') != ?`,
		AttemptOutcomeSuperseded, id, AttemptOutcomeSuperseded,
	); err != nil {
		return err
	}
	return nil
}

// MoveQueuedJobToUnplaced clears a queued job's host assignment and promotes it
// for rental launch. This is the explicit move-to-cloud path: jobs that are not
// inventory- or rental-tagged gain the rental placement tag so they remain
// eligible for rental launch workflows, and the placement reason records the
// manual move. Use MoveQueuedJobToUnplacedWithReason for re-place/drain flows
// (e.g. the source-sync wedge drain) that return a job to the on-prem pool
// without routing it to cloud. Unlike ResetJobToUnplaced, this does not archive
// a run; it only clears queue placement metadata. If the visible queued
// placement is a user requeue intent over a terminal latest attempt, it creates
// a fresh unplaced attempt so job_status no longer exposes the terminal
// attempt's host.
func MoveQueuedJobToUnplaced(database *sql.DB, id int64) error {
	return moveQueuedJobToUnplaced(database, id, "manually moved to unplaced queue", true)
}

// MoveQueuedJobToUnplacedWithReason clears a queued job's host assignment and
// records the caller-supplied placement reason without promoting the job to
// rental. Used by re-place/drain flows that return a job to the unplaced on-prem
// pool; a host cordon — not a rental tag — keeps re-placement off the unhealthy
// host. Aside from suppressing the rental promotion and using the given reason,
// behavior matches MoveQueuedJobToUnplaced.
func MoveQueuedJobToUnplacedWithReason(database *sql.DB, id int64, reason string) error {
	return moveQueuedJobToUnplaced(database, id, reason, false)
}

func moveQueuedJobToUnplaced(database *sql.DB, id int64, reason string, promoteToRental bool) error {
	job, err := GetJobByID(database, id)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}
	if job.Backend == BackendSkyPilot {
		return fmt.Errorf("SkyPilot job %d cannot be moved to Weft's unplaced queue", id)
	}
	tags := append([]string(nil), job.Tags...)
	if promoteToRental && !job.HasTag(TagInventory) && !job.HasTag(TagRental) {
		tags = append(tags, TagRental)
	}
	tagValue, err := encodeTags(tags)
	if err != nil {
		return err
	}

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if err := resolveOpenMoveIntentAbandonedTx(tx, id, now, "job unplaced; move abandoned"); err != nil {
		tx.Rollback()
		return err
	}
	if err := resolveOpenPlacementIntentCanceledTx(tx, id, now, "job unplaced; placement abandoned"); err != nil {
		tx.Rollback()
		return err
	}
	result, err := tx.Exec(`UPDATE job_attempts SET host = '', launch_id = NULL, target_id = NULL, pending_status = NULL, pending_at = NULL, last_synced_status = NULL, queued_at = NULL WHERE job_id = ? AND end_time IS NULL`, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		if _, err := createAttemptTx(tx, id, "", nil, StatusQueued); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusQueued, id); err != nil {
		tx.Rollback()
		return err
	}
	_, err = tx.Exec(
		`UPDATE jobs
		 SET tags = ?,
		     placement_reasons = ?
		 WHERE id = ?`,
		tagValue, encodeStringSlice([]string{reason}), id,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// HasTagHostConflict reports whether a queued job's tags conflict with its
// current host placement (e.g. rental tag on an inventory host).
func (j *Job) HasTagHostConflict() bool {
	_, hasRequestedProvider := RequestedProvider(j.Tags)
	return j != nil &&
		j.EffectiveStatus() == StatusQueued &&
		j.HasInventoryHost() &&
		(HasRentalTag(j.Tags) || hasRequestedProvider)
}

// ResetJobToUnplaced resets a single job to unplaced state (queued with empty host),
// clearing cloud instance association and run metadata. Used when restarting cloud
// jobs whose original instance is no longer available.
func ResetJobToUnplaced(database *sql.DB, jobID int64) error {
	job, _ := GetJobByID(database, jobID)

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ResetJobToUnplacedTx(tx, jobID, job); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetJobToUnplacedTx resets a single job to unplaced state inside a
// caller-owned transaction.
func ResetJobToUnplacedTx(tx *sql.Tx, jobID int64, job *Job) error {
	if job != nil && job.Backend == BackendSkyPilot {
		return fmt.Errorf("external SkyPilot job %d cannot be reset to Weft's unplaced queue", jobID)
	}
	now := time.Now().Unix()
	if err := closeAttemptsAndRequeue(tx, jobID, now); err != nil {
		return err
	}
	if _, err := createAttemptTx(tx, jobID, "", nil, StatusQueued); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusQueued, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
		encodeStringSlice(resetJobPlacementReasons(job)), jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET placement_blocked = NULL WHERE id = ?`, jobID); err != nil {
		return err
	}
	return nil
}

func resetJobPlacementReasons(job *Job) []string {
	if job == nil {
		return []string{"job reset to unplaced queue"}
	}
	if job.LaunchID != nil && *job.LaunchID > 0 {
		return []string{fmt.Sprintf("cloud instance %d unavailable; job reset to unplaced queue", *job.LaunchID)}
	}
	return []string{"job reset to unplaced queue"}
}

// SetQueuedAtNow sets queued_at to the current time if it's not already set.
// Called when a job is successfully added to the remote queue.
func SetQueuedAtNow(db *sql.DB, jobID int64) error {
	_, err := db.Exec(
		`UPDATE job_attempts SET queued_at = ? WHERE id = `+latestOpenAttemptSubquery+` AND (queued_at IS NULL OR queued_at = 0)`,
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
		`SELECT MIN(queued_at) FROM job_status WHERE host = ? AND status = ? AND queued_at > 0 AND id != ? AND tombstoned = 0`,
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

	_, err = db.Exec(`UPDATE job_attempts SET queued_at = ? WHERE id = `+latestOpenAttemptSubquery, newQueuedAt, jobID)
	return err
}

// SetJobPriority updates the durable scheduling priority for a job.
func SetJobPriority(db *sql.DB, jobID int64, priority int) error {
	if priority < 0 {
		return fmt.Errorf("priority must be non-negative")
	}
	_, err := db.Exec(`UPDATE jobs SET priority = ? WHERE id = ?`, priority, jobID)
	return err
}

// SchedulingLess orders jobs by user scheduling intent, then job ID.
func SchedulingLess(a, b *Job) bool {
	if a == nil || b == nil {
		return a != nil
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}

// IsTerminalStatus returns true if the status represents a terminal state.
func IsTerminalStatus(s string) bool {
	return status.IsTerminal(s)
}

// CountQueuedByHost returns the number of queued jobs for a host
func CountQueuedByHost(db *sql.DB, host string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM job_status WHERE host = ? AND status = ? AND tombstoned = 0`,
		host, StatusQueued,
	).Scan(&count)
	return count, err
}

// CountQueueRunnerActiveByHost returns queued/running queue-runner jobs for a host.
func CountQueueRunnerActiveByHost(db *sql.DB, host string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM job_status
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

// RecordQueuedWithGPUTx records a queued job with GPU specification inside tx.
func RecordQueuedWithGPUTx(tx *sql.Tx, host, workingDir, command, description, gpu string) (int64, error) {
	return recordQueuedWithGPU(tx, 0, host, workingDir, command, description, gpu, false)
}

// RecordQueuedWithGPUAndID records a queued job using an explicit ID.
func RecordQueuedWithGPUAndID(db *sql.DB, id int64, host, workingDir, command, description, gpu string) error {
	_, err := recordQueuedWithGPU(db, id, host, workingDir, command, description, gpu, true)
	return err
}

// RecordQueuedWithGPUAndIDTx records a queued job using an explicit ID inside tx.
func RecordQueuedWithGPUAndIDTx(tx *sql.Tx, id int64, host, workingDir, command, description, gpu string) error {
	_, err := recordQueuedWithGPU(tx, id, host, workingDir, command, description, gpu, true)
	return err
}

func recordQueuedWithGPU(db dbExecer, id int64, host, workingDir, command, description, gpu string, explicitID bool) (int64, error) {
	if gpu == "" {
		gpu = ParseGPUFromCommandString(command)
	}
	var err error
	workingDir, err = workdir.Normalize(workingDir)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	// For unplaced jobs (no host), set requested_status='queued' so the view
	// derives 'queued' without an attempt. For placed jobs (host provided),
	// the attempt itself carries the status; requested_status is left NULL.
	var requestedStatus any
	if host == "" {
		requestedStatus = StatusQueued
	}
	if explicitID {
		_, err := db.Exec(
			`INSERT INTO jobs (id, working_dir, command, description, created_at, gpu, placement_host, requested_status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			 	working_dir = excluded.working_dir,
			 	command = excluded.command,
			 	description = excluded.description,
			 	gpu = excluded.gpu,
			 	placement_host = excluded.placement_host,
			 	requested_status = excluded.requested_status`,
			id, workingDir, command, description, now, gpu, host, requestedStatus,
		)
		if err != nil {
			return 0, err
		}
		// On-prem jobs with a host: create an attempt so the sync path can track them.
		if host != "" {
			if _, err := createAttemptTx(db, id, host, nil, StatusQueued); err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	result, err := db.Exec(
		`INSERT INTO jobs (working_dir, command, description, created_at, gpu, placement_host, requested_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		workingDir, command, description, now, gpu, host, requestedStatus,
	)
	if err != nil {
		return 0, err
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	// On-prem jobs with a host: create an attempt so the sync path can track them.
	if host != "" {
		if _, err := createAttemptTx(db, jobID, host, nil, StatusQueued); err != nil {
			return 0, err
		}
	}
	return jobID, nil
}

// RecordDraftJobWithGPU records a job that should remain in draft locally.
func RecordDraftJobWithGPU(db *sql.DB, host, workingDir, command, description, gpu string) (int64, error) {
	return RecordDraftJob(db, host, workingDir, command, description, gpu, "")
}

// RecordDraftJob records a job that should remain in draft locally, with optional dependency.
func RecordDraftJob(db *sql.DB, host, workingDir, command, description, gpu, depSpec string) (int64, error) {
	return recordDraftJob(db, host, workingDir, command, description, gpu, depSpec)
}

// RecordDraftJobWithGPUTx records a draft job inside tx.
func RecordDraftJobWithGPUTx(tx *sql.Tx, host, workingDir, command, description, gpu string) (int64, error) {
	return recordDraftJob(tx, host, workingDir, command, description, gpu, "")
}

func recordDraftJob(db dbExecer, host, workingDir, command, description, gpu, depSpec string) (int64, error) {
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
		`INSERT INTO jobs (working_dir, command, description, created_at, gpu, dep_spec, placement_host)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		workingDir, command, description, createdAt, gpu, depSpec, host,
	)
	if err != nil {
		return 0, err
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Create the attempt at draft status. Draft jobs with a host get an
	// attempt so the reconcile path can track pending_status on them.
	if host != "" {
		if _, err := createAttemptTx(db, jobID, host, nil, StatusDraft); err != nil {
			return 0, err
		}
	} else {
		// For hostless draft jobs, set requested_status so the view derives 'draft'.
		if _, err := db.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusDraft, jobID); err != nil {
			return 0, err
		}
	}
	return jobID, nil
}

// SetJobGPU updates the GPU field for a job
func SetJobGPU(db dbExecer, jobID int64, gpu string) error {
	_, err := db.Exec(`UPDATE jobs SET gpu = ? WHERE id = ?`, gpu, jobID)
	return err
}

// SetJobGPUClass updates the GPU class field for a job
func SetJobGPUClass(db dbExecer, jobID int64, gpuClass string) error {
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
	_, err := db.Exec(`UPDATE job_attempts SET remote_id = ? WHERE id = `+latestOpenAttemptSubquery, remoteID, jobID)
	return err
}

// SetJobRemoteState sets the backend-specific state and optional failure reason.
func SetJobRemoteState(db *sql.DB, jobID int64, remoteState, failureReason string) error {
	_, err := db.Exec(`UPDATE job_attempts SET remote_state = ?, failure_reason = ? WHERE id = `+latestOpenAttemptSubquery, remoteState, failureReason, jobID)
	return err
}

// setJobNullableInt updates a nullable integer column on the jobs table.
func setJobNullableInt(db dbExecer, jobID int64, column string, value *int) error {
	var v interface{}
	if value != nil {
		v = *value
	}
	_, err := db.Exec(`UPDATE jobs SET `+column+` = ? WHERE id = ?`, v, jobID)
	return err
}

// SetJobCPUAllotment updates the CPU allotment percent for a job (nil clears it).
func SetJobCPUAllotment(db dbExecer, jobID int64, allotment *int) error {
	return setJobNullableInt(db, jobID, "cpu_allotment", allotment)
}

// SetJobGPUMemGB updates the GPU memory reservation in GB per device (nil clears it).
func SetJobGPUMemGB(db dbExecer, jobID int64, gpuMemGB *int) error {
	return setJobNullableInt(db, jobID, "gpu_mem_gb", gpuMemGB)
}

// SetJobGPUMemMaxGB updates legacy GPU memory upper metadata in GB (nil clears it).
func SetJobGPUMemMaxGB(db dbExecer, jobID int64, gpuMemMaxGB *int) error {
	return setJobNullableInt(db, jobID, "gpu_mem_max_gb", gpuMemMaxGB)
}

// SetJobMaxComputeCap stores the resolved CUDA compute-capability cap. The
// empty string clears the column (NULL = unresolved); "any" marks the job as
// explicitly unbounded; numeric strings ("9.0", "12.0") record the cap.
func SetJobMaxComputeCap(db dbExecer, jobID int64, cap string) error {
	var v interface{}
	if cap != "" {
		v = cap
	}
	_, err := db.Exec(`UPDATE jobs SET max_compute_cap = ? WHERE id = ?`, v, jobID)
	return err
}

// SetJobSubmitToken stores the client-generated submit idempotency token.
func SetJobSubmitToken(db dbExecer, jobID int64, token string) error {
	var v interface{}
	if strings.TrimSpace(token) != "" {
		v = strings.TrimSpace(token)
	}
	_, err := db.Exec(`UPDATE jobs SET submit_token = ? WHERE id = ?`, v, jobID)
	return err
}

// FindJobIDBySubmitToken returns the job created for a submit token, if any.
func FindJobIDBySubmitToken(db dbExecer, token string) (int64, bool, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, false, nil
	}
	var jobID int64
	err := db.QueryRow(`SELECT id FROM jobs WHERE submit_token = ? LIMIT 1`, token).Scan(&jobID)
	if err == nil {
		return jobID, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return 0, false, err
}

// SetJobSubmitterSession stores the opaque agent-session id of the submitter.
// An empty value clears the column, which is the normal state for a job
// submitted from a plain shell or recorded by a process that is not the
// submitter (see config.Config.SubmitterSession).
func SetJobSubmitterSession(db dbExecer, jobID int64, session string) error {
	var v interface{}
	if strings.TrimSpace(session) != "" {
		v = strings.TrimSpace(session)
	}
	_, err := db.Exec(`UPDATE jobs SET submitter_session = ? WHERE id = ?`, v, jobID)
	return err
}

// JobSubmitterSession returns the submitting agent's session id for a job, or
// "" when none was recorded. It reads the jobs table directly rather than
// travelling on Job, because the shared job_status view cannot carry the
// column; see PopulateSubmitterSessions for the bulk equivalent and for why
// the view is off limits.
func JobSubmitterSession(db dbExecer, jobID int64) (string, error) {
	var session sql.NullString
	err := db.QueryRow(`SELECT submitter_session FROM jobs WHERE id = ?`, jobID).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return session.String, nil
}

// SetJobEnvVars updates the stored environment variables for a job.
// The values are stored as a JSON array; passing nil or an empty slice clears the field.
func SetJobEnvVars(db dbExecer, jobID int64, envVars []string) error {
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
func SetJobTags(db dbExecer, jobID int64, tags []string) error {
	value, err := encodeTags(tags)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET tags = ? WHERE id = ?`, value, jobID)
	return err
}

// SetJobMetadata updates the stored metadata JSON for a job (nil clears it).
//
// Metadata normally lives on the latest job_attempts row. A job submitted
// while unplaced has no attempt row yet, so its metadata is stored on the
// jobs table instead; the job_status view COALESCEs the two. Once the job
// gets an attempt, later SetJobMetadata calls (telemetry, resource usage)
// read-modify-write the full struct and carry the submission metadata onto
// the attempt row.
func SetJobMetadata(db dbExecer, jobID int64, meta *JobMetadata) error {
	value, err := encodeJobMetadata(meta)
	if err != nil {
		return err
	}
	// Update latest attempt (open or not — metadata can be set after completion).
	res, err := db.Exec(`UPDATE job_attempts SET job_metadata = ? WHERE id = `+latestAttemptSubquery, value, jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// No attempt row: the job is still unplaced. Persist on the jobs table so
	// submission metadata (cloud dependencies, disk floor) is not silently lost.
	res, err = db.Exec(`UPDATE jobs SET job_metadata = ? WHERE id = ?`, value, jobID)
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("SetJobMetadata: job %d not found", jobID)
	}
	return nil
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
		return fmt.Errorf("job %d: %w", jobID, ErrJobNotFound)
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
		return fmt.Errorf("job %d: %w", jobID, ErrJobNotFound)
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
func SetJobDepSpec(db dbExecer, jobID int64, depSpec string) error {
	if depSpec == "" {
		_, err := db.Exec(`UPDATE jobs SET dep_spec = NULL WHERE id = ?`, jobID)
		return err
	}
	_, err := db.Exec(`UPDATE jobs SET dep_spec = ? WHERE id = ?`, depSpec, jobID)
	return err
}

// SetJobInputs sets the data asset inputs for a job (stored as JSON array).
func SetJobInputs(db dbExecer, jobID int64, inputs []string) error {
	return setJobStringSlice(db, jobID, "inputs", inputs)
}

// SetJobObservedInputs sets the post-mortem observed inputs for a job.
// These are data inputs discovered after a failure (e.g., HF models found
// in the cache during a disk-full event that weren't declared via --input).
func SetJobObservedInputs(db *sql.DB, jobID int64, inputs []string) error {
	return setJobStringSlice(db, jobID, "observed_inputs", inputs)
}

// SetJobOutputs sets the data asset outputs for a job (stored as JSON array).
func SetJobOutputs(db dbExecer, jobID int64, outputs []string) error {
	return setJobStringSlice(db, jobID, "outputs", outputs)
}

// SetJobOutputDirs sets the convention-based output directories for a job (stored as JSON array).
func SetJobOutputDirs(db dbExecer, jobID int64, dirs []string) error {
	return setJobStringSlice(db, jobID, "output_dirs", dirs)
}

// SetJobProduces sets the artifact specs this job produces (stored as JSON array).
func SetJobProduces(db dbExecer, jobID int64, produces []string) error {
	return setJobStringSlice(db, jobID, "produces", produces)
}

// SetJobNeeds sets the artifact specs this job needs (stored as JSON array).
func SetJobNeeds(db dbExecer, jobID int64, needs []string) error {
	return setJobStringSlice(db, jobID, "needs", needs)
}

func setJobStringSlice(db dbExecer, jobID int64, column string, values []string) error {
	var arg any
	if len(values) > 0 {
		data, err := json.Marshal(values)
		if err != nil {
			return err
		}
		arg = string(data)
	}
	query := fmt.Sprintf(`UPDATE jobs SET %s = ? WHERE id = ?`, column)
	return RetryOnDatabaseLocked(context.Background(), "set job "+column, func() error {
		_, err := db.Exec(query, arg, jobID)
		return err
	})
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
func SetJobProject(db dbExecer, jobID int64, project string) error {
	var workingDir, command string
	if err := db.QueryRow(`SELECT working_dir, command FROM jobs WHERE id = ?`, jobID).Scan(&workingDir, &command); err != nil {
		return err
	}
	normalized, err := NormalizeProjectName(project, workingDir, command)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, normalized, jobID)
	return err
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

// FilterJobsByIDSet filters jobs to those whose IDs appear in ids. A nil or
// empty set is treated as "no restriction" and returns the input unchanged.
func FilterJobsByIDSet(jobs []*Job, ids map[int64]bool) []*Job {
	if len(ids) == 0 {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && ids[job.ID] {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

// ProjectHasAnyJobs reports whether any job is associated with the given project.
// An empty project returns true (no narrowing).
func ProjectHasAnyJobs(database *sql.DB, project string) (bool, error) {
	if project == "" {
		return true, nil
	}
	var exists int
	err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM jobs WHERE project = ? LIMIT 1)`, project).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
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
	if err := repairPlaceholderProjectsInTable(db, "jobs"); err != nil {
		return err
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
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE status = ? AND dep_spec IS NOT NULL AND dep_spec != '' AND tombstoned = 0`, jobSelectColumns)
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

// ListQueued returns queued jobs for a host.
func ListQueued(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE status = ? AND host = ? AND tombstoned = 0 ORDER BY priority DESC, id ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued, host)
}

// UpdateQueuedToRunning transitions a queued job to running
func UpdateQueuedToRunning(db *sql.DB, id int64) error {
	return UpdateAttemptRunning(db, id)
}

// UpdateQueuedToRunningWithSession transitions a queued job to running and sets session_name.
// Used when starting a queued job directly via tmux (not through queue runner).
func UpdateQueuedToRunningWithSession(db *sql.DB, id int64, sessionName string) error {
	if err := UpdateAttemptRunning(db, id); err != nil {
		return err
	}
	return SetAttemptSessionName(db, id, sessionName)
}

// ClearSessionName removes the session_name from a job.
// Used when a job that was started via tmux is now being managed by the queue runner.
func ClearSessionName(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE job_attempts SET session_name = NULL WHERE job_id = ? AND end_time IS NULL`, id)
	return err
}

// RecordCompletion updates a job with its exit code and end time.
// Also updates last_synced_status since this is a sync operation.
func RecordCompletion(db *sql.DB, host, sessionName string, exitCode int, endTime int64) error {
	// Find the running job by host+session_name via the attempt
	var jobID int64
	err := db.QueryRow(`
		SELECT ja.job_id FROM job_attempts ja
		WHERE ja.host = ? AND ja.session_name = ? AND ja.status = ? AND ja.end_time IS NULL
		ORDER BY ja.id DESC LIMIT 1`, host, sessionName, StatusRunning).Scan(&jobID)
	if err != nil {
		return nil // no matching job found
	}
	return UpdateAttemptCompletion(db, jobID, exitCode, endTime)
}

// MarkDead marks a running job as failed (unexpected termination).
// Also updates last_synced_status since this is a sync operation.
func MarkDead(db *sql.DB, host, sessionName string) error {
	// Find the running job by host+session_name via the attempt
	var jobID int64
	err := db.QueryRow(`
		SELECT ja.job_id FROM job_attempts ja
		WHERE ja.host = ? AND ja.session_name = ? AND ja.status = ? AND ja.end_time IS NULL
		ORDER BY ja.id DESC LIMIT 1`, host, sessionName, StatusRunning).Scan(&jobID)
	if err != nil {
		return nil // no matching job found
	}
	return UpdateAttemptDead(db, jobID)
}

// UpdateStartTime updates the start_time for a job (for jobs where start_time was initially null/0).
//
// Targets the latest attempt whether open or closed: completion sync can close
// an attempt (end_time set) before any sync tick managed to read the host's
// metadata, and the start time only becomes available from host-side evidence
// afterwards. Restricting the backfill to open attempts made that gap
// permanent — every later backfill silently no-oped, leaving completed
// attempts with end_time but NULL start_time. The (start_time IS NULL OR 0)
// guard keeps the write a pure backfill; it never overwrites a recorded start.
func UpdateStartTime(db *sql.DB, id int64, startTime int64) error {
	_, err := db.Exec(
		`UPDATE job_attempts SET start_time = ? WHERE id = `+latestAttemptSubquery+` AND (start_time IS NULL OR start_time = 0)`,
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
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 1 AND status IN (?, ?, ?, ?)`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning, StatusQueued, StatusStarting, StatusPaused)
}

// GetJob retrieves a job by host and session name (most recent)
func GetJob(db *sql.DB, host, sessionName string) (*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE host = ? AND session_name = ? ORDER BY start_time DESC LIMIT 1`, jobSelectColumns)
	row := db.QueryRow(query, host, sessionName)
	return scanJob(row)
}

// GetJobByID retrieves a job by ID
func GetJobByID(db *sql.DB, id int64) (*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE id = ?`, jobSelectColumns)
	row := db.QueryRow(query, id)
	return scanJob(row)
}

// GetJobsByIDs retrieves multiple jobs in a single query. Missing IDs are
// silently skipped (no error). Duplicate IDs are deduplicated.
func GetJobsByIDs(database *sql.DB, ids []int64) (map[int64]*Job, error) {
	out := make(map[int64]*Job, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return out, nil
	}
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE id IN (%s)`, jobSelectColumns, strings.Join(placeholders, ","))
	jobs, err := queryJobs(database, query, args...)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		out[j.ID] = j
	}
	return out, nil
}

// GetRunningJobsByHost retrieves all running jobs for a specific host
func GetRunningJobsByHost(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE host = ? AND status IN (?, ?) ORDER BY start_time DESC`, jobSelectColumns)
	rows, err := db.Query(query, host, StatusRunning, StatusPaused)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

// GetJobsByHostAndStatus retrieves jobs for a specific host with a specific status
func GetJobsByHostAndStatus(db *sql.DB, host, status string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE host = ? AND status = ? AND tombstoned = 0 ORDER BY created_at ASC`, jobSelectColumns)
	rows, err := db.Query(query, host, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

// GetJobsByHost retrieves all jobs for a specific host
func GetJobsByHost(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE host = ? ORDER BY id DESC`, jobSelectColumns)
	rows, err := db.Query(query, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

// jobScanFields holds nullable scan targets for job queries. Both scanJob and
// scanJobs use this struct so that column additions only need one set of
// variable declarations and one populateJob implementation.
type jobScanFields struct {
	sessionName      sql.NullString
	desc             sql.NullString
	generatedDesc    sql.NullString
	generationHash   sql.NullString
	errorMsg         sql.NullString
	backend          sql.NullString
	remoteID         sql.NullString
	remoteState      sql.NullString
	failureReason    sql.NullString
	gpu              sql.NullString
	gpuClass         sql.NullString
	cpuAllotment     sql.NullInt64
	gpuMemGB         sql.NullInt64
	gpuMemMaxGB      sql.NullInt64
	maxComputeCap    sql.NullString
	envVars          sql.NullString
	tags             sql.NullString
	depSpec          sql.NullString
	inputs           sql.NullString
	observedInputs   sql.NullString
	outputs          sql.NullString
	outputDirs       sql.NullString
	produces         sql.NullString
	needs            sql.NullString
	project          sql.NullString
	priority         sql.NullInt64
	createdAt        sql.NullInt64
	queuedAt         sql.NullInt64
	startTime        sql.NullInt64
	endTime          sql.NullInt64
	exitCode         sql.NullInt64
	tombstoned       sql.NullInt64
	lastSyncedStatus sql.NullString
	pendingStatus    sql.NullString
	pendingAt        sql.NullInt64
	jobMetadata      sql.NullString
	cost             sql.NullFloat64
	errorDiagnosis   sql.NullString
	retryCount       sql.NullInt64
	placementMeta    sql.NullString
	placementReasons sql.NullString
	cliOverrides     sql.NullString
	cloudInstanceID  sql.NullInt64
	campaignJobIndex sql.NullInt64
	latestRunID      sql.NullInt64
	placementBlocked sql.NullString
}

// scanDests returns pointers to all scan targets in jobSelectColumns order.
// The caller must also pass &j.ID, &j.Host, &j.WorkingDir, &j.Command, &j.Status
// which are scanned directly into Job fields.
func (f *jobScanFields) scanDests(j *Job) []any {
	return []any{
		&j.ID, &j.Host, &f.sessionName, &j.WorkingDir, &j.Command,
		&f.desc, &f.generatedDesc, &f.generationHash,
		&f.priority, &f.createdAt, &f.queuedAt, &f.startTime, &f.endTime, &f.exitCode,
		&j.Status, &f.errorMsg, &f.backend, &f.remoteID, &f.remoteState,
		&f.failureReason, &f.gpu, &f.gpuClass,
		&f.cpuAllotment, &f.gpuMemGB, &f.gpuMemMaxGB, &f.maxComputeCap,
		&f.envVars, &f.tags, &f.depSpec,
		&f.inputs, &f.observedInputs, &f.outputs, &f.outputDirs,
		&f.produces, &f.needs, &f.project, &f.tombstoned,
		&f.lastSyncedStatus, &f.pendingStatus, &f.pendingAt,
		&f.jobMetadata, &f.cost, &f.errorDiagnosis, &f.retryCount,
		&f.placementMeta, &f.placementReasons, &f.cliOverrides,
		&f.cloudInstanceID, &f.campaignJobIndex, &f.latestRunID,
		&f.placementBlocked,
	}
}

// populateJob copies nullable scan fields into the Job struct.
func (f *jobScanFields) populateJob(j *Job) {
	if f.sessionName.Valid {
		j.SessionName = f.sessionName.String
	}
	if f.desc.Valid {
		j.Description = f.desc.String
	}
	if f.generatedDesc.Valid {
		j.GeneratedDescription = f.generatedDesc.String
	}
	if f.generationHash.Valid {
		j.GenerationHash = f.generationHash.String
	}
	if f.errorMsg.Valid {
		j.ErrorMessage = f.errorMsg.String
	}
	if f.backend.Valid {
		j.Backend = f.backend.String
	}
	if f.remoteID.Valid {
		j.RemoteID = f.remoteID.String
	}
	if f.remoteState.Valid {
		j.RemoteState = f.remoteState.String
	}
	if f.failureReason.Valid {
		j.FailureReason = f.failureReason.String
	}
	if f.gpu.Valid {
		j.GPU = f.gpu.String
	}
	if f.gpuClass.Valid {
		j.GPUClass = f.gpuClass.String
	}
	if f.cpuAllotment.Valid {
		val := int(f.cpuAllotment.Int64)
		j.CPUAllotment = &val
	}
	if f.gpuMemGB.Valid {
		val := int(f.gpuMemGB.Int64)
		j.GPUMemGB = &val
	}
	if f.gpuMemMaxGB.Valid {
		val := int(f.gpuMemMaxGB.Int64)
		j.GPUMemMaxGB = &val
	}
	if f.maxComputeCap.Valid {
		j.MaxComputeCap = f.maxComputeCap.String
	}
	j.EnvVars = decodeEnvVars(f.envVars)
	j.Tags = decodeTags(f.tags)
	if f.depSpec.Valid {
		j.DepSpec = f.depSpec.String
	}
	j.Inputs = decodeStringSlice(f.inputs)
	j.ObservedInputs = decodeStringSlice(f.observedInputs)
	j.Outputs = decodeStringSlice(f.outputs)
	j.OutputDirs = decodeStringSlice(f.outputDirs)
	j.Produces = decodeStringSlice(f.produces)
	j.Needs = decodeStringSlice(f.needs)
	if f.project.Valid {
		j.Project = f.project.String
	}
	if f.priority.Valid {
		j.Priority = int(f.priority.Int64)
	}
	if f.createdAt.Valid {
		j.CreatedAt = f.createdAt.Int64
	}
	if f.queuedAt.Valid {
		j.QueuedAt = f.queuedAt.Int64
	}
	if f.startTime.Valid {
		j.StartTime = f.startTime.Int64
	}
	if f.endTime.Valid {
		j.EndTime = &f.endTime.Int64
	}
	if f.exitCode.Valid {
		code := int(f.exitCode.Int64)
		j.ExitCode = &code
	}
	if f.tombstoned.Valid {
		j.Tombstoned = f.tombstoned.Int64 != 0
	}
	if f.lastSyncedStatus.Valid {
		j.LastSyncedStatus = f.lastSyncedStatus.String
	}
	if f.pendingStatus.Valid {
		j.PendingStatus = &f.pendingStatus.String
	}
	if f.pendingAt.Valid {
		j.PendingAt = &f.pendingAt.Int64
	}
	j.Metadata = decodeJobMetadata(f.jobMetadata)
	if j.Metadata != nil && len(j.Metadata.BestEffortInputs) > 0 {
		j.BestEffortInputs = append([]string(nil), j.Metadata.BestEffortInputs...)
	}
	if f.cost.Valid {
		j.Cost = &f.cost.Float64
	}
	if f.errorDiagnosis.Valid {
		j.ErrorDiagnosis = f.errorDiagnosis.String
	}
	if f.retryCount.Valid {
		j.RetryCount = int(f.retryCount.Int64)
	}
	j.PlacementMeta = decodePlacementMeta(f.placementMeta)
	j.PlacementReasons = decodeStringSlice(f.placementReasons)
	j.PlacementBlockedJSON = f.placementBlocked.String
	j.CLIResourceOverrides = decodeCLIResourceOverrides(f.cliOverrides)
	if f.cloudInstanceID.Valid {
		j.LaunchID = &f.cloudInstanceID.Int64
	}
	if f.campaignJobIndex.Valid {
		v := int(f.campaignJobIndex.Int64)
		j.CampaignJobIndex = &v
	}
	if f.latestRunID.Valid {
		j.LatestRunID = &f.latestRunID.Int64
	}
	if j.Backend == "" {
		j.Backend = BackendQueueRunner
	}
}

func scanJob(row *sql.Row) (*Job, error) {
	var j Job
	var f jobScanFields
	err := row.Scan(f.scanDests(&j)...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.populateJob(&j)
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
	if provider, ok := providerFromTag(tag); ok {
		return TagProviderPrefix + provider
	}
	switch tag {
	case TagCloudLegacy:
		return TagRental
	case TagOnPremLegacy:
		return TagInventory
	case TagPreemptibleLegacy:
		return TagInterruptible
	case TagComputeIntensiveLegacy:
		return TagCPUIntensive
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

// IsPreemptibleTag reports whether the tag means "interruptible/preemptible placement".
func IsPreemptibleTag(tag string) bool {
	return CanonicalizeTag(tag) == TagPreemptible
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

// HasPreemptibleTag reports whether the tag set allows interruptible/preemptible placement.
func HasPreemptibleTag(tags []string) bool {
	for _, tag := range tags {
		if IsPreemptibleTag(tag) {
			return true
		}
	}
	return false
}

// HasBenchmarkTag reports whether the tag set marks the job as a benchmark.
func HasBenchmarkTag(tags []string) bool {
	for _, tag := range tags {
		if CanonicalizeTag(tag) == TagBenchmark {
			return true
		}
	}
	return false
}

// HasCPUIntensiveTag reports whether the tag set marks the job as
// cpu-intensive, accepting legacy spellings via canonicalization.
func HasCPUIntensiveTag(tags []string) bool {
	for _, tag := range tags {
		if CanonicalizeTag(tag) == TagCPUIntensive {
			return true
		}
	}
	return false
}

// IsPlacementTag reports whether a tag directly affects scheduling or execution.
// Tags that only affect display/filtering (e.g. 'processed' or user labels) are
// not placement tags and can be edited on a running job.
func IsPlacementTag(tag string) bool {
	canonical := CanonicalizeTag(tag)
	switch canonical {
	case TagExclusive, TagBenchmark, TagRental, TagInventory, TagInterruptible, TagCPUIntensive:
		return true
	}
	return strings.HasPrefix(canonical, TagProviderPrefix)
}

// ProviderTag returns the canonical reserved provider tag for a provider name.
func ProviderTag(provider string) (string, error) {
	p := normalizeProviderName(provider)
	if p == "" {
		return "", fmt.Errorf("unsupported provider %q (expected one of: vastai, runpod)", strings.TrimSpace(provider))
	}
	return TagProviderPrefix + p, nil
}

// RequestedProvider returns a provider requested via reserved tags.
func RequestedProvider(tags []string) (string, bool) {
	for _, tag := range tags {
		if provider, ok := providerFromTag(tag); ok {
			return provider, true
		}
	}
	return "", false
}

func normalizeProviderName(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "vastai":
		return "vastai"
	case "runpod":
		return "runpod"
	default:
		return ""
	}
}

func providerFromTag(tag string) (string, bool) {
	trimmed := strings.TrimSpace(tag)
	if len(trimmed) < len(TagProviderPrefix) {
		return "", false
	}
	if !strings.EqualFold(trimmed[:len(TagProviderPrefix)], TagProviderPrefix) {
		return "", false
	}
	provider := normalizeProviderName(trimmed[len(TagProviderPrefix):])
	if provider == "" {
		return "", false
	}
	return provider, true
}

func validateReservedPlacementTags(tags []string) error {
	if HasRentalTag(tags) && HasInventoryTag(tags) {
		return fmt.Errorf("tags %q and %q cannot be combined", TagRental, TagInventory)
	}
	if HasBenchmarkTag(tags) && HasPreemptibleTag(tags) {
		return fmt.Errorf("tags %q and %q cannot be combined: preemption invalidates benchmark timing", TagBenchmark, TagPreemptible)
	}
	var selectedProvider string
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if len(trimmed) >= len(TagProviderPrefix) && strings.EqualFold(trimmed[:len(TagProviderPrefix)], TagProviderPrefix) {
			provider := normalizeProviderName(trimmed[len(TagProviderPrefix):])
			if provider == "" {
				return fmt.Errorf("unsupported provider tag %q (supported: %q, %q)", tag, TagProviderVastai, TagProviderRunpod)
			}
			if selectedProvider != "" && selectedProvider != provider {
				return fmt.Errorf("tags %q and %q cannot be combined", TagProviderPrefix+selectedProvider, TagProviderPrefix+provider)
			}
			selectedProvider = provider
		}
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

// toHostSet builds a set from a host name slice, trimming whitespace and
// skipping empty strings.
func toHostSet(hosts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h != "" {
			set[h] = struct{}{}
		}
	}
	return set
}

// FilterByFreshStatus keeps jobs whose status is trustworthy: inventory jobs
// on a recently-synced host, plus all cloud and unplaced jobs (whose status
// is tracked locally). An empty freshHosts list means no filtering.
func FilterByFreshStatus(jobs []*Job, freshHosts []string) []*Job {
	hostSet := toHostSet(freshHosts)
	if len(hostSet) == 0 {
		return jobs
	}
	filtered := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		if job.HasFreshStatus(hostSet) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

// FilterByHost keeps only jobs whose host matches one of the given hosts.
// Unlike FilterByFreshStatus, this is a strict content filter (e.g. for --host).
func FilterByHost(jobs []*Job, hosts []string) []*Job {
	hostSet := toHostSet(hosts)
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
		var f jobScanFields
		if err := rows.Scan(f.scanDests(&j)...); err != nil {
			return nil, err
		}
		f.populateJob(&j)
		jobs = append(jobs, &j)
	}
	return jobs, rows.Err()
}

// ListJobs returns jobs matching the given filters
func ListJobs(db *sql.DB, status, host string, limit int, tags []string, processedFilter string) ([]*Job, error) {
	return ListJobsWithMaxAge(db, status, host, limit, 0, tags, processedFilter)
}

// ListJobsByStatuses returns jobs matching any provided status and optional host/project filters.
// An empty statuses slice means no status filter.
func ListJobsByStatuses(db *sql.DB, statuses []string, host, project string, limit int, tags []string, processedFilter string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0`, qualifiedJobSelectColumns("job_status"))
	args := make([]interface{}, 0, len(statuses)+4)

	if len(statuses) > 0 {
		placeholders := make([]string, 0, len(statuses))
		for _, status := range statuses {
			trimmed := strings.TrimSpace(status)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			query += ` AND status IN (` + strings.Join(placeholders, ", ") + `)`
		}
	}
	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}
	if project != "" {
		query += ` AND project = ?`
		args = append(args, project)
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

// ListJobsWithMaxAge returns jobs, optionally filtered by status, host, and age.
// maxAgeDays of 0 means no age limit.
func ListJobsWithMaxAge(db *sql.DB, status, host string, limit, maxAgeDays int, tags []string, processedFilter string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0`, qualifiedJobSelectColumns("job_status"))
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

	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0`, qualifiedJobSelectColumns("job_status"))
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
	query += fmt.Sprintf(` AND (host IN (%s) OR host = '' OR launch_id IS NOT NULL)`, strings.Join(placeholders, ", "))
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
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE status = ? AND host = ? AND tombstoned = 0 ORDER BY start_time DESC`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning, host)
}

// ListAllRunning returns all running jobs across all hosts
func ListAllRunning(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE status = ? AND tombstoned = 0 ORDER BY start_time DESC`, jobSelectColumns)
	return queryJobs(db, query, StatusRunning)
}

// ListRecentFailed returns recently failed jobs (last 24 hours)
// Includes: completed with non-zero exit code, status=failed, status=dead
func ListRecentFailed(db *sql.DB, limit int) ([]*Job, error) {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	query := fmt.Sprintf(`SELECT %s FROM job_status
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		AND end_time IS NOT NULL
		AND end_time >= ?
		AND status IN (?, ?, ?, ?, ?)
		ORDER BY end_time DESC, id DESC`, jobSelectColumns)
	return queryJobs(db, query, sinceUnix, StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled)
}

// UpdateErrorDiagnosis stores an error diagnosis and increments retry count for a job.
func UpdateErrorDiagnosis(db *sql.DB, id int64, diagnosis string, retryCount int) error {
	return SetAttemptErrorDiagnosis(db, id, diagnosis)
}

// UpdateRunErrorDiagnosis updates the error_diagnosis on a job_run by run ID.
func UpdateRunErrorDiagnosis(db *sql.DB, runID int64, diagnosis string) error {
	// Try job_attempts first, fall back to job_runs for legacy data
	_, err := db.Exec(`UPDATE job_attempts SET error_diagnosis = ? WHERE id = ?`, diagnosis, runID)
	return err
}

// ListRecentFailedUndiagnosed returns recently failed jobs that have not been diagnosed yet.
// These are failed/dead jobs, plus completed jobs with non-zero exit code,
// retry_count == 0, and no error_diagnosis.
func ListRecentFailedUndiagnosed(db *sql.DB, limit int) ([]*Job, error) {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		AND end_time > ?
		AND (
			(status = ? AND exit_code IS NOT NULL AND exit_code != 0)
			OR status = ?
			OR status = ?
		)
		AND (retry_count IS NULL OR retry_count = 0)
		AND (error_diagnosis IS NULL OR error_diagnosis = '')
		ORDER BY end_time DESC
		LIMIT ?`, jobSelectColumns)
	return queryJobs(db, query, cutoff, StatusCompleted, StatusFailed, StatusDead, limit)
}

// ListUniqueRunningHosts returns all unique hosts with running jobs
func ListUniqueRunningHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM job_status WHERE status IN (?, ?, ?) AND tombstoned = 0`, StatusRunning, StatusStarting, StatusPaused)
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
			SELECT host FROM job_status
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
	rows, err := db.Query(`SELECT DISTINCT host FROM job_status WHERE status = ? AND tombstoned = 0`, StatusQueued)
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
	rows, err := db.Query(`SELECT DISTINCT host FROM job_status
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
	rows, err := db.Query(`SELECT DISTINCT host FROM job_status WHERE status = ? AND tombstoned = 0 AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?)`,
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND status IN (?, ?, ?, ?) AND tombstoned = 0
		ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost), StatusRunning, StatusStarting, StatusPaused, StatusQueued)
}

// ListActiveOnPremJobs returns all non-cloud active jobs with host assignments.
func ListActiveOnPremJobs(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE effective_target_kind = ?
		AND status IN (?, ?, ?, ?) AND tombstoned = 0
		ORDER BY host ASC,
			CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END,
			id ASC`, jobSelectColumns)
	return queryJobs(db, query, string(JobTargetInventoryHost), StatusRunning, StatusStarting, StatusPaused, StatusQueued)
}

// ListUnsyncedQueuedJobs returns queued jobs on a host that haven't been pushed
// to the remote queue yet (last_synced_status is not 'queued' and no pending operation).
func ListUnsyncedQueuedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND status = ?
		  AND (last_synced_status IS NULL OR last_synced_status != ?)
		  AND (pending_status IS NULL OR pending_status = ?) AND tombstoned = 0
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost), StatusQueued, StatusQueued, StatusQueued)
}

// ListSyncedQueuedJobs returns queued jobs on a host that were already pushed to
// the remote queue (last_synced_status = 'queued') but may be missing from the
// runner's live state (e.g. runner crashed before processing the command).
func ListSyncedQueuedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND status = ?
		  AND last_synced_status = ? AND pending_status IS NULL AND tombstoned = 0
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost), StatusQueued, StatusQueued)
}

// ListDraftJobsPendingSync returns draft jobs that still need remote cleanup.
func ListDraftJobsPendingSync(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND status = ? AND tombstoned = 0
		  AND (pending_status = ? OR IFNULL(last_synced_status, '') <> ?)
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost), StatusDraft, StatusDraft, StatusDraft)
}

// ListJobsPendingReconciliation returns jobs that have unresolved pending operations.
// These are jobs where the user requested a status change (kill, cancel, etc.) that
// may not have been applied to the remote yet.
func ListJobsPendingReconciliation(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND pending_status IS NOT NULL AND tombstoned = 0
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost))
}

// ListPotentiallyRestartedJobs returns jobs that may have been restarted by the queue runner.
// These are queue runner jobs (no session name) that are in terminal status (failed, dead)
// but may have been re-queued and started again.
func ListPotentiallyRestartedJobs(db *sql.DB, host string) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE host = ? AND effective_target_kind = ? AND (backend IS NULL OR backend = ?)
		  AND status IN (?, ?) AND tombstoned = 0
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, host, string(JobTargetInventoryHost), BackendQueueRunner, StatusFailed, StatusDead)
}

// ListAllQueued returns all queued jobs across all hosts
func ListAllQueued(db *sql.DB) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE status = ? AND tombstoned = 0 ORDER BY start_time ASC`, jobSelectColumns)
	return queryJobs(db, query, StatusQueued)
}

// ListUniqueHosts returns all unique hosts from all jobs
func ListUniqueHosts(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT host FROM job_status WHERE tombstoned = 0 AND host != '' ORDER BY host`)
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

// ListHostsForTUI returns host names for dashboard/monitor views.
// It includes:
// - inventory hosts from jobs and cached host info
// - only active rental hosts (running/launching/grace launches)
func ListHostsForTUI(database *sql.DB) ([]string, error) {
	jobHosts, err := ListUniqueHosts(database)
	if err != nil {
		return nil, err
	}

	cachedHosts, err := LoadAllCachedHosts(database)
	if err != nil {
		cachedHosts = nil
	}

	runningLaunches, err := ListRunningLaunches(database)
	if err != nil {
		return nil, err
	}
	runningRentalHosts := make(map[string]struct{}, len(runningLaunches)*2)
	for _, launch := range runningLaunches {
		if launch == nil || launch.ID <= 0 {
			continue
		}
		runningRentalHosts[LaunchHost(launch.ID)] = struct{}{}
		if provider := strings.TrimSpace(strings.ToLower(launch.Provider)); provider != "" {
			runningRentalHosts[provider+":"+strconv.FormatInt(launch.ID, 10)] = struct{}{}
		}
	}

	hostSet := make(map[string]struct{}, len(jobHosts)+len(cachedHosts)+len(runningRentalHosts))
	addHost := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if IsLaunchHost(name) {
			if _, ok := runningRentalHosts[name]; !ok {
				return
			}
		}
		hostSet[name] = struct{}{}
	}

	for _, host := range jobHosts {
		addHost(host)
	}
	for _, cached := range cachedHosts {
		if cached == nil {
			continue
		}
		addHost(cached.Name)
	}
	for host := range runningRentalHosts {
		hostSet[host] = struct{}{}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	util.NaturalSortStrings(hosts)
	return hosts, nil
}

// SearchJobs searches jobs by description or command
func SearchJobs(db *sql.DB, query string, limit int) ([]*Job, error) {
	pattern := "%" + query + "%"
	stmt := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0 AND (description LIKE ? OR command LIKE ?) ORDER BY start_time DESC`, jobSelectColumns)
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
		`DELETE FROM jobs WHERE id IN (SELECT id FROM job_status WHERE status IN (?, ?, ?, ?, ?) AND start_time < ?)`,
		StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, cutoff,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneJobs tombstones terminal jobs so they no longer appear in listings.
func PruneJobs(db *sql.DB, deadOnly bool, olderThan *time.Time) (int64, error) {
	query := `UPDATE jobs SET tombstoned = 1 WHERE tombstoned = 0
		AND id IN (SELECT id FROM job_status WHERE tombstoned = 0`
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

	query += `)`

	result, err := db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListJobsForPrune returns jobs that would be deleted by prune
func ListJobsForPrune(db *sql.DB, deadOnly bool, olderThan *time.Time) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0 AND `, jobSelectColumns)
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
		if legacyQuery, ok := rewriteQueryForMissingCLIOverrides(query, err); ok {
			rows, err = db.Query(legacyQuery, args...)
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

func queryJobsTx(tx *sql.Tx, query string, args ...interface{}) ([]*Job, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		if legacyQuery, ok := rewriteQueryForMissingCLIOverrides(query, err); ok {
			rows, err = tx.Query(legacyQuery, args...)
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanJobs(rows)
}

func rewriteQueryForMissingCLIOverrides(query string, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "no such column") {
		return "", false
	}
	if !strings.Contains(msg, "cli_overrides") {
		return "", false
	}
	replacements := map[string]string{
		"placement_reasons, cli_overrides, launch_id":                                  "placement_reasons, NULL AS cli_overrides, launch_id",
		"job_status.placement_reasons, job_status.cli_overrides, job_status.launch_id": "job_status.placement_reasons, NULL AS cli_overrides, job_status.launch_id",
		"js.placement_reasons, js.cli_overrides, js.launch_id":                         "js.placement_reasons, NULL AS cli_overrides, js.launch_id",
		"j.placement_reasons, j.cli_overrides, j.launch_id":                            "j.placement_reasons, NULL AS cli_overrides, j.launch_id",
	}
	rewritten := query
	for oldExpr, newExpr := range replacements {
		rewritten = strings.ReplaceAll(rewritten, oldExpr, newExpr)
	}
	if rewritten == query {
		return "", false
	}
	return rewritten, true
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

// GetJobRunIDs returns all run/attempt IDs for a job, ordered most recent first.
func GetJobRunIDs(database *sql.DB, jobID int64) ([]int64, error) {
	rows, err := database.Query(`
		SELECT id FROM job_attempts WHERE job_id = ?
		ORDER BY id DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
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

// RequestsGPU reports whether the job declares a GPU *resource* requirement
// (gpu-class or gpu-mem). Torch-derived arch caps and CUDA/driver floors
// apply only to such jobs. Contrast UsesGPU, which also counts a bare device
// pin (--gpu 0): a device-pinned job is UsesGPU-true but RequestsGPU-false.
func (j *Job) RequestsGPU() bool {
	return j.GPUClass != "" || (j.GPUMemGB != nil && *j.GPUMemGB > 0)
}

// HasAssignedHost reports whether the job currently has a concrete host target.
func (j *Job) HasAssignedHost() bool {
	return j.HasInventoryHost()
}

// EffectiveStatus returns the status to use for UI decisions.
// Returns PendingStatus if set (the desired/target state), otherwise Status.
// A terminal status always wins — a stale PendingStatus cannot override a
// completed/failed/killed job.
// A job without a host cannot actually be running, starting, or paused; treat
// those impossible states as queued so the UI does not report them as active on
// a nonexistent host.
func (j *Job) EffectiveStatus() string {
	status := j.Status
	if j.PendingStatus != nil && !IsTerminalStatus(j.Status) {
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
// UsesGPU reports whether the job has any GPU association (device pin,
// class, or memory). For constraint gating (arch caps, CUDA/driver floors)
// use RequestsGPU, which excludes bare device pins.
func (j *Job) UsesGPU() bool {
	return j.GPU != "" || j.GPUClass != "" || j.GPUMemGB != nil
}

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

// HostInfoStaleThreshold is the age beyond which CachedHostInfo.LastUpdated
// is considered stale — shared by the dashboard, web UI, and list-TUI Host
// footer so "last seen" messaging fires at a single breakpoint.
const HostInfoStaleThreshold = 5 * time.Minute

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
		INSERT OR REPLACE INTO host_info_cache (name, arch, os_version, model, cpu_count, cpu_model, cpu_freq, mem_total, gpus_json, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		info.Name, info.Arch, info.OSVersion, info.Model, info.CPUCount, info.CPUModel, info.CPUFreq, info.MemTotal, info.GPUsJSON, info.LastUpdated,
	)
	if err != nil {
		return err
	}
	return UpsertInventoryExecutionTarget(db, info.Name)
}

// LoadCachedHostInfo retrieves cached host information by name
func LoadCachedHostInfo(db *sql.DB, name string) (*CachedHostInfo, error) {
	row := db.QueryRow(`
		SELECT name, arch, os_version, model, cpu_count, cpu_model, cpu_freq, mem_total, gpus_json, last_updated
		FROM host_info_cache WHERE name = ?`, name)

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
		FROM host_info_cache ORDER BY name`)
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

const CloudSyncTargetName = "__cloud__"

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

// RecordCloudSync stores the latest successful cloud sync timestamp.
func RecordCloudSync(db *sql.DB, syncedAt time.Time) error {
	return RecordHostSync(db, CloudSyncTargetName, syncedAt)
}

// GetLastHostSync returns the timestamp when a host was last synced.
func GetLastHostSync(db *sql.DB, host string) time.Time {
	var ts int64
	err := db.QueryRow(`SELECT last_synced FROM host_syncs WHERE name = ?`, host).Scan(&ts)
	if err != nil || ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// GetLastCloudSync returns the timestamp when cloud state was last synced.
func GetLastCloudSync(db *sql.DB) time.Time {
	return GetLastHostSync(db, CloudSyncTargetName)
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
		if name == CloudSyncTargetName {
			continue
		}
		if name != "" && lastSynced > 0 {
			times[name] = time.Unix(lastSynced, 0)
		}
	}
	return times, rows.Err()
}

// ListHostsSyncedSince returns hosts synced since the provided timestamp.
func ListHostsSyncedSince(db *sql.DB, since time.Time) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM host_syncs WHERE last_synced >= ? AND name != ? ORDER BY name`, since.Unix(), CloudSyncTargetName)
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
	_, err := db.Exec(`DELETE FROM host_info_cache WHERE name = ?`, name)
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
func AddDeferredOperation(db *sql.DB, host, operation string, jobID int64, payload string) error {
	_, err := AddDeferredOperationReturningID(db, host, operation, jobID, payload)
	return err
}

// AddDeferredOperationReturningID adds an operation and returns its ID
func AddDeferredOperationReturningID(db *sql.DB, host, operation string, jobID int64, payload string) (int64, error) {
	createdAt := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO deferred_operations (host, operation, job_id, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		host, operation, jobID, payload, createdAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetDeferredOperations returns all deferred operations for a host
func GetDeferredOperations(db *sql.DB, host string) ([]*DeferredOperation, error) {
	rows, err := db.Query(
		`SELECT id, host, operation, job_id, payload, created_at
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
		var payload sql.NullString
		if err := rows.Scan(&op.ID, &op.Host, &op.Operation, &op.JobID, &payload, &op.CreatedAt); err != nil {
			return nil, err
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

// AllowDevMigrationEnv lets a developer who means it migrate the shared
// database from a build that is not the installed one.
const AllowDevMigrationEnv = "WEFT_ALLOW_DEV_MIGRATION"

// checkDevBuildMayMigrate refuses to migrate the shared database from a binary
// that is not the one on PATH.
//
// From the installed binary's point of view the migration is one-way and
// silent: the failure surfaces later, in another process, in another project,
// as "schema is at version N but this weft binary only knows N-1". The moment
// of migration is the only place the warning is cheap (wb75).
//
// Only the shared jobs database is protected. A developer pointing at their own
// file is doing nothing to anyone else, and bugs.db needs no equivalent guard
// because initBugSchema is idempotent DDL with no version counter to advance —
// an older binary can still open a bugs.db a newer one touched, which is what
// keeps `weft bug list` usable when jobs.db is the thing that is broken.
//
// Only a confirmed mismatch refuses:
// an unresolvable executable path, or no weft on PATH, is unknown rather than
// mismatched, and refusing on unknown evidence would block legitimate work on
// a machine with no weft installed at all.
func checkDevBuildMayMigrate(path string) error {
	if os.Getenv(AllowDevMigrationEnv) != "" {
		return nil
	}
	if !samePath(path, sharedDBFile) {
		return nil
	}
	running, err := os.Executable()
	if err != nil {
		return nil
	}
	installed, err := exec.LookPath("weft")
	if err != nil {
		return nil
	}
	if samePath(running, installed) {
		return nil
	}
	return fmt.Errorf(
		"refusing to migrate the shared database from a build that is not the installed weft.\n"+
			"  running:   %s\n  installed: %s\n  database:  %s\n"+
			"Migrating would advance the schema past what the installed binary understands, "+
			"and every weft command on this machine would fail until it is rebuilt. "+
			"Install first (just install), or set %s=1 to migrate anyway.",
		running, installed, path, AllowDevMigrationEnv)
}

// samePath compares two filesystem paths through symlinks, falling back to a
// lexical comparison when a path cannot be resolved. Resolution can only
// collapse two names onto one real file, never separate one file into two, so a
// caller that refuses on a match never refuses on an unresolved path.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// PopulateSubmitterSessions fills in Job.SubmitterSession for the given jobs
// in one query. The column lives on the jobs table and is deliberately absent
// from jobSelectColumns: job rows are read through the job_status and
// launch_job_membership views, whose definitions are pinned by a schema guard
// and replayed by migrations that run before the column exists. Reading it
// separately keeps those views untouched.
//
// Jobs with no recorded session keep an empty value, which means the
// submission was not attributable to a session rather than that the lookup
// failed.
func PopulateSubmitterSessions(db *sql.DB, jobs []*Job) error {
	if len(jobs) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(jobs))
	ids := make([]any, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		ids = append(ids, job.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(`SELECT id, submitter_session FROM jobs WHERE id IN (`+sqlPlaceholders(len(ids))+`)`, ids...)
	if err != nil {
		return fmt.Errorf("read submitter sessions: %w", err)
	}
	defer rows.Close()
	sessions := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var session sql.NullString
		if err := rows.Scan(&id, &session); err != nil {
			return fmt.Errorf("scan submitter session: %w", err)
		}
		sessions[id] = session.String
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if session, ok := sessions[job.ID]; ok {
			job.SubmitterSession = session
		}
	}
	return nil
}
