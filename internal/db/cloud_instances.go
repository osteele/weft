package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/instanceintent"
)

// launchSelectColumns is the column list for SELECT queries on launches.
const launchSelectColumns = `id, campaign_id, host_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
		max_spend_cents, max_time_seconds, actual_spend_cents,
		created_at, ready_at, launched_at, ended_at,
		bootstrap_deadline_unix, agent_ready_at_unix,
		resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		inet_down_mbps, inet_up_mbps, cuda_version,
		cpu_cores_effective, cpu_name, ram_gb,
		provider_instance_id, data_center,
		instance_role, donor_instance_id, seed_download_secs, seed_copy_secs, replaced_instance_id,
		grace_period_seconds, grace_started_at, grace_deadline,
		termination_reason, termination_detail,
		disk_gb, provisioned_inputs,
		termination_requested_at, termination_intent_json,
		results_verified,
		machine_id, docker_image,
		provider_running_at,
		instance_type, max_bid_price_cents, on_demand_ref_cents,
		cordoned, cordon_reason, cordoned_at,
		hedge_cohort_id, first_onstart_probe_seen_unix`

// Launch status constants (same values used for both Launch and Campaign).
const (
	LaunchStatusPlanned   = "planned"
	LaunchStatusLaunching = "launching"
	LaunchStatusRunning   = "running"
	LaunchStatusPaused    = "paused"
	LaunchStatusGrace     = "grace"
	LaunchStatusCompleted = "completed"
	LaunchStatusFailed    = "failed"
	LaunchStatusCancelled = "canceled"
)

// Termination reason constants for Launch.TerminationReason.
const (
	TerminationReasonCompleted        = "completed"
	TerminationReasonProviderFailure  = "provider_failure"
	TerminationReasonJobFailure       = "job_failure"
	TerminationReasonDiskFull         = "disk_full"
	TerminationReasonInfraFailure     = "infra_failure"
	TerminationReasonBootstrapTimeout = "bootstrap_timeout"
	TerminationReasonCancelled        = "canceled"
	TerminationReasonPhaseStall       = "phase_stall"
	TerminationReasonPreempted        = "preempted"
	TerminationReasonUnknown          = "unknown"
	// TerminationReasonWeftBug labels failures attributable to a
	// weft-side defect rather than the machine, network, or provider.
	// Excluded from the survival model so a class of self-inflicted
	// failures doesn't poison machine-level priors.
	TerminationReasonWeftBug = "weft_bug"
	// TerminationReasonProviderTimeout labels failures where weft's CLI call
	// to the provider exceeded its per-command deadline (cloud.ErrProviderCommandTimeout).
	// These reflect transient provider-API slowness, not a real machine or
	// workload failure, so they are excluded from the clustered-failures
	// signal (see IsTransientInstanceTermination) while remaining visible
	// in the recent-failed-instances table for cost/postmortem accounting.
	TerminationReasonProviderTimeout = "provider_timeout"
	// TerminationReasonUploadStall labels self-destructs triggered by the
	// agent's upload-drain layer when R2 uploads stall repeatedly with no
	// progress, indicating that this instance has lost effective R2
	// connectivity even though SSH/heartbeat may still appear healthy.
	// Retryable: a fresh instance on a different network path usually works.
	TerminationReasonUploadStall = "upload_stall"
	// TerminationReasonAccountCreditExhausted labels failures attributable to
	// the operator's provider account running out of credit — either a
	// CreateInstance call rejected with an explicit "account lacks credit"
	// error, or a running instance destroyed mid-flight when the provider
	// stopped billing. Excluded from the survival model because these
	// failures say nothing about the machine, SKU, or provider region.
	//
	// The runtime-destruction case has no programmatic signal in the
	// provider response (Vast.ai just reports the instance as destroyed),
	// so the only path to this reason today is the operator running
	// `weft instance mark-credit-exhausted` after the fact. The
	// CreateInstance-time path still records `provider_failure` plus a
	// substring-matched detail; wiring that path through this constant
	// requires the call-site survey described in docs/planning/ROADMAP.md
	// § "Structured termination reasons for credit exhaustion".
	TerminationReasonAccountCreditExhausted = "account_credit_exhausted"
)

// IsRetryableTermination reports whether a failed cloud instance should be
// automatically relaunched. Returns true for infrastructure-level failures
// (provider failure, infra failure, failed to launch) where retrying on a different
// instance is likely to succeed. Returns false for job-level failures, disk
// full, user cancellations, and completed instances.
func IsRetryableTermination(ci *Launch) bool {
	if ci == nil || ci.Status != LaunchStatusFailed {
		return false
	}
	switch ci.TerminationReason {
	case TerminationReasonProviderFailure, TerminationReasonInfraFailure, TerminationReasonBootstrapTimeout, TerminationReasonPhaseStall, TerminationReasonPreempted, TerminationReasonProviderTimeout, TerminationReasonUploadStall, TerminationReasonAccountCreditExhausted, TerminationReasonUnknown, "":
		return true
	default:
		return false
	}
}

// IsInfrastructureTermination reports whether a failed cloud instance ended
// for an infrastructure-side reason that should NOT count against the
// runaway breaker's orphan-churn or no-progress-chain tallies. Tighter than
// IsRetryableTermination: Unknown and empty reasons are excluded so the
// breaker still trips on unexplained failures.
func IsInfrastructureTermination(reason string) bool {
	switch reason {
	case TerminationReasonProviderFailure,
		TerminationReasonInfraFailure,
		TerminationReasonBootstrapTimeout,
		TerminationReasonPhaseStall,
		TerminationReasonPreempted,
		TerminationReasonProviderTimeout,
		TerminationReasonUploadStall,
		TerminationReasonAccountCreditExhausted:
		return true
	default:
		return false
	}
}

// InfrastructureTerminationReasons returns the termination_reason values that
// IsInfrastructureTermination treats as infrastructure-side, suitable for
// constructing SQL `NOT IN (...)` filters with sqlPlaceholders.
func InfrastructureTerminationReasons() []string {
	return []string{
		TerminationReasonProviderFailure,
		TerminationReasonInfraFailure,
		TerminationReasonBootstrapTimeout,
		TerminationReasonPhaseStall,
		TerminationReasonPreempted,
		TerminationReasonProviderTimeout,
		TerminationReasonUploadStall,
		TerminationReasonAccountCreditExhausted,
	}
}

// IsTransientInstanceTermination reports whether a termination reason reflects
// a transient provider-side condition (provider CLI/API timeouts) rather than
// a real machine, network, or workload failure. Such terminations should not
// contribute to the clustered-failures common-factor signal — a slow vast.ai
// API hour does not constitute a "provider outage" worth flagging.
func IsTransientInstanceTermination(reason string) bool {
	return reason == TerminationReasonProviderTimeout
}

// IsNormalInstanceTermination reports whether a termination reason represents a
// normal instance lifecycle outcome rather than an operational failure.
func IsNormalInstanceTermination(reason string) bool {
	switch reason {
	case TerminationReasonCompleted, TerminationReasonJobFailure, TerminationReasonCancelled:
		return true
	default:
		return false
	}
}

// Launch represents a single cloud GPU deployment (e.g. one Vast.ai instance).
type Launch struct {
	ID                 int64
	CampaignID         *int64
	Status             string
	Provider           string
	GPUSpec            string
	GPUClass           string
	GPUMemGB           int
	VastaiInstanceID   string // legacy; use ProviderInstanceID for new code
	ProviderInstanceID string // provider-neutral instance ID
	MaxSpendCents      int
	MaxTimeSeconds     int
	ActualSpendCents   int
	CreatedAt          int64
	ReadyAt            *int64 // cloud instance ready (before SSH setup)
	LaunchedAt         *int64
	EndedAt            *int64
	// BootstrapDeadlineUnix is the absolute deadline by which the agent
	// should have come up. Set by LaunchInstance from the configured
	// bootstrap-terminate timeout; backfilled for pre-#4 rows by
	// migration. The reconciler reads it directly to decide whether
	// bootstrap has stalled.
	BootstrapDeadlineUnix *int64
	// AgentReadyAtUnix is set by the live-state sync the first time the
	// agent's bootstrap_stage transitions to "ready" (or, equivalently,
	// the first job starts running). It stays set across restarts and
	// gives the move-to-new "claim after agent ready" path a concrete
	// per-launch signal to consult.
	AgentReadyAtUnix *int64
	// FirstOnStartProbeSeenUnix is set the first time sync observes the
	// instance's OnStart probe key in R2. It is the closest signal we
	// have to "the container actually executed and had network", and
	// the input data to a future per-provider survival fit that will
	// replace the fixed dudVastTimeout. See migration 00009 and the
	// "Open" note in specs/campaign-lifecycle.allium § DudVastDetection.
	FirstOnStartProbeSeenUnix *int64
	DataCenter                string // data center / geolocation of the instance
	InstanceRole              string // "worker" or "donor"
	DonorInstanceID           *int64 // DB ID of the donor instance that seeded this worker (data locality)
	SeedDownloadSecs          *int   // on donor: total download duration (HF + uv)
	SeedCopySecs              *int   // on worker: copy-from-donor duration
	ReplacedInstanceID        *int64 // DB ID of the failed instance this one replaces (relaunch chain)
	// HedgeCohortID, when non-nil, is the launch ID of the primary in a
	// hedged-launch cohort. All members of a cohort (primary + probes)
	// share the same value, which by convention is the primary's own
	// launch_id. Probes have no claimed jobs at launch time. Once any
	// cohort member reaches agent_ready, the others are culled. See
	// campaign-lifecycle.allium § HedgeCohortCull.
	HedgeCohortID *int64

	// Grace period (failure-tolerant rental sessions)
	GracePeriodSeconds int    // configured grace period duration (0 = disabled)
	GraceStartedAt     *int64 // when the grace period started
	GraceDeadline      *int64 // when the grace period expires

	// Termination classification
	TerminationReason      string // "completed", "provider_failure", "job_failure", "disk_full", "infra_failure", "preempted", "canceled"
	TerminationDetail      string // human-readable detail for the termination reason
	TerminationRequestedAt *int64
	TerminationIntent      *instanceintent.Marker

	// Result verification (set by reconciler from R2 completion manifest)
	// nil = not checked (legacy marker), true = all uploads OK, false = uploads partial/failed
	ResultsVerified *bool

	// Instance capacity (for reuse matching)
	DiskGB            int      // Container disk allocation requested at create (max of createOpts.DiskGB and group estimate); not the host machine's total disk.
	ProvisionedInputs []string // Input refs provisioned at launch (e.g., "hf:meta-llama/Llama-3-8B")

	// Offer metadata (captured at launch)
	ResolvedGPUName  string
	CostPerHourCents int
	NumGPUs          int
	DLPerf           float64
	Reliability      float64
	InetDownMbps     float64
	InetUpMbps       float64
	CUDAVersion      float64
	CPUCores         int
	CPUName          string
	RAMGB            int

	// Provider machine identifier (for reliability tracking)
	MachineID string

	// DockerImage is the Docker image used for this instance (e.g. "nvidia/cuda:12.4.1-runtime-ubuntu22.04").
	DockerImage string

	// ProviderRunningAt is when the cloud provider first reported the instance
	// as "running" (Docker container started, onstart can execute). Bootstrap
	// timeout is measured from this timestamp, not LaunchedAt.
	ProviderRunningAt *int64

	// Rental type and pricing metadata (for preemptible / interruptible
	// instance analysis).
	InstanceType     string // "on-demand" or "interruptible"; empty for pre-migration rows
	MaxBidPriceCents *int   // Bid ceiling paid on interruptible offers; nil for on-demand
	OnDemandRefCents *int   // Cheapest concurrent on-demand $/hr at launch, for the same GPU class; nil if not captured

	// Cordon: when true, autopilot/reuse skips this instance for new job
	// placement. The instance keeps running and its current job continues;
	// only new routing is suppressed. CordonReason is an optional human-
	// readable note. CordonedAt is the unix timestamp the cordon was set.
	Cordoned     bool
	CordonReason string
	CordonedAt   *int64
}

// BootstrapOrigin returns the best timestamp to measure bootstrap elapsed time
// from: ProviderRunningAt if known (when the container started), otherwise
// LaunchedAt (when the instance was created).
func (c *Launch) BootstrapOrigin() *int64 {
	if c.ProviderRunningAt != nil {
		return c.ProviderRunningAt
	}
	return c.LaunchedAt
}

// CordonDetail returns the human-readable detail string shown after the
// "Cordoned:" label in status/watch output. Empty when not cordoned.
func (c *Launch) CordonDetail() string {
	if c == nil || !c.Cordoned {
		return ""
	}
	prefix := "yes"
	if c.CordonReason != "" {
		prefix = c.CordonReason
	}
	return prefix + " (no new jobs will be routed here)"
}

// DisplayGPUSpec returns a human-readable GPU spec string, falling back to
// GPUClass if GPUSpec is empty, and appending the resolved GPU name if known.
func (c *Launch) DisplayGPUSpec() string {
	spec := c.GPUSpec
	if spec == "" {
		spec = c.GPUClass
	}
	if c.ResolvedGPUName != "" {
		return fmt.Sprintf("%s (%s)", c.ResolvedGPUName, spec)
	}
	return spec
}

// DisplayGPUBrief returns a compact, instance-hardware-focused GPU label for
// headings and table columns (for example: "RTX 4090 24GB", "2x A100 80GB").
func (c *Launch) DisplayGPUBrief() string {
	model := c.displayGPUModelName()
	if model == "" {
		return ""
	}
	parts := []string{model}
	if c.GPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("%dGB", c.GPUMemGB))
	}
	label := strings.Join(parts, " ")
	if c.NumGPUs > 1 {
		return fmt.Sprintf("%dx %s", c.NumGPUs, label)
	}
	return label
}

// DisplayGPUDetails returns a detailed GPU line for instance status blocks
// (for example: "RTX 4090, 24GB per GPU, CUDA 12.4").
func (c *Launch) DisplayGPUDetails() string {
	model := c.displayGPUModelName()
	if model == "" {
		return ""
	}

	details := []string{model}
	if c.GPUMemGB > 0 {
		details = append(details, fmt.Sprintf("%dGB per GPU", c.GPUMemGB))
	}
	if c.NumGPUs > 1 {
		details = append(details, fmt.Sprintf("%dx GPUs", c.NumGPUs))
	}
	if c.CUDAVersion > 0 {
		details = append(details, "CUDA "+strconv.FormatFloat(c.CUDAVersion, 'f', -1, 64))
	}
	return strings.Join(details, ", ")
}

func (c *Launch) displayGPUModelName() string {
	if c == nil {
		return ""
	}
	if c.ResolvedGPUName != "" {
		return c.ResolvedGPUName
	}
	// Legacy rows may encode "Model (constraint text)" in GPUSpec.
	if idx := strings.Index(c.GPUSpec, " ("); idx > 0 {
		return strings.TrimSpace(c.GPUSpec[:idx])
	}
	if c.GPUSpec != "" {
		return c.GPUSpec
	}
	return c.GPUClass
}

// GraceStatusLabel returns a human-readable label for the grace period status.
// Returns empty string if the instance is not in grace status.
func (c *Launch) GraceStatusLabel() string {
	if c.Status != LaunchStatusGrace {
		return ""
	}
	if c.GraceDeadline != nil {
		remaining := time.Until(time.Unix(*c.GraceDeadline, 0)).Truncate(time.Second)
		if remaining < 0 {
			return "grace period — expired"
		}
		return fmt.Sprintf("grace period — %s remaining", remaining)
	}
	return "grace period"
}

// IsTerminal reports whether the instance is in a terminal status.
func (c *Launch) IsTerminal() bool {
	return IsTerminalLaunchStatus(c.Status)
}

// terminalLaunchStatuses is the single source of truth for terminal launch
// statuses. Use IsTerminalLaunchStatus for predicate checks and
// notTerminalLaunchClause for SQL guards.
var terminalLaunchStatuses = []string{
	LaunchStatusCompleted,
	LaunchStatusFailed,
	LaunchStatusCancelled,
}

// IsTerminalLaunchStatus reports whether status is a terminal launch status
// (completed, failed, canceled).
func IsTerminalLaunchStatus(status string) bool {
	for _, s := range terminalLaunchStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// notTerminalLaunchClause returns a SQL fragment `status NOT IN (...)` and its
// bind args, built from terminalLaunchStatuses.
func notTerminalLaunchClause() (string, []any) {
	return "status NOT IN (" + sqlPlaceholders(len(terminalLaunchStatuses)) + ")",
		anySliceArgs(terminalLaunchStatuses)
}

// ErrLaunchTerminal reports that a non-terminal status write was skipped
// because the launch is already in a terminal status. Terminal statuses are
// sticky: late writers (e.g. a launch flow whose final "running" write lands
// after the user terminated the instance) must not revive the row.
var ErrLaunchTerminal = errors.New("launch already in terminal status")

// errIfTerminalSkipped classifies a guarded non-terminal status UPDATE that
// matched zero rows. If the row exists and is terminal, the write was
// (correctly) skipped and ErrLaunchTerminal is returned so callers can notice
// the launch already ended. A missing row remains a silent no-op, matching
// the previous unguarded behavior.
func errIfTerminalSkipped(db *sql.DB, id int64, res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var status string
	err = db.QueryRow(`SELECT status FROM launches WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if IsTerminalLaunchStatus(status) {
		return ErrLaunchTerminal
	}
	return nil
}

// liveLaunchStatuses are launch statuses that still hold (or may still hold) a
// provider instance — not terminal, not destroyed. Used by reconciler queries
// and aggregations that should include in-flight rentals.
var liveLaunchStatuses = []string{
	LaunchStatusRunning,
	LaunchStatusLaunching,
	LaunchStatusPaused,
	LaunchStatusGrace,
}

// IsLiveLaunchStatus reports whether a launch is in one of liveLaunchStatuses.
func IsLiveLaunchStatus(status string) bool {
	for _, s := range liveLaunchStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// isActiveLaunchStatus reports whether a launch status represents an
// actively-progressing instance that should claim its jobs. Unlike
// liveLaunchStatuses this also includes Completed, since a completed launch
// still owns its jobs until they are reconciled.
func isActiveLaunchStatus(status string) bool {
	switch status {
	case LaunchStatusLaunching, LaunchStatusRunning, LaunchStatusPaused, LaunchStatusGrace, LaunchStatusCompleted:
		return true
	default:
		return false
	}
}

func anySliceArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, n*3-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, '?')
	}
	return string(out)
}

// HasActiveTerminationIntent reports whether the instance has a
// TerminationIntent in flight (state open or destroying). Delegates to
// the marker's IsActive predicate, which encapsulates the lifecycle
// state (and the back-compat fallback for markers written before the
// explicit State field existed). See specs/job-move.allium
// "TerminationIntent" — the model is mirrored on Launch via this
// JSON-encoded marker.
func (c *Launch) HasActiveTerminationIntent() bool {
	if c == nil {
		return false
	}
	return c.TerminationIntent.IsActive()
}

// EffectiveProviderID returns ProviderInstanceID, falling back to VastaiInstanceID for legacy records.
func (c *Launch) EffectiveProviderID() string {
	if c.ProviderInstanceID != "" {
		return c.ProviderInstanceID
	}
	return c.VastaiInstanceID
}

// DisplayTerminationReason returns a human-readable termination description.
// Prefers TerminationDetail (specific context), falls back to a humanized
// version of TerminationReason, then to Status.
func (c *Launch) DisplayTerminationReason() string {
	if c.TerminationDetail != "" {
		return c.TerminationDetail
	}
	if c.TerminationReason != "" {
		return HumanizeTerminationReason(c.TerminationReason)
	}
	return c.Status
}

// HumanizeTerminationReason converts a snake_case termination reason enum
// to a human-readable string.
func HumanizeTerminationReason(reason string) string {
	switch reason {
	case TerminationReasonCompleted:
		return "completed"
	case TerminationReasonProviderFailure:
		return "provider-side failure"
	case TerminationReasonJobFailure:
		return "job failure"
	case TerminationReasonDiskFull:
		return "disk full"
	case TerminationReasonInfraFailure:
		return "infrastructure failure"
	case TerminationReasonBootstrapTimeout:
		return "bootstrap timeout"
	case TerminationReasonPhaseStall:
		return "setup phase stalled"
	case TerminationReasonUploadStall:
		return "R2 uploads stalled"
	case TerminationReasonAccountCreditExhausted:
		return "account credit exhausted"
	case TerminationReasonPreempted:
		return "preempted (interruptible lost bid)"
	case TerminationReasonCancelled:
		return "cancelled by user"
	case TerminationReasonUnknown:
		return "unknown failure"
	default:
		return reason
	}
}

// CreateLaunch inserts a new cloud instance record and returns its ID.
func CreateLaunch(db *sql.DB, c *Launch) (int64, error) {
	now := time.Now().Unix()

	var provisionedInputsJSON *string
	if len(c.ProvisionedInputs) > 0 {
		data, _ := json.Marshal(c.ProvisionedInputs)
		s := string(data)
		provisionedInputsJSON = &s
	}

	return RetryOnDatabaseLockedValue(context.Background(), "create launch row", func() (int64, error) {
		var instanceType any
		if c.InstanceType != "" {
			instanceType = c.InstanceType
		}
		var maxBidPriceCents any
		if c.MaxBidPriceCents != nil {
			maxBidPriceCents = *c.MaxBidPriceCents
		}
		var onDemandRefCents any
		if c.OnDemandRefCents != nil {
			onDemandRefCents = *c.OnDemandRefCents
		}
		// Include grace_started_at / grace_deadline in the initial INSERT
		// when set on the struct, so callers that construct a Launch in
		// grace status (typically tests) satisfy the GraceRequiresDeadline
		// invariant enforced by triggers in 00007. When status is grace and
		// neither was set explicitly, default to (now, now + 5min) so the
		// common pattern (test setup) doesn't have to spell them out. The
		// trigger still catches UPDATE leaks on the transition path.
		if c.Status == LaunchStatusGrace {
			if c.GraceStartedAt == nil {
				v := now
				c.GraceStartedAt = &v
			}
			if c.GraceDeadline == nil {
				v := now + 300
				c.GraceDeadline = &v
			}
		}
		result, err := db.Exec(
			`INSERT INTO launches (campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
			 max_spend_cents, max_time_seconds, created_at,
			 resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
			 inet_down_mbps, inet_up_mbps, cuda_version,
			 cpu_cores_effective, cpu_name, ram_gb,
			 disk_gb, provisioned_inputs, machine_id, docker_image,
			 instance_type, max_bid_price_cents, on_demand_ref_cents,
			 grace_started_at, grace_deadline)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.CampaignID, c.Status, c.Provider, c.GPUSpec, c.GPUClass, c.GPUMemGB,
			c.MaxSpendCents, c.MaxTimeSeconds, now,
			c.ResolvedGPUName, c.CostPerHourCents, c.NumGPUs, c.DLPerf, c.Reliability,
			c.InetDownMbps, c.InetUpMbps, c.CUDAVersion,
			c.CPUCores, c.CPUName, c.RAMGB,
			c.DiskGB, provisionedInputsJSON, c.MachineID, c.DockerImage,
			instanceType, maxBidPriceCents, onDemandRefCents,
			c.GraceStartedAt, c.GraceDeadline,
		)
		if err != nil {
			return 0, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := EnsureRentalExecutionTarget(db, id); err != nil {
			return 0, err
		}
		return id, nil
	})
}

// GetLaunch retrieves a cloud instance by ID.
func GetLaunch(db *sql.DB, id int64) (*Launch, error) {
	row := db.QueryRow(
		`SELECT `+launchSelectColumns+` FROM launches WHERE id = ?`, id,
	)
	c, err := scanLaunchFrom(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetLaunchesByIDs returns the launches matching the given IDs as a map
// keyed by launch ID. Non-positive and duplicate IDs are skipped; missing
// rows are omitted from the result. Mirrors the input-handling style of
// GetLaunchLiveStates / GetLaunchStatuses so all three can be called with
// the same launchIDs slice.
func GetLaunchesByIDs(database *sql.DB, ids []int64) (map[int64]*Launch, error) {
	out := make(map[int64]*Launch, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(ids))
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
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
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		out[c.ID] = c
	}
	return out, rows.Err()
}

// GetLaunchByProviderID retrieves a launch by its provider instance ID.
// Returns nil, nil if no matching launch is found.
func GetLaunchByProviderID(database *sql.DB, providerID string) (*Launch, error) {
	row := database.QueryRow(
		`SELECT `+launchSelectColumns+` FROM launches WHERE provider_instance_id = ?`, providerID,
	)
	c, err := scanLaunchFrom(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// ListRecentAbnormalFailedInstances returns failed instances whose ended_at is
// at or after sinceUnix, excluding normal lifecycle outcomes such as user job
// failures. Used by the grouped jobs list TUI to surface recent operational
// failures.
func ListRecentAbnormalFailedInstances(database *sql.DB, sinceUnix int64) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+`
		   FROM launches
		  WHERE status = ?
		    AND ended_at IS NOT NULL
		    AND ended_at >= ?
		    AND (
		      termination_reason IS NULL
		      OR termination_reason = ''
		      OR termination_reason NOT IN (?, ?, ?)
		    )
		  ORDER BY ended_at DESC`,
		LaunchStatusFailed, sinceUnix,
		TerminationReasonCompleted,
		TerminationReasonJobFailure,
		TerminationReasonCancelled,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRunpodLaunchesMissingProviderMetadata returns RunPod launches whose
// provider pod ID is known but machine/datacenter metadata has not been stored.
func ListRunpodLaunchesMissingProviderMetadata(database *sql.DB) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+`
		   FROM launches
		  WHERE provider = ?
		    AND provider_instance_id != ''
		    AND (
		      COALESCE(machine_id, '') = ''
		      OR COALESCE(data_center, '') = ''
		    )
		  ORDER BY created_at DESC`,
		string(cloud.ProviderRunpod),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LaunchSuccessorsRecovered returns the subset of the given failed-launch IDs
// that have a successor launch (replaced_instance_id = id) currently in a
// non-failed live state or already completed. Used to dim recovered failures
// in the grouped jobs list TUI.
func LaunchSuccessorsRecovered(database *sql.DB, failedIDs []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(failedIDs))
	if len(failedIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(failedIDs))
	args := make([]any, 0, len(failedIDs)+4)
	for _, id := range failedIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	idCount := len(args)
	args = append(args,
		LaunchStatusRunning, LaunchStatusCompleted,
		LaunchStatusPaused, LaunchStatusGrace,
	)
	rows, err := database.Query(
		`SELECT DISTINCT replaced_instance_id
		   FROM launches
		  WHERE replaced_instance_id IN (`+sqlPlaceholders(idCount)+`)
		    AND status IN (?, ?, ?, ?)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullInt64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id.Valid {
			out[id.Int64] = true
		}
	}
	return out, rows.Err()
}

// LaunchChainTerminalStatuses walks replaced_instance_id successor links forward
// from each seed launch and returns the status of the chain's terminal (newest)
// launch, keyed by seed launch ID. A seed with no successor maps to its own
// status. Successors always have a higher id than the launch they replace, so
// the walk is acyclic and terminates.
func LaunchChainTerminalStatuses(database *sql.DB, seedIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(seedIDs))
	if len(seedIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(seedIDs))
	args := make([]any, 0, len(seedIDs))
	for _, id := range seedIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := database.Query(
		`WITH RECURSIVE chain(seed, id, status) AS (
		   SELECT id, id, status FROM launches WHERE id IN (`+sqlPlaceholders(len(args))+`)
		   UNION ALL
		   SELECT chain.seed, l.id, l.status
		     FROM launches l JOIN chain ON l.replaced_instance_id = chain.id
		 )
		 SELECT seed, id, status FROM chain`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terminalID := make(map[int64]int64, len(args))
	for rows.Next() {
		var (
			seed   int64
			id     int64
			status string
		)
		if err := rows.Scan(&seed, &id, &status); err != nil {
			return nil, err
		}
		// The terminal launch of a chain is the newest (highest id).
		if id >= terminalID[seed] {
			terminalID[seed] = id
			out[seed] = status
		}
	}
	return out, rows.Err()
}

// ProjectsByLaunchIDs returns one project name per launch (the most recent
// non-empty project across the launch's job_attempts). Empty values and
// missing launches are omitted.
func ProjectsByLaunchIDs(database *sql.DB, launchIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(launchIDs))
	if len(launchIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(launchIDs))
	args := make([]any, 0, len(launchIDs))
	for _, id := range launchIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := database.Query(
		`SELECT ja.launch_id, COALESCE(j.project, '')
		   FROM job_attempts ja
		   JOIN jobs j ON j.id = ja.job_id
		  WHERE ja.launch_id IN (`+sqlPlaceholders(len(args))+`)
		  ORDER BY ja.launch_id, ja.id DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			launchID int64
			project  string
		)
		if err := rows.Scan(&launchID, &project); err != nil {
			return nil, err
		}
		project = strings.TrimSpace(project)
		if project == "" {
			continue
		}
		if _, ok := out[launchID]; ok {
			continue
		}
		out[launchID] = project
	}
	return out, rows.Err()
}

// LaunchJobOutcome summarizes the current state of the job that ran on a
// launch. Status is the job's effective status (from the job_status view, which
// derives it from the latest attempt), used to classify a failed instance's
// series outcome.
type LaunchJobOutcome struct {
	JobID  int64
	Status string
	Host   string
}

// JobOutcomesByLaunchIDs returns the most recent job per launch (highest
// job_attempts id), with that job's current effective status and host. Launches
// with no job_attempts row are omitted — callers treat their absence as a
// "dud": the instance failed before any job ran on it. Status comes from the
// job_status view because the jobs table has no status column; status is
// derived from the latest attempt.
func JobOutcomesByLaunchIDs(database *sql.DB, launchIDs []int64) (map[int64]LaunchJobOutcome, error) {
	out := make(map[int64]LaunchJobOutcome, len(launchIDs))
	if len(launchIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]struct{}, len(launchIDs))
	args := make([]any, 0, len(launchIDs))
	for _, id := range launchIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := database.Query(
		`SELECT ja.launch_id, j.id, j.status, COALESCE(j.host, '')
		   FROM job_attempts ja
		   JOIN job_status j ON j.id = ja.job_id
		  WHERE ja.launch_id IN (`+sqlPlaceholders(len(args))+`)
		  ORDER BY ja.launch_id, ja.id DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			launchID int64
			outcome  LaunchJobOutcome
		)
		if err := rows.Scan(&launchID, &outcome.JobID, &outcome.Status, &outcome.Host); err != nil {
			return nil, err
		}
		if _, ok := out[launchID]; ok {
			continue
		}
		out[launchID] = outcome
	}
	return out, rows.Err()
}

// ListNonTerminalLaunches returns launches whose status is not in the
// terminal set (completed/failed/canceled), ordered by creation time
// descending. Equivalent to filtering ListLaunches by !IsTerminal(), but
// keeps the per-tick read cost bounded as the launches table grows.
// ListStalePlannedLaunches returns launches stuck in 'planned' status
// with no provider instance ID and created at or before cutoff. Used by
// the planned-launch reaper; pushes the filter into SQL so a busy
// reconcile loop doesn't scan every live launch every interval.
func ListStalePlannedLaunches(database *sql.DB, cutoff int64) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches
		 WHERE status = ?
		   AND (provider_instance_id IS NULL OR provider_instance_id = '')
		   AND created_at <= ?
		 ORDER BY created_at ASC`,
		LaunchStatusPlanned, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Launch
	for rows.Next() {
		l, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func ListNonTerminalLaunches(database *sql.DB) ([]*Launch, error) {
	clause, terminalArgs := notTerminalLaunchClause()
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches WHERE `+clause+` ORDER BY created_at DESC`,
		terminalArgs...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// ListLaunches returns all cloud instances ordered by creation time descending.
func ListLaunches(db *sql.DB) ([]*Launch, error) {
	rows, err := db.Query(
		`SELECT ` + launchSelectColumns + ` FROM launches ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// UpdateLaunchStatus updates a cloud instance's status and optionally sets timestamps.
// For terminal statuses (completed, failed, canceled), an optional terminationReason
// classifies why the instance ended (e.g. "provider_failure", "infra_failure"), and an
// optional terminationDetail provides a human-readable explanation.
func UpdateLaunchStatus(db *sql.DB, id int64, status string, terminationInfo ...string) error {
	now := time.Now().Unix()
	reason := ""
	detail := ""
	if len(terminationInfo) > 0 {
		reason = terminationInfo[0]
	}
	if len(terminationInfo) > 1 {
		detail = terminationInfo[1]
	}
	err := RetryOnDatabaseLocked(context.Background(), "update launch status", func() error {
		switch status {
		case LaunchStatusRunning:
			// COALESCE preserves launched_at on resume from paused/grace, so
			// bootstrap-survival metrics anchor to the original launch.
			// Terminal statuses are sticky: a late "running" write (e.g.
			// LaunchInstance finishing after a user terminate) must not
			// revive a row that already ended.
			clause, terminalArgs := notTerminalLaunchClause()
			res, err := db.Exec(`UPDATE launches SET status = ?, launched_at = COALESCE(launched_at, ?) WHERE id = ? AND `+clause,
				append([]any{status, now, id}, terminalArgs...)...)
			if err != nil {
				return err
			}
			return errIfTerminalSkipped(db, id, res)
		case LaunchStatusCompleted, LaunchStatusFailed, LaunchStatusCancelled:
			// Don't overwrite an already-terminal instance that has a SPECIFIC
			// termination reason set (e.g., bootstrap_timeout → job_failure on
			// next pass). 'unknown' is the placeholder set by the auto-derive
			// trigger (00007) when a launch was inserted/transitioned to
			// terminal without a reason — treat it as overridable so callers
			// that DO know the specific reason can still record it.
			var currentStatus, currentReason string
			if err := db.QueryRow(`SELECT status, COALESCE(termination_reason, '') FROM launches WHERE id = ?`, id).Scan(&currentStatus, &currentReason); err == nil {
				if IsTerminalLaunchStatus(currentStatus) && currentReason != "" && currentReason != TerminationReasonUnknown {
					return nil
				}
			}
			if reason != "" && detail != "" {
				_, err := db.Exec(`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ?, termination_detail = ? WHERE id = ?`, status, now, reason, detail, id)
				return err
			}
			if reason != "" {
				_, err := db.Exec(`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`, status, now, reason, id)
				return err
			}
			// Caller didn't specify a reason. Mirror the trigger's
			// derivation (00007 launches_terminal_auto_derive_reason*) at the
			// call site so the row's reason matches its status the moment
			// the writer commits, not after a follow-up trigger fires.
			// 'unknown' is the documented sentinel for "writer didn't know"
			// and is treated as overridable by the skip-check above.
			derivedReason := TerminationReasonUnknown
			switch status {
			case LaunchStatusCancelled:
				derivedReason = TerminationReasonCancelled
			case LaunchStatusCompleted:
				derivedReason = TerminationReasonCompleted
			}
			_, err := db.Exec(`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`, status, now, derivedReason, id)
			return err
		default:
			// Terminal statuses are sticky against non-terminal writes
			// (launching, paused, grace, ...).
			clause, terminalArgs := notTerminalLaunchClause()
			res, err := db.Exec(`UPDATE launches SET status = ? WHERE id = ? AND `+clause,
				append([]any{status, id}, terminalArgs...)...)
			if err != nil {
				return err
			}
			return errIfTerminalSkipped(db, id, res)
		}
	})
	if err != nil {
		return err
	}
	return EnsureRentalExecutionTarget(db, id)
}

// UpdateLaunchDockerImage sets the Docker image used for a launch.
func UpdateLaunchDockerImage(db *sql.DB, id int64, image string) error {
	_, err := db.Exec(`UPDATE launches SET docker_image = ? WHERE id = ?`, image, id)
	return err
}

// UpdateLaunchOnDemandRefCents sets the cheapest-concurrent-on-demand
// counterfactual captured for an interruptible launch.
func UpdateLaunchOnDemandRefCents(db *sql.DB, id int64, cents int) error {
	_, err := db.Exec(`UPDATE launches SET on_demand_ref_cents = ? WHERE id = ?`, cents, id)
	return err
}

// UpdateLaunchResultsVerified sets the results_verified flag on a cloud instance.
func UpdateLaunchResultsVerified(db *sql.DB, id int64, verified bool) error {
	_, err := db.Exec(`UPDATE launches SET results_verified = ? WHERE id = ?`, verified, id)
	return err
}

// SetLaunchCordoned marks a launch as cordoned (excluded from autopilot reuse).
// reason is optional human-readable detail. Setting cordoned=false clears the
// reason and timestamp. Returns sql.ErrNoRows if id does not exist.
func SetLaunchCordoned(db *sql.DB, id int64, cordoned bool, reason string) error {
	var reasonArg, cordonedAtArg any
	flag := 0
	if cordoned {
		flag = 1
		if reason != "" {
			reasonArg = reason
		}
		cordonedAtArg = time.Now().Unix()
	}
	res, err := db.Exec(
		`UPDATE launches SET cordoned = ?, cordon_reason = ?, cordoned_at = ? WHERE id = ?`,
		flag, reasonArg, cordonedAtArg, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return EnsureRentalExecutionTarget(db, id)
}

// SetLaunchTerminationRequested stamps termination_requested_at for a
// user-initiated terminate (UserTerminatesInstance in
// specs/campaign-lifecycle.allium). It preserves an earlier request time and
// leaves termination_intent_json (managed by the reconciler) untouched.
func SetLaunchTerminationRequested(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE launches SET termination_requested_at = COALESCE(termination_requested_at, ?) WHERE id = ?`,
		time.Now().Unix(), id,
	)
	return err
}

func UpdateLaunchTerminationIntent(db *sql.DB, id int64, marker *instanceintent.Marker) error {
	if marker == nil {
		_, err := db.Exec(`UPDATE launches SET termination_requested_at = NULL, termination_intent_json = NULL WHERE id = ?`, id)
		return err
	}

	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	requestedAt := marker.RequestedAtUnix
	if requestedAt == 0 {
		requestedAt = time.Now().Unix()
	}
	_, err = db.Exec(`UPDATE launches SET termination_requested_at = ?, termination_intent_json = ? WHERE id = ?`,
		requestedAt, string(data), id)
	return err
}

// SetLaunchReadyAt records when the Vast.ai instance became ready.
func SetLaunchReadyAt(db *sql.DB, id int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(`UPDATE launches SET ready_at = ? WHERE id = ?`, now, id)
	return err
}

// SetLaunchBootstrapDeadline persists the absolute bootstrap deadline.
// Called once at launch time.
func SetLaunchBootstrapDeadline(db *sql.DB, id int64, deadline time.Time) error {
	_, err := db.Exec(`UPDATE launches SET bootstrap_deadline_unix = ? WHERE id = ?`, deadline.Unix(), id)
	return err
}

// SetLaunchAgentReadyAtIfUnset records the first time the agent reports
// ready. Subsequent calls are no-ops (the first transition wins, same
// pattern as SetLaunchProviderRunningAt).
func SetLaunchAgentReadyAtIfUnset(db *sql.DB, id int64, t time.Time) error {
	_, err := db.Exec(
		`UPDATE launches SET agent_ready_at_unix = ? WHERE id = ? AND agent_ready_at_unix IS NULL`,
		t.Unix(), id,
	)
	return err
}

// SetLaunchFirstOnStartProbeSeenIfUnset records the first OnStart-probe
// observation for this launch. Idempotent. See EXP-021.
func SetLaunchFirstOnStartProbeSeenIfUnset(db *sql.DB, id int64, t time.Time) error {
	_, err := db.Exec(
		`UPDATE launches SET first_onstart_probe_seen_unix = ? WHERE id = ? AND first_onstart_probe_seen_unix IS NULL`,
		t.Unix(), id,
	)
	return err
}

// IsAgentReady reports whether the agent on this launch has signaled
// readiness at least once. Used by the move-to-new speculative confirm
// path to decide when it is safe to claim the job onto this instance.
func (c *Launch) IsAgentReady() bool {
	return c != nil && c.AgentReadyAtUnix != nil
}

// InDudDetectionWindow reports whether this launch is in the window where
// the dud-provider watchdog applies: provider reports running, but the
// agent has not yet signaled ready. Used by both sync_state to decide
// which R2 fetches to issue and by instance_check rule 4d to gate firing.
func (c *Launch) InDudDetectionWindow() bool {
	return c != nil &&
		c.Status == LaunchStatusRunning &&
		c.LaunchedAt != nil && *c.LaunchedAt > 0 &&
		c.AgentReadyAtUnix == nil
}

// BootstrapDeadlineExceeded reports whether `now` is past the launch's
// bootstrap deadline. False if the deadline is unset (defensive, but
// after the backfill migration this should not happen for live rows).
func (c *Launch) BootstrapDeadlineExceeded(now time.Time) bool {
	if c == nil || c.BootstrapDeadlineUnix == nil {
		return false
	}
	return now.Unix() > *c.BootstrapDeadlineUnix
}

// SetLaunchProviderRunningAt records when the cloud provider first reported the
// instance as "running". Only writes if not already set (first transition wins).
func SetLaunchProviderRunningAt(db *sql.DB, id int64, t time.Time) error {
	_, err := db.Exec(
		`UPDATE launches SET provider_running_at = ? WHERE id = ? AND provider_running_at IS NULL`,
		t.Unix(), id)
	return err
}

// LatestInstanceRunningAt returns the most recent time any launch's instance
// reached the provider-running state, or 0 when none have. It marks fleet
// recovery: a successful launch since a cluster of failures.
func LatestInstanceRunningAt(db *sql.DB) (int64, error) {
	var ts sql.NullInt64
	err := db.QueryRow(`SELECT MAX(provider_running_at) FROM launches WHERE provider_running_at IS NOT NULL`).Scan(&ts)
	if err != nil {
		return 0, err
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// SetLaunchProviderID sets the provider-neutral instance ID for a cloud instance.
// Also stamps launched_at if it is still null, so that bootstrap-timeout rules
// have an anchor even when the launching goroutine is wedged in a provider-
// specific pre-running step (e.g. RunPod SSH-readiness polling) and the
// instance never reaches status=running.
func SetLaunchProviderID(db *sql.DB, id int64, providerID string) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE launches
		 SET provider_instance_id = ?,
		     launched_at = COALESCE(launched_at, ?)
		 WHERE id = ?`,
		providerID, now, id,
	)
	return err
}

// UpdateLaunchOfferMetadata refreshes the offer-derived fields on a cloud
// instance row before a create-instance retry uses a replacement offer.
// The provider column is also updated so cross-provider replacements
// (e.g. retry pivots from RunPod to Vast.ai when the original provider's
// pool dries up) end up with the correct downstream provider routing.
func UpdateLaunchOfferMetadata(database *sql.DB, id int64, offer cloud.Offer) error {
	_, err := database.Exec(
		`UPDATE launches
		 SET provider = ?,
		     resolved_gpu_name = ?, cost_per_hour_cents = ?, num_gpus = ?, dl_perf = ?, reliability = ?,
		     inet_down_mbps = ?, inet_up_mbps = ?, cuda_version = ?,
		     cpu_cores_effective = ?, cpu_name = ?, ram_gb = ?,
		     disk_gb = ?, gpu_mem_gb = ?
		 WHERE id = ?`,
		string(offer.Provider),
		offer.GPUName,
		int(offer.CostPerHour*100),
		offer.NumGPUs,
		offer.DLPerf,
		offer.Reliability,
		offer.DownloadBandwidth,
		offer.UploadBandwidth,
		offer.CUDAVersion,
		offer.CPUCores,
		offer.CPUName,
		offer.RAMGB,
		int(offer.DiskSpaceGB),
		int(math.Round(offer.GPUMemGB)),
		id,
	)
	return err
}

// UpdateLaunchInstanceMetadata refreshes provider readback fields that are not
// always present in the offer row.
func UpdateLaunchInstanceMetadata(database *sql.DB, id int64, inst *cloud.Instance) error {
	if inst == nil {
		return nil
	}
	_, err := database.Exec(
		`UPDATE launches
		 SET cpu_cores_effective = CASE WHEN ? > 0 THEN ? ELSE cpu_cores_effective END,
		     cpu_name = CASE WHEN ? != '' THEN ? ELSE cpu_name END,
		     ram_gb = CASE WHEN ? > 0 THEN ? ELSE ram_gb END,
		     disk_gb = CASE WHEN ? > 0 THEN CAST(? AS INTEGER) ELSE disk_gb END,
		     machine_id = CASE WHEN ? != '' THEN ? ELSE machine_id END,
		     data_center = CASE WHEN ? != '' THEN ? ELSE data_center END
		 WHERE id = ?`,
		inst.CPUCores, inst.CPUCores,
		inst.CPUName, inst.CPUName,
		inst.RAMGB, inst.RAMGB,
		inst.DiskGB, inst.DiskGB,
		inst.MachineID, inst.MachineID,
		inst.DataCenter, inst.DataCenter,
		id,
	)
	return err
}

// SetLaunchVastaiID sets the Vast.ai instance ID for a cloud instance.
// Deprecated: use SetLaunchProviderID instead.
func SetLaunchVastaiID(db *sql.DB, id int64, vastaiID string) error {
	return SetLaunchProviderID(db, id, vastaiID)
}

// SetLaunchDataCenter sets the data center/geolocation for a cloud instance.
func SetLaunchDataCenter(db *sql.DB, id int64, dc string) error {
	_, err := db.Exec(`UPDATE launches SET data_center = ? WHERE id = ?`, dc, id)
	return err
}

// SetLaunchActualSpend updates the actual spend in cents.
func SetLaunchActualSpend(db *sql.DB, id int64, cents int) error {
	_, err := db.Exec(`UPDATE launches SET actual_spend_cents = ? WHERE id = ?`, cents, id)
	return err
}

// RefineInstanceTerminationReason upgrades a generic "job_failure" termination
// reason to a more specific reason inferred from associated job failure reasons.
// Currently upgrades to "disk_full" when any associated job reports it.
func RefineInstanceTerminationReason(database *sql.DB, instanceID int64) error {
	if instanceID <= 0 {
		return nil
	}

	var current sql.NullString
	err := database.QueryRow(`SELECT termination_reason FROM launches WHERE id = ?`, instanceID).Scan(&current)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.Valid || current.String != TerminationReasonJobFailure {
		return nil
	}

	var hasDiskFull int
	err = database.QueryRow(
		`SELECT EXISTS(
			SELECT 1
			FROM job_attempts ja
			JOIN jobs j ON j.id = ja.job_id
			WHERE j.tombstoned = 0
			  AND ja.failure_reason = ?
			  AND ja.launch_id = ?
		)`,
		TerminationReasonDiskFull, instanceID,
	).Scan(&hasDiskFull)
	if err != nil {
		return err
	}
	if hasDiskFull == 0 {
		return nil
	}

	_, err = database.Exec(
		`UPDATE launches
		 SET termination_reason = ?
		 WHERE id = ? AND termination_reason = ?`,
		TerminationReasonDiskFull, instanceID, TerminationReasonJobFailure,
	)
	return err
}

// CreditSignal classifies the strength of evidence that a launch failed
// because of provider-account credit exhaustion, based on substrings in
// termination_detail.
type CreditSignal int

const (
	// CreditSignalNone — no credit-related substring found. The launch
	// failed for an unrelated reason (dud detection, bootstrap timeout,
	// upload stall, RunPod SSH-key error, etc.) and should not be
	// reclassified as credit-exhausted regardless of how recently it ended.
	CreditSignalNone CreditSignal = iota
	// CreditSignalStrong — detail explicitly names a credit-out condition
	// observed by the runtime classifier (CreateInstance returning an empty
	// response, or an explicit "insufficient balance / credit" error from
	// the provider). Always credit-related; safe to reclassify automatically.
	CreditSignalStrong
	// CreditSignalCluster — generic "provider dead with no completion or
	// intent marker" detail produced by the reconciler when a running
	// instance unexpectedly disappears from the provider. Could be
	// credit-related (rentals destroyed when funds went to zero) or could
	// be ordinary provider-side death. Reclassify only when clustered in
	// time with strong signals.
	CreditSignalCluster
)

// strongCreditDetailPhrases match termination_detail substrings produced
// by the runtime credit-out classifier in internal/vastai/client.go
// isAccountCreditError and the CreateInstance empty-response branch.
//
// Drift warning: keep in sync with isAccountCreditError, with the LIKE
// patterns in internal/bidding/build.go LoadInstanceOutcomes, and with
// the corresponding lines in the campaign-lifecycle spec.
var strongCreditDetailPhrases = []string{
	"provider returned empty response",
	"insufficient balance",
	"insufficient credit",
	"account lacks credit",
	"account credit",
}

// clusterCreditDetailPhrases match termination_detail substrings produced
// when the reconciler observes a running instance disappear. These
// signatures are credit-related only when clustered with strong signals.
var clusterCreditDetailPhrases = []string{
	"provider dead with no completion or intent marker",
}

// ClassifyCreditSignal returns the credit-exhaustion signal strength for
// a launch's termination_detail. Matching is case-insensitive substring
// against the canonical phrase lists.
func ClassifyCreditSignal(detail string) CreditSignal {
	lower := strings.ToLower(detail)
	for _, p := range strongCreditDetailPhrases {
		if strings.Contains(lower, p) {
			return CreditSignalStrong
		}
	}
	for _, p := range clusterCreditDetailPhrases {
		if strings.Contains(lower, p) {
			return CreditSignalCluster
		}
	}
	return CreditSignalNone
}

// IsReclassifyEligibleReason reports whether a launch's current
// termination_reason can be overwritten by ReclassifyLaunchTerminationReason.
// The eligible set is the generic infrastructure/unknown reasons; specific
// reasons (disk_full, job_failure, completed, weft_bug, etc.) are protected
// from manual overwrite so the operator can't accidentally lose more
// specific information.
func IsReclassifyEligibleReason(reason string) bool {
	switch reason {
	case "",
		TerminationReasonProviderFailure,
		TerminationReasonInfraFailure,
		TerminationReasonUnknown:
		return true
	default:
		return false
	}
}

// ListReclassifyEligibleLaunches returns failed/canceled launches whose
// ended_at is at or after sinceUnix and whose current termination_reason is
// eligible for reclassification (see IsReclassifyEligibleReason). Returned
// in reverse-chronological order by ended_at so the CLI preview shows the
// most recent first.
func ListReclassifyEligibleLaunches(database *sql.DB, sinceUnix int64) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+`
		   FROM launches
		  WHERE status IN (?, ?)
		    AND ended_at IS NOT NULL
		    AND ended_at >= ?
		    AND (
		      termination_reason IS NULL
		      OR termination_reason = ''
		      OR termination_reason IN (?, ?, ?)
		    )
		  ORDER BY ended_at DESC, id DESC`,
		LaunchStatusFailed, LaunchStatusCancelled,
		sinceUnix,
		TerminationReasonProviderFailure,
		TerminationReasonInfraFailure,
		TerminationReasonUnknown,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReclassifyLaunchTerminationReason overwrites termination_reason (and
// optionally prepends to termination_detail) on an already-terminal launch.
// Unlike UpdateLaunchStatus, this is allowed to overwrite a non-unknown
// reason, but only when the current reason is in the eligible set (see
// IsReclassifyEligibleReason). It does not touch status, ended_at,
// attempt_outcome, or any other column.
//
// Used by `weft instance mark-credit-exhausted` to retroactively label
// instances destroyed by the provider for non-payment, after the operator
// confirms the incident — there is no programmatic signal that distinguishes
// these from generic provider failures.
//
// Returns sql.ErrNoRows if the launch does not exist. Returns a typed error
// if the launch is not terminal, or if the current termination_reason is
// not eligible for reclassification.
func ReclassifyLaunchTerminationReason(database *sql.DB, launchID int64, newReason, detailPrefix string) error {
	var currentStatus, currentReason, currentDetail string
	err := database.QueryRow(
		`SELECT status, COALESCE(termination_reason, ''), COALESCE(termination_detail, '') FROM launches WHERE id = ?`,
		launchID,
	).Scan(&currentStatus, &currentReason, &currentDetail)
	if err != nil {
		return err
	}
	if currentStatus != LaunchStatusFailed && currentStatus != LaunchStatusCancelled {
		return fmt.Errorf("launch %d has status %q; reclassify is only allowed on failed/canceled launches", launchID, currentStatus)
	}
	if !IsReclassifyEligibleReason(currentReason) {
		return fmt.Errorf("launch %d has termination_reason %q; reclassify is only allowed when the current reason is generic (provider_failure, infra_failure, unknown, or empty)", launchID, currentReason)
	}
	newDetail := currentDetail
	if detailPrefix != "" {
		if currentDetail != "" {
			newDetail = detailPrefix + "; " + currentDetail
		} else {
			newDetail = detailPrefix
		}
	}
	_, err = database.Exec(
		`UPDATE launches SET termination_reason = ?, termination_detail = ? WHERE id = ?`,
		newReason, newDetail, launchID,
	)
	return err
}

// SetLaunchRole sets the instance role ("worker" or "donor").
func SetLaunchRole(db *sql.DB, id int64, role string) error {
	_, err := db.Exec(`UPDATE launches SET instance_role = ? WHERE id = ?`, role, id)
	return err
}

// SetLaunchDonorID sets the DB ID of the donor instance that seeded this worker.
func SetLaunchDonorID(db *sql.DB, id int64, donorID int64) error {
	_, err := db.Exec(`UPDATE launches SET donor_instance_id = ? WHERE id = ?`, donorID, id)
	return err
}

// SetLaunchReplacedID records the DB ID of the failed instance this one replaces (relaunch chain).
func SetLaunchReplacedID(db *sql.DB, id int64, replacedID int64) error {
	_, err := db.Exec(`UPDATE launches SET replaced_instance_id = ? WHERE id = ?`, replacedID, id)
	return err
}

// SetLaunchSeedDownloadSecs records the total download duration on a donor instance.
func SetLaunchSeedDownloadSecs(db *sql.DB, id int64, secs int) error {
	_, err := db.Exec(`UPDATE launches SET seed_download_secs = ? WHERE id = ?`, secs, id)
	return err
}

// SetLaunchSeedCopySecs records the copy-from-donor duration on a worker instance.
func SetLaunchSeedCopySecs(db *sql.DB, id int64, secs int) error {
	_, err := db.Exec(`UPDATE launches SET seed_copy_secs = ? WHERE id = ?`, secs, id)
	return err
}

// LaunchHost returns the synthetic host name for a cloud instance.
// LaunchHost returns the legacy synthetic host name used by older
// records. New code should prefer LaunchID and TargetKind helpers.
func LaunchHost(instanceID int64) string {
	return fmt.Sprintf("vastai:%d", instanceID)
}

// IsLaunchHost reports whether a host string refers to a legacy synthetic
// rental host name (e.g. "vastai:123" or "runpod:456").
func IsLaunchHost(host string) bool {
	_, ok := parseLegacyLaunchHostInstanceID(host)
	return ok
}

func parseLegacyLaunchHostInstanceID(host string) (int64, bool) {
	trimmed := strings.TrimSpace(host)
	for _, prefix := range []string{"vastai:", "runpod:", "rental:"} {
		suffix, ok := strings.CutPrefix(trimmed, prefix)
		if !ok {
			continue
		}
		id, err := ids.ParseInstanceID(suffix)
		if err == nil && id > 0 {
			return id, true
		}
	}
	return 0, false
}

// SetJobLaunchID associates a job with a cloud instance by creating a new
// attempt with launch_id set. The job must have an open queued attempt
// (integrity violation otherwise). The open attempt is closed and a fresh
// attempt is created with the instance assignment. Rejects with
// ErrJobAlreadyClaimed if another active launch already owns the job; use
// TransferJobLaunchID for a deliberate move that supersedes the prior owner.
func SetJobLaunchID(database *sql.DB, jobID, instanceID int64) error {
	return ClaimJobForLaunch(database, jobID, instanceID)
}

// TransferJobLaunchID is like SetJobLaunchID but skips the
// ErrJobAlreadyClaimed guard, allowing a move to supersede an active prior
// owner. Intended only for deliberate moves under an open MoveIntent (see
// specs/job-move.allium); the source attempt is closed and a fresh attempt
// is created on the new instance, just like SetJobLaunchID.
func TransferJobLaunchID(database *sql.DB, jobID, instanceID int64) error {
	return TransferJobToLaunch(database, jobID, instanceID)
}

func setJobLaunchIDOnce(database *sql.DB, jobID, instanceID int64, allowSupersede bool) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}

	if !allowSupersede {
		// Reject if the job is already claimed by an active launch.
		var currentLaunchID sql.NullInt64
		_ = tx.QueryRow(`
			SELECT launch_id FROM job_attempts
			WHERE job_id = ? AND end_time IS NULL
			ORDER BY attempt_number DESC LIMIT 1`, jobID,
		).Scan(&currentLaunchID)
		if currentLaunchID.Valid {
			var launchStatus string
			err = tx.QueryRow(`SELECT status FROM launches WHERE id = ?`, currentLaunchID.Int64).Scan(&launchStatus)
			if err == nil && isActiveLaunchStatus(launchStatus) {
				tx.Rollback()
				return fmt.Errorf("job %d: %w", jobID, ErrJobAlreadyClaimed)
			}
		}
	}

	// Capture the latest pre-existing attempt id (if any) so the new
	// attempt can record it as its predecessor — letting runtime-estimation
	// queries follow the relaunch chain across instances.
	var predecessorID sql.NullInt64
	_ = tx.QueryRow(
		`SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&predecessorID)

	// Close any existing open attempt and create the placement attempt.
	now := time.Now().Unix()
	if err := closeOpenAttempts(tx, jobID, now); err != nil {
		tx.Rollback()
		return err
	}
	newAttemptID, err := createAttemptTx(tx, jobID, "", &instanceID, StatusQueued)
	if err != nil {
		tx.Rollback()
		return err
	}
	if predecessorID.Valid {
		if _, err := tx.Exec(
			`UPDATE job_attempts SET predecessor_attempt_id = ? WHERE id = ?`,
			predecessorID.Int64, newAttemptID,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("link predecessor attempt %d -> %d: %w", predecessorID.Int64, newAttemptID, err)
		}
	}
	// Clear requested_status since the job is now placed.
	if _, err := tx.Exec(`UPDATE jobs SET requested_status = NULL WHERE id = ?`, jobID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err := tx.Exec(`UPDATE jobs SET placement_reasons = NULL WHERE id = ?`, jobID); err != nil {
		tx.Rollback()
		return err
	}
	// Mark any previous cloud attempts as superseded
	if _, err := tx.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?
		 WHERE job_id = ? AND launch_id IS NOT NULL AND end_time IS NOT NULL AND cloud_outcome IS NULL`,
		AttemptOutcomeSuperseded, jobID,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("mark superseded attempts: %w", err)
	}
	return tx.Commit()
}

// SetJobCampaignIndex sets the 0-based position of a job within its campaign sequence.
func SetJobCampaignIndex(db *sql.DB, jobID int64, index int) error {
	return RetryOnDatabaseLocked(context.Background(), "set job campaign index", func() error {
		_, err := db.Exec(`UPDATE jobs SET campaign_job_index = ? WHERE id = ?`, index, jobID)
		return err
	})
}

// GetLaunchJobs returns all jobs associated with a cloud instance.
func GetLaunchJobs(db *sql.DB, instanceID int64) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE launch_id = ? AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, instanceID)
}

// GetLaunchJobsIncludingAttempts returns jobs currently associated with a
// cloud instance, jobs with an open cloud attempt on that instance, or jobs
// that only have historical cloud attempt records for it. This is useful for
// display purposes: after ResetLaunchJobs clears cloud_instance_id on
// re-queued jobs, those jobs are still visible via the job_cloud_attempts
// table.
func GetLaunchJobsIncludingAttempts(database *sql.DB, instanceID int64) ([]*Job, error) {
	// Jobs with a campaign_job_index (the launched jobs) sort by that index;
	// historical-only jobs (no index) follow. Within each group, sort by id.
	// membership_rank is a final tiebreaker so the current row wins over the
	// historical row for the same job when deduplicating below.
	query := fmt.Sprintf(`SELECT %s FROM launch_job_membership
		WHERE membership_launch_id = ?
		  AND tombstoned = 0
		ORDER BY
			CASE WHEN campaign_job_index IS NOT NULL THEN 0 ELSE 1 END ASC,
			campaign_job_index ASC,
			id ASC,
			membership_rank ASC`, qualifiedJobSelectColumns("launch_job_membership"))
	all, err := queryJobs(database, query, instanceID)
	if err != nil {
		return nil, err
	}
	// Deduplicate: a job can appear as both current and historical membership.
	// Keep the first occurrence (current, rank=0) since we sorted membership_rank ASC.
	seen := make(map[int64]struct{}, len(all))
	result := all[:0]
	for _, job := range all {
		if _, ok := seen[job.ID]; ok {
			continue
		}
		seen[job.ID] = struct{}{}
		result = append(result, job)
	}

	attemptFacts, err := GetLaunchAttemptFacts(database, instanceID)
	if err != nil {
		return nil, fmt.Errorf("get launch attempt facts: %w", err)
	}

	// Patch rows with the timing and exit facts from the attempt that actually
	// ran on this instance. Historical rows otherwise inherit fields from the
	// latest global attempt, which can be a later retry on another instance.
	for _, job := range result {
		fact, ok := attemptFacts[job.ID]
		if !ok {
			continue
		}
		job.StartTime = fact.StartTime
		job.EndTime = fact.EndTime
		job.ExitCode = fact.ExitCode
		attemptID := fact.AttemptID
		job.LatestRunID = &attemptID
		if job.Host == "" && fact.Host != "" {
			job.Host = fact.Host
		}
	}

	// For historical jobs (no longer assigned to this instance), override the
	// display status with the attempt outcome so the UI shows what happened on
	// this instance rather than the job's current global status.
	var historical []*Job
	for _, job := range result {
		if job.LaunchID == nil || *job.LaunchID != instanceID {
			historical = append(historical, job)
		}
	}
	if len(historical) > 0 {
		outcomes, err := GetAttemptOutcomesByLaunch(database, instanceID)
		if err != nil {
			return nil, fmt.Errorf("get attempt outcomes: %w", err)
		}
		for _, job := range historical {
			outcome, ok := outcomes[job.ID]
			if !ok {
				continue
			}
			switch outcome {
			case AttemptOutcomeFailed:
				job.Status = StatusFailed
			case AttemptOutcomeOrphaned:
				// Leave status as-is (queued); AttemptDisplayStatus() uses
				// the attempt outcome for instance-scoped display.
			case AttemptOutcomeCompleted:
				job.Status = StatusCompleted
			case AttemptOutcomeCancelled:
				job.Status = StatusCanceled
			default:
				if fact, ok := attemptFacts[job.ID]; ok {
					job.Status = fact.Status
				}
				// AttemptOutcomeSuperseded: keep current status (moved to another instance)
			}
		}
	}

	return result, nil
}

// ListUnplacedJobs returns jobs that are truly unplaced: no inventory host, no
// current cloud-instance assignment, and no open cloud attempt on a non-terminal
// instance.
func ListUnplacedJobs(db *sql.DB) ([]*Job, error) {
	// Include both queued and pending_placement statuses: the latter is a
	// transient state the orchestration layer uses to mark jobs being
	// processed by a relaunch pass. Excluding it hides the very jobs the
	// relaunch was invoked for — see internal/orchestration/pending_placement.go.
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		  AND effective_target_kind = ?
		  AND status IN (?, ?)
		  AND host = ''
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, string(JobTargetUnplaced), StatusQueued, StatusPendingPlacement)
}

// CountJobsWaitingOnInstances returns the number of queued jobs that are
// assigned to non-terminal rental instances (i.e. the instance is still
// setting up or running but the job hasn't started yet).
func CountJobsWaitingOnInstances(database *sql.DB) (int, error) {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*) FROM job_status
		WHERE tombstoned = 0
		  AND status = ?
		  AND effective_target_kind = ?`,
		StatusQueued, string(JobTargetRentalInstance),
	).Scan(&count)
	return count, err
}

// AssignJobHost atomically assigns a host to an unplaced queued job.
// Returns true if the job was updated (false if it was already claimed or changed status).
// Uses COALESCE(pending_status, status) so that a failed job with
// pending_status='queued' (retry intent) is eligible for placement.
func AssignJobHost(database *sql.DB, jobID int64, host string) (bool, error) {
	cordoned, _, err := IsInventoryExecutionTargetCordoned(database, host)
	if err != nil {
		return false, err
	}
	if cordoned {
		return false, nil
	}

	// Check effective status and current host from job_status view
	var effectiveStatus, currentHost string
	if err := database.QueryRow(
		`SELECT COALESCE(pending_status, status), host FROM job_status WHERE id = ? AND tombstoned = 0`, jobID,
	).Scan(&effectiveStatus, &currentHost); err != nil {
		return false, err
	}
	if effectiveStatus != StatusQueued || currentHost != "" {
		return false, nil
	}

	// Check if an attempt exists. If not, create one (unplaced jobs have no
	// attempt until placement).
	attemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		return false, err
	}
	targetID, err := ensureExecutionTargetForAttempt(database, host, nil)
	if err != nil {
		return false, err
	}
	if attemptID == 0 {
		if _, err := CreateAttempt(database, jobID, host, nil, StatusQueued); err != nil {
			return false, err
		}
	} else {
		// Update host on the existing attempt (may be closed if pending retry)
		result, err := database.Exec(
			`UPDATE job_attempts
			    SET host = ?,
			        launch_id = NULL,
			        target_id = ?
			 WHERE id = `+latestAttemptSubquery+` AND host = ''`,
			host, targetID, jobID)
		if err != nil {
			return false, err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return false, nil
		}
	}
	if _, err := database.Exec(`UPDATE jobs SET placement_reasons = NULL WHERE id = ?`, jobID); err != nil {
		return false, err
	}
	return true, nil
}

// ResetLaunchJobs resets non-terminal jobs in a cloud instance back to
// unplaced (queued with empty host) and clears their instance association.
// Records the attempt outcome before resetting. Returns the number of jobs reset.
func ResetLaunchJobs(database *sql.DB, instanceID int64, outcome string) (int64, error) {
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	ci, err := scanLaunchFrom(tx.QueryRow(`SELECT `+launchSelectColumns+` FROM launches WHERE id = ?`, instanceID))
	if err != nil && err != sql.ErrNoRows {
		tx.Rollback()
		return 0, err
	}
	placementReasons := encodeStringSlice(launchResetPlacementReasons(ci, outcome))

	// Filter by both the derived view status AND the raw requested_status:
	// a job with requested_status='canceled' must never be requeued even if
	// the view hasn't yet surfaced 'canceled' (e.g., because the latest
	// attempt is still open). See CancelSurvivesInstanceTermination in
	// specs/job-lifecycle.allium.
	jobs, err := queryJobsTx(tx,
		fmt.Sprintf(`SELECT %s FROM job_status js WHERE js.launch_id = ? AND js.status NOT IN (?, ?, ?, ?, ?, ?) AND js.tombstoned = 0 AND COALESCE((SELECT requested_status FROM jobs WHERE jobs.id = js.id), '') NOT IN (?, ?, ?) ORDER BY js.id ASC`, qualifiedJobSelectColumns("js")),
		instanceID,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
		StatusCanceled, StatusKilled, StatusDraft,
	)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if len(jobs) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, nil
	}

	jobIDs := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		jobIDs = append(jobIDs, job.ID)
	}

	now := time.Now().Unix()
	unplacedJobIDs := make([]int64, 0, len(jobIDs))
	restoredJobIDs := make([]int64, 0)
	for _, jobID := range jobIDs {
		transition, err := handleMoveTargetFailedBeforeStartTx(tx, jobID, instanceID, now, outcome)
		if err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("restore failed move target for job %d: %w", jobID, err)
		}
		if transition.Handled {
			restoredJobIDs = append(restoredJobIDs, jobID)
			continue
		}
		if err := closeAttemptsAndRequeueWithOutcome(tx, jobID, now, outcome); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("requeue job %d: %w", jobID, err)
		}
		// Clear start_time on orphaned attempts so the retry budget doesn't
		// charge infrastructure overhead (bootstrap/setup) against the job.
		if outcome == AttemptOutcomeOrphaned {
			if _, err := tx.Exec(
				`UPDATE job_attempts SET start_time = NULL WHERE job_id = ? AND end_time = ? AND cloud_outcome = ?`,
				jobID, now, AttemptOutcomeOrphaned); err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("clear start_time for orphaned job %d: %w", jobID, err)
			}
		}
		unplacedJobIDs = append(unplacedJobIDs, jobID)
	}

	var n int64
	if len(unplacedJobIDs) > 0 {
		placementArgs := make([]any, 0, len(unplacedJobIDs)+1)
		placementArgs = append(placementArgs, placementReasons)
		placementPlaceholders := make([]string, 0, len(unplacedJobIDs))
		for _, jobID := range unplacedJobIDs {
			placementArgs = append(placementArgs, jobID)
			placementPlaceholders = append(placementPlaceholders, "?")
		}
		result, err := tx.Exec(
			fmt.Sprintf(`UPDATE jobs SET placement_reasons = ? WHERE id IN (%s)`, strings.Join(placementPlaceholders, ", ")),
			placementArgs...,
		)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		n, err = result.RowsAffected()
		if err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	for _, jobID := range restoredJobIDs {
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice([]string{fmt.Sprintf("move target instance %d failed before start; job restored to source queue", instanceID)}),
			jobID,
		); err != nil {
			tx.Rollback()
			return 0, err
		}
		n++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func launchResetPlacementReasons(ci *Launch, outcome string) []string {
	if ci == nil {
		return []string{"returned from cloud instance to unplaced queue"}
	}

	switch ci.Status {
	case LaunchStatusFailed:
		if ci.TerminationReason != "" {
			return []string{fmt.Sprintf("cloud instance %d failed (%s)", ci.ID, ci.TerminationReason)}
		}
		return []string{fmt.Sprintf("cloud instance %d failed", ci.ID)}
	case LaunchStatusCancelled:
		return []string{fmt.Sprintf("cloud instance %d was canceled", ci.ID)}
	default:
		if outcome != "" {
			return []string{fmt.Sprintf("cloud instance %d returned job to queue (%s)", ci.ID, outcome)}
		}
		return []string{fmt.Sprintf("cloud instance %d returned job to queue", ci.ID)}
	}
}

// ResetOrphanedCloudJobs resets non-terminal jobs whose host references a cloud
// instance (host LIKE 'vastai:%') that is either terminal or missing from the DB
// entirely. This catches jobs stranded by stale host fields when cloud_instance_id
// was already cleared — including "running" jobs whose instance died after the
// agent started execution but before the reset could clear them.
func ResetOrphanedCloudJobs(database *sql.DB) (int64, error) {
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(`
		SELECT id, host FROM job_status js
		WHERE status IN (?, ?) AND host LIKE 'vastai:%' AND tombstoned = 0
		AND NOT EXISTS (
			SELECT 1 FROM launches ci
			WHERE ci.id = CAST(SUBSTR(js.host, 8) AS INTEGER)
			AND ci.status IN (?, ?, ?, ?, ?)
		)`,
		StatusQueued, StatusRunning,
		LaunchStatusRunning, LaunchStatusLaunching, LaunchStatusPaused, LaunchStatusGrace, LaunchStatusCompleted,
	)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer rows.Close()

	type staleCloudJob struct {
		id   int64
		host string
	}
	var jobs []staleCloudJob
	for rows.Next() {
		var job staleCloudJob
		if err := rows.Scan(&job.id, &job.host); err != nil {
			tx.Rollback()
			return 0, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		tx.Rollback()
		return 0, err
	}

	now := time.Now().Unix()
	for _, job := range jobs {
		// Close old attempt and create fresh unplaced one
		if _, err := tx.Exec(`
			UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ?
			WHERE job_id = ? AND end_time IS NULL`,
			StatusCanceled, now, AttemptOutcomeOrphaned, job.id,
		); err != nil {
			tx.Rollback()
			return 0, err
		}
		if _, err := createAttemptTx(tx, job.id, "", nil, StatusQueued); err != nil {
			tx.Rollback()
			return 0, err
		}
		// Update spec columns on jobs
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice(orphanedCloudPlacementReasons(job.host)), job.id,
		); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(jobs)), nil
}

func orphanedCloudPlacementReasons(host string) []string {
	if instanceID := strings.TrimPrefix(host, "vastai:"); instanceID != "" && instanceID != host {
		return []string{fmt.Sprintf("cloud instance %s no longer active; job returned to unplaced queue", instanceID)}
	}
	return []string{"cloud instance no longer active; job returned to unplaced queue"}
}

// GetLaunchJobCounts returns a map from cloud instance ID to job count.
func GetLaunchJobCounts(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT launch_id, COUNT(*) FROM job_status WHERE launch_id IS NOT NULL AND tombstoned = 0 GROUP BY launch_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// GetActiveLaunchJobCounts returns a map from cloud instance ID to the
// number of non-terminal jobs still assigned to that instance.
func GetActiveLaunchJobCounts(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT launch_id, COUNT(*) FROM job_status
		WHERE launch_id IS NOT NULL
		  AND tombstoned = 0
		  AND effective_target_kind = ?
		  AND status NOT IN (?, ?, ?, ?, ?, ?)
		GROUP BY launch_id`,
		string(JobTargetRentalInstance),
		StatusCompleted, StatusDead, StatusFailed, StatusKilled, StatusCanceled, StatusDraft,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// GetCampaignInstances returns all cloud instances for a campaign.
func GetCampaignInstances(db *sql.DB, campaignID int64) ([]*Launch, error) {
	rows, err := db.Query(
		`SELECT `+launchSelectColumns+` FROM launches WHERE campaign_id = ? ORDER BY created_at ASC`, campaignID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// cloudInstanceScanner is implemented by both *sql.Row and *sql.Rows.
type cloudInstanceScanner interface {
	Scan(dest ...any) error
}

func scanLaunchFrom(s cloudInstanceScanner) (*Launch, error) {
	var c Launch
	var campaignID sql.NullInt64
	var hostID sql.NullInt64
	var gpuSpec, gpuClass sql.NullString
	var gpuMemGB, maxSpend, maxTime, actualSpend sql.NullInt64
	var readyAt, launchedAt, endedAt sql.NullInt64
	var bootstrapDeadline, agentReadyAt sql.NullInt64
	var resolvedGPUName sql.NullString
	var costPerHourCents, numGPUs sql.NullInt64
	var dlPerf, reliability, inetDown, inetUp, cudaVersion sql.NullFloat64
	var cpuCores, ramGB sql.NullInt64
	var cpuName sql.NullString
	var providerInstanceID, dataCenter sql.NullString
	var instanceRole sql.NullString
	var donorInstanceID sql.NullInt64
	var seedDownloadSecs, seedCopySecs sql.NullInt64
	var replacedInstanceID sql.NullInt64
	var gracePeriodSeconds sql.NullInt64
	var graceStartedAt, graceDeadline sql.NullInt64
	var terminationReason sql.NullString
	var terminationDetail sql.NullString
	var diskGB sql.NullInt64
	var provisionedInputsJSON sql.NullString
	var terminationRequestedAt sql.NullInt64
	var terminationIntentJSON sql.NullString
	var resultsVerified sql.NullBool
	var machineID sql.NullString
	var dockerImage sql.NullString
	var providerRunningAt sql.NullInt64
	var instanceType sql.NullString
	var maxBidPriceCents, onDemandRefCents sql.NullInt64
	var cordoned sql.NullInt64
	var cordonReason sql.NullString
	var cordonedAt sql.NullInt64
	var hedgeCohortID sql.NullInt64
	var firstOnStartProbeSeen sql.NullInt64

	err := s.Scan(
		&c.ID, &campaignID, &hostID, &c.Status, &c.Provider, &gpuSpec, &gpuClass, &gpuMemGB,
		&maxSpend, &maxTime, &actualSpend,
		&c.CreatedAt, &readyAt, &launchedAt, &endedAt,
		&bootstrapDeadline, &agentReadyAt,
		&resolvedGPUName, &costPerHourCents, &numGPUs, &dlPerf, &reliability,
		&inetDown, &inetUp, &cudaVersion,
		&cpuCores, &cpuName, &ramGB,
		&providerInstanceID, &dataCenter,
		&instanceRole, &donorInstanceID, &seedDownloadSecs, &seedCopySecs, &replacedInstanceID,
		&gracePeriodSeconds, &graceStartedAt, &graceDeadline,
		&terminationReason, &terminationDetail,
		&diskGB, &provisionedInputsJSON,
		&terminationRequestedAt, &terminationIntentJSON,
		&resultsVerified,
		&machineID, &dockerImage,
		&providerRunningAt,
		&instanceType, &maxBidPriceCents, &onDemandRefCents,
		&cordoned, &cordonReason, &cordonedAt,
		&hedgeCohortID, &firstOnStartProbeSeen,
	)
	_ = hostID // TODO: populate Launch.HostID when field is added
	if err != nil {
		return nil, err
	}

	if campaignID.Valid {
		c.CampaignID = &campaignID.Int64
	}
	if gpuSpec.Valid {
		c.GPUSpec = gpuSpec.String
	}
	if gpuClass.Valid {
		c.GPUClass = gpuClass.String
	}
	if gpuMemGB.Valid {
		c.GPUMemGB = int(gpuMemGB.Int64)
	}
	if maxSpend.Valid {
		c.MaxSpendCents = int(maxSpend.Int64)
	}
	if maxTime.Valid {
		c.MaxTimeSeconds = int(maxTime.Int64)
	}
	if actualSpend.Valid {
		c.ActualSpendCents = int(actualSpend.Int64)
	}
	if readyAt.Valid {
		c.ReadyAt = &readyAt.Int64
	}
	if launchedAt.Valid {
		c.LaunchedAt = &launchedAt.Int64
	}
	if endedAt.Valid {
		c.EndedAt = &endedAt.Int64
	}
	if bootstrapDeadline.Valid {
		c.BootstrapDeadlineUnix = &bootstrapDeadline.Int64
	}
	if agentReadyAt.Valid {
		c.AgentReadyAtUnix = &agentReadyAt.Int64
	}
	if resolvedGPUName.Valid {
		c.ResolvedGPUName = resolvedGPUName.String
	}
	if costPerHourCents.Valid {
		c.CostPerHourCents = int(costPerHourCents.Int64)
	}
	if numGPUs.Valid {
		c.NumGPUs = int(numGPUs.Int64)
	}
	if dlPerf.Valid {
		c.DLPerf = dlPerf.Float64
	}
	if reliability.Valid {
		c.Reliability = reliability.Float64
	}
	if inetDown.Valid {
		c.InetDownMbps = inetDown.Float64
	}
	if inetUp.Valid {
		c.InetUpMbps = inetUp.Float64
	}
	if cudaVersion.Valid {
		c.CUDAVersion = cudaVersion.Float64
	}
	if cpuCores.Valid {
		c.CPUCores = int(cpuCores.Int64)
	}
	if cpuName.Valid {
		c.CPUName = cpuName.String
	}
	if ramGB.Valid {
		c.RAMGB = int(ramGB.Int64)
	}
	if providerInstanceID.Valid {
		c.ProviderInstanceID = providerInstanceID.String
	}
	if dataCenter.Valid {
		c.DataCenter = dataCenter.String
	}
	if instanceRole.Valid {
		c.InstanceRole = instanceRole.String
	}
	if donorInstanceID.Valid {
		c.DonorInstanceID = &donorInstanceID.Int64
	}
	if seedDownloadSecs.Valid {
		v := int(seedDownloadSecs.Int64)
		c.SeedDownloadSecs = &v
	}
	if seedCopySecs.Valid {
		v := int(seedCopySecs.Int64)
		c.SeedCopySecs = &v
	}
	if replacedInstanceID.Valid {
		c.ReplacedInstanceID = &replacedInstanceID.Int64
	}
	if gracePeriodSeconds.Valid {
		c.GracePeriodSeconds = int(gracePeriodSeconds.Int64)
	}
	if graceStartedAt.Valid {
		c.GraceStartedAt = &graceStartedAt.Int64
	}
	if graceDeadline.Valid {
		c.GraceDeadline = &graceDeadline.Int64
	}
	if terminationReason.Valid {
		c.TerminationReason = terminationReason.String
	}
	if terminationDetail.Valid {
		c.TerminationDetail = terminationDetail.String
	}
	if terminationRequestedAt.Valid {
		c.TerminationRequestedAt = &terminationRequestedAt.Int64
	}
	if diskGB.Valid {
		c.DiskGB = int(diskGB.Int64)
	}
	if provisionedInputsJSON.Valid && provisionedInputsJSON.String != "" {
		_ = json.Unmarshal([]byte(provisionedInputsJSON.String), &c.ProvisionedInputs)
	}
	if terminationIntentJSON.Valid && terminationIntentJSON.String != "" {
		var marker instanceintent.Marker
		if json.Unmarshal([]byte(terminationIntentJSON.String), &marker) == nil {
			c.TerminationIntent = &marker
		}
	}
	if resultsVerified.Valid {
		v := resultsVerified.Bool
		c.ResultsVerified = &v
	}
	if machineID.Valid {
		c.MachineID = machineID.String
	}
	if dockerImage.Valid {
		c.DockerImage = dockerImage.String
	}
	if providerRunningAt.Valid {
		c.ProviderRunningAt = &providerRunningAt.Int64
	}
	if instanceType.Valid {
		c.InstanceType = instanceType.String
	}
	if maxBidPriceCents.Valid {
		v := int(maxBidPriceCents.Int64)
		c.MaxBidPriceCents = &v
	}
	if onDemandRefCents.Valid {
		v := int(onDemandRefCents.Int64)
		c.OnDemandRefCents = &v
	}
	if cordoned.Valid {
		c.Cordoned = cordoned.Int64 != 0
	}
	if cordonReason.Valid {
		c.CordonReason = cordonReason.String
	}
	if cordonedAt.Valid {
		c.CordonedAt = &cordonedAt.Int64
	}
	if hedgeCohortID.Valid {
		c.HedgeCohortID = &hedgeCohortID.Int64
	}
	if firstOnStartProbeSeen.Valid {
		c.FirstOnStartProbeSeenUnix = &firstOnStartProbeSeen.Int64
	}
	return &c, nil
}

// SetLaunchHedgeCohort sets the hedge_cohort_id of a launch. The
// primary uses its own id as the cohort id; probes share that value.
func SetLaunchHedgeCohort(database *sql.DB, launchID int64, cohortID int64) error {
	_, err := database.Exec(`UPDATE launches SET hedge_cohort_id = ? WHERE id = ?`, cohortID, launchID)
	return err
}

// HedgeCohortHasReadySibling returns true if any launch with the given
// cohort id (other than excludeLaunchID) has a non-NULL agent_ready_at_unix.
func HedgeCohortHasReadySibling(database *sql.DB, cohortID, excludeLaunchID int64) (bool, error) {
	var exists int
	err := database.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM launches WHERE hedge_cohort_id = ? AND id != ? AND agent_ready_at_unix IS NOT NULL)`,
		cohortID, excludeLaunchID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

// Job cloud attempt outcome constants.
const (
	AttemptOutcomeCompleted  = "completed"
	AttemptOutcomeFailed     = "failed"
	AttemptOutcomeCancelled  = "canceled"
	AttemptOutcomeOrphaned   = "orphaned"
	AttemptOutcomeSuperseded = "superseded"
	// AttemptOutcomePreempted marks an attempt that was running on an
	// interruptible cloud instance whose pause was not recovered before
	// stale_pause_timeout, triggering a relaunch on a fresh offer. The
	// successor attempt's predecessor_attempt_id points back to this one
	// so runtime-estimation queries can sum durations across the chain.
	AttemptOutcomePreempted = "preempted"
)

// LaunchAttempt records a single association between a job and a cloud instance.
type LaunchAttempt struct {
	ID        int64
	JobID     int64
	LaunchID  int64
	StartedAt int64
	EndedAt   *int64
	Outcome   string // "completed", "failed", "canceled", "orphaned"
}

// LaunchAttemptFact captures the latest attempt facts for a job on a specific
// cloud instance, even if the job has since been retried elsewhere.
type LaunchAttemptFact struct {
	AttemptID    int64
	Status       string
	StartTime    int64
	EndTime      *int64
	ExitCode     *int
	CloudOutcome string
	Host         string
}

// CloseLaunchAttempt sets cloud_outcome on the latest cloud-associated attempt for a job.
func CloseLaunchAttempt(database *sql.DB, jobID int64, outcome string) error {
	_, err := database.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?
		 WHERE id = (
			SELECT id FROM job_attempts
			WHERE job_id = ? AND launch_id IS NOT NULL
			ORDER BY attempt_number DESC LIMIT 1
		 )`,
		outcome, jobID,
	)
	return err
}

// CloseLaunchAttempts sets cloud_outcome on all open attempts associated with
// the given cloud instance. Completed launches are conservative: per-job R2
// completion sync is the only path that can mark an attempt successful, so any
// still-open attempts are returned to the queue as orphaned work.
func CloseLaunchAttempts(database *sql.DB, instanceID int64, outcome string) error {
	if outcome == AttemptOutcomeCompleted {
		if _, err := database.Exec(
			`UPDATE job_attempts
			    SET end_time = NULL
			  WHERE launch_id = ?
			    AND end_time = 0
			    AND status NOT IN (?, ?, ?, ?, ?, ?)`,
			instanceID,
			StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
		); err != nil {
			return err
		}
		_, err := ResetLaunchJobs(database, instanceID, AttemptOutcomeOrphaned)
		return err
	}

	// Map cloud outcome to attempt status: "failed" → failed, "completed" → completed,
	// everything else (orphaned, superseded) → canceled.
	attemptStatus := StatusCanceled
	switch outcome {
	case AttemptOutcomeFailed:
		attemptStatus = StatusFailed
	}
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, cloud_outcome = ?, end_time = COALESCE(NULLIF(end_time, 0), ?), pending_status = NULL
		 WHERE launch_id = ? AND (end_time IS NULL OR end_time = 0)`,
		attemptStatus, outcome, now, instanceID,
	)
	return err
}

// repairAttemptEndTimeZero fixes attempts where end_time was written as 0
// (meaning "unknown") instead of NULL. This caused CloseLaunchAttempts and
// ComputeJobState to treat the attempt as having a known end time of epoch 0,
// preventing instance cleanup.
func repairAttemptEndTimeZero(database *sql.DB) error {
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET end_time = COALESCE(NULLIF(start_time, 0), queued_at, strftime('%s','now'))
		 WHERE end_time = 0 AND status IN (?, ?, ?)`,
		StatusCompleted, StatusFailed, StatusCanceled,
	)
	return err
}

// repairCompletedCloudAttemptsMissingExitCode fixes synthetic completion rows
// created by CloseLaunchAttempts before it populated exit_code=0. Without this,
// the job_status view derives a terminal cloud attempt as "dead".
func repairCompletedCloudAttemptsMissingExitCode(database *sql.DB) error {
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET exit_code = COALESCE(exit_code, 0),
		     last_synced_status = ?,
		     pending_status = NULL
		 WHERE launch_id IS NOT NULL
		   AND status = ?
		   AND cloud_outcome = ?
		   AND end_time IS NOT NULL
		   AND (exit_code IS NULL OR last_synced_status IS NULL OR last_synced_status != ?)`,
		StatusCompleted,
		StatusCompleted,
		AttemptOutcomeCompleted,
		StatusCompleted,
	)
	return err
}

// repairCompletedCloudAttemptsWithoutEvidence repairs rows fabricated by older
// completed-launch cleanup. Those rows have no start_time because no per-job R2
// completion was ingested, but were nevertheless stamped as synced completed
// attempts with exit_code=0. Return them to the queue instead.
func repairCompletedCloudAttemptsWithoutEvidence(database *sql.DB) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	rows, err := tx.Query(
		`SELECT ja.id, ja.job_id, ja.launch_id
		   FROM job_attempts ja
		   JOIN jobs j ON j.id = ja.job_id
		  WHERE ja.id = (
				SELECT MAX(ja2.id) FROM job_attempts ja2 WHERE ja2.job_id = ja.job_id
		  )
		    AND ja.launch_id IS NOT NULL
		    AND ja.status = ?
		    AND ja.cloud_outcome = ?
		    AND ja.exit_code = 0
		    AND ja.start_time IS NULL
		    AND ja.last_synced_status = ?
		    AND COALESCE(j.requested_status, '') NOT IN (?, ?, ?)`,
		StatusCompleted,
		AttemptOutcomeCompleted,
		StatusCompleted,
		StatusCanceled,
		StatusKilled,
		StatusDraft,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer rows.Close()

	type poisonedAttempt struct {
		attemptID  int64
		jobID      int64
		instanceID int64
	}
	var attempts []poisonedAttempt
	for rows.Next() {
		var a poisonedAttempt
		if err := rows.Scan(&a.attemptID, &a.jobID, &a.instanceID); err != nil {
			tx.Rollback()
			return err
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		tx.Rollback()
		return err
	}
	if err := rows.Close(); err != nil {
		tx.Rollback()
		return err
	}

	now := time.Now().Unix()
	for _, a := range attempts {
		if _, err := tx.Exec(
			`UPDATE job_attempts
			    SET status = ?, end_time = COALESCE(NULLIF(end_time, 0), ?),
			        exit_code = NULL, last_synced_status = NULL,
			        cloud_outcome = ?, pending_status = NULL
			  WHERE id = ?`,
			StatusCanceled, now, AttemptOutcomeOrphaned, a.attemptID,
		); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := createAttemptTx(tx, a.jobID, "", nil, StatusQueued); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(
			`UPDATE jobs SET placement_reasons = ? WHERE id = ?`,
			encodeStringSlice([]string{fmt.Sprintf("cloud instance %d completed without job result; job returned to unplaced queue", a.instanceID)}),
			a.jobID,
		); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// CountLaunchAttempts returns the number of cloud-associated attempts for a job.
// Attempts where the job was orphaned before it started (instance failed during
// provisioning) are excluded — infrastructure failures should not count against
// the job's retry budget.
func CountLaunchAttempts(database *sql.DB, jobID int64) (int, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts
		 WHERE job_id = ?
		   AND launch_id IS NOT NULL
		   AND COALESCE(cloud_outcome, '') != ?
		   AND NOT (COALESCE(cloud_outcome, '') = ? AND start_time IS NULL)`,
		jobID, AttemptOutcomeSuperseded,
		AttemptOutcomeOrphaned,
	).Scan(&count)
	return count, err
}

// CountLaunchAttemptsInCampaign returns the number of cloud-associated attempts
// for a job within a specific campaign. This prevents attempts from earlier
// campaigns (e.g. a previous `launch` command) from counting against the retry
// budget of the current campaign.
// Attempts where the job was orphaned before it started are excluded.
func CountLaunchAttemptsInCampaign(database *sql.DB, jobID int64, campaignID int64) (int, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts ja
		 JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.job_id = ?
		   AND ja.launch_id IS NOT NULL
		   AND l.campaign_id = ?
		   AND COALESCE(ja.cloud_outcome, '') != ?
		   AND NOT (COALESCE(ja.cloud_outcome, '') = ? AND ja.start_time IS NULL)`,
		jobID, campaignID, AttemptOutcomeSuperseded,
		AttemptOutcomeOrphaned,
	).Scan(&count)
	return count, err
}

// GetLaunchAttempts returns the cloud attempt history for a job, ordered by start time.
func GetLaunchAttempts(database *sql.DB, jobID int64) ([]LaunchAttempt, error) {
	rows, err := database.Query(
		`SELECT id, job_id, launch_id, COALESCE(queued_at, start_time, 0), end_time, cloud_outcome
		 FROM job_attempts
		 WHERE job_id = ?
		   AND launch_id IS NOT NULL
		   AND COALESCE(cloud_outcome, '') != ?
		 ORDER BY attempt_number ASC`,
		jobID, AttemptOutcomeSuperseded,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []LaunchAttempt
	for rows.Next() {
		var a LaunchAttempt
		var endedAt sql.NullInt64
		var outcome sql.NullString
		if err := rows.Scan(&a.ID, &a.JobID, &a.LaunchID, &a.StartedAt, &endedAt, &outcome); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			a.EndedAt = &endedAt.Int64
		}
		if outcome.Valid {
			a.Outcome = outcome.String
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

// GetAttemptOutcomesByLaunch returns the cloud_outcome for each job attempt
// that ran on the given cloud instance. Returns map[jobID]outcome.
func GetAttemptOutcomesByLaunch(database *sql.DB, cloudInstanceID int64) (map[int64]string, error) {
	// Return the outcome of the LATEST attempt on this launch per job. A newer
	// open attempt on the same launch must hide an older closed attempt's
	// outcome — otherwise diagnose reports stale terminal states (e.g.
	// "superseded") for jobs that have an active attempt on the instance.
	rows, err := database.Query(
		`WITH ranked AS (
			SELECT job_id, cloud_outcome,
			       ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY attempt_number DESC, id DESC) AS rn
			FROM job_attempts
			WHERE launch_id = ?
		)
		SELECT job_id, cloud_outcome FROM ranked
		WHERE rn = 1 AND cloud_outcome IS NOT NULL`,
		cloudInstanceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	outcomes := make(map[int64]string)
	for rows.Next() {
		var jobID int64
		var outcome string
		if err := rows.Scan(&jobID, &outcome); err != nil {
			return nil, err
		}
		outcomes[jobID] = outcome
	}
	return outcomes, rows.Err()
}

// GetLaunchAttemptFacts returns the latest attempt facts for each job that ran
// on the given cloud instance.
func GetLaunchAttemptFacts(database *sql.DB, cloudInstanceID int64) (map[int64]LaunchAttemptFact, error) {
	rows, err := database.Query(
		`WITH ranked_attempts AS (
			SELECT
				ja.job_id,
				ja.id,
				ja.status,
				ja.host,
				ja.start_time,
				ja.end_time,
				ja.exit_code,
				COALESCE(ja.cloud_outcome, '') AS cloud_outcome,
				ROW_NUMBER() OVER (PARTITION BY ja.job_id ORDER BY ja.attempt_number DESC, ja.id DESC) AS rn
			FROM job_attempts ja
			WHERE ja.launch_id = ?
		)
		SELECT job_id, id, status, host, start_time, end_time, exit_code, cloud_outcome
		FROM ranked_attempts
		WHERE rn = 1`,
		cloudInstanceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	facts := make(map[int64]LaunchAttemptFact)
	for rows.Next() {
		var (
			jobID        int64
			fact         LaunchAttemptFact
			host         sql.NullString
			startTime    sql.NullInt64
			endTime      sql.NullInt64
			exitCode     sql.NullInt64
			cloudOutcome sql.NullString
		)
		if err := rows.Scan(&jobID, &fact.AttemptID, &fact.Status, &host, &startTime, &endTime, &exitCode, &cloudOutcome); err != nil {
			return nil, err
		}
		if host.Valid {
			fact.Host = host.String
		}
		if startTime.Valid {
			fact.StartTime = startTime.Int64
		}
		if endTime.Valid {
			end := endTime.Int64
			fact.EndTime = &end
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			fact.ExitCode = &code
		}
		if cloudOutcome.Valid {
			fact.CloudOutcome = cloudOutcome.String
		}
		facts[jobID] = fact
	}
	return facts, rows.Err()
}

// GetLatestAttemptOutcome returns the most recent cloud_outcome for a job.
// Returns empty string if no cloud attempts exist.
func GetLatestAttemptOutcome(database *sql.DB, jobID int64) string {
	var outcome sql.NullString
	err := database.QueryRow(
		`SELECT cloud_outcome FROM job_attempts
		 WHERE job_id = ? AND cloud_outcome IS NOT NULL
		 ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&outcome)
	if err != nil || !outcome.Valid {
		return ""
	}
	return outcome.String
}

// JobOrphanStreak describes a job whose most recent cloud attempt was
// orphaned and whose overall orphan count meets or exceeds a caller-supplied
// threshold. Used to surface stuck-in-launch-loop jobs that the per-campaign
// runaway breaker doesn't catch (orphans are excluded from the retry budget
// by design, but a long unbroken streak still signals trouble).
type JobOrphanStreak struct {
	JobID       int64
	OrphanCount int
	Description string
	Project     string
}

// JobsWithOrphanStreaks returns currently-active jobs (queued or running)
// whose tail of recent non-superseded cloud attempts is at least `threshold`
// orphans long. Tombstoned and terminal jobs are excluded so historical
// detritus doesn't pollute the warning list.
func JobsWithOrphanStreaks(database *sql.DB, threshold int) ([]JobOrphanStreak, error) {
	if threshold < 1 {
		threshold = 1
	}
	// SQLite-portable consecutive-tail count: rank attempts newest-first,
	// then for each job take the rn of the FIRST non-orphan (or one past the
	// last attempt if all are orphans). The streak length is that rn minus
	// one.
	rows, err := database.Query(
		`WITH ranked AS (
			SELECT job_id, cloud_outcome,
			       ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY attempt_number DESC, id DESC) AS rn
			FROM job_attempts
			WHERE COALESCE(cloud_outcome, '') != ?
		),
		first_non_orphan AS (
			SELECT job_id, MIN(rn) AS rn FROM ranked
			WHERE COALESCE(cloud_outcome, '') != ?
			GROUP BY job_id
		),
		max_rn AS (
			SELECT job_id, MAX(rn) + 1 AS rn FROM ranked GROUP BY job_id
		),
		streaks AS (
			SELECT m.job_id,
			       COALESCE(f.rn, m.rn) - 1 AS streak
			  FROM max_rn m
			  LEFT JOIN first_non_orphan f USING (job_id)
		)
		SELECT j.id,
		       COALESCE(j.description, ''),
		       COALESCE(j.project, ''),
		       s.streak
		  FROM jobs j
		  JOIN streaks s ON s.job_id = j.id
		  JOIN job_status js ON js.id = j.id
		 WHERE COALESCE(j.tombstoned, 0) = 0
		   AND js.status IN (?, ?)
		   AND s.streak >= ?
		 ORDER BY s.streak DESC, j.id ASC`,
		AttemptOutcomeSuperseded,
		AttemptOutcomeOrphaned,
		StatusQueued, StatusRunning,
		threshold,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobOrphanStreak
	for rows.Next() {
		var s JobOrphanStreak
		if err := rows.Scan(&s.JobID, &s.Description, &s.Project, &s.OrphanCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetLaunchGracePeriod stores the configured grace period for a cloud instance.
func SetLaunchGracePeriod(db *sql.DB, id int64, seconds int) error {
	_, err := db.Exec(`UPDATE launches SET grace_period_seconds = ? WHERE id = ?`, seconds, id)
	return err
}

// SetLaunchGraceStarted transitions an instance to grace status with a deadline.
// Terminal statuses are sticky: if the instance already ended (e.g. the user
// terminated it), the transition is skipped and ErrLaunchTerminal is returned.
func SetLaunchGraceStarted(db *sql.DB, id int64, deadline int64) error {
	now := time.Now().Unix()
	clause, terminalArgs := notTerminalLaunchClause()
	res, err := db.Exec(
		`UPDATE launches SET status = ?, grace_started_at = ?, grace_deadline = ? WHERE id = ? AND `+clause,
		append([]any{LaunchStatusGrace, now, deadline, id}, terminalArgs...)...,
	)
	if err != nil {
		return err
	}
	return errIfTerminalSkipped(db, id, res)
}

// ExtendLaunchGrace updates the grace deadline for an instance.
func ExtendLaunchGrace(db *sql.DB, id int64, newDeadline int64) error {
	_, err := db.Exec(`UPDATE launches SET grace_deadline = ? WHERE id = ?`, newDeadline, id)
	return err
}

// ClearLaunchGrace transitions an instance from grace back to running,
// clearing the grace fields. This is used when the agent picks up
// resubmitted jobs during grace period.
func ClearLaunchGrace(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE launches SET status = ?, grace_started_at = NULL, grace_deadline = NULL WHERE id = ? AND status = ?`,
		LaunchStatusRunning, id, LaunchStatusGrace,
	)
	return err
}

// ListRecentlyTerminalLaunches returns cloud instances that reached a terminal status
// (failed, completed, canceled) within the last `since` duration and have a provider ID.
// Used as a safety net to destroy leaked provider instances.
func ListRecentlyTerminalLaunches(database *sql.DB, since time.Duration) ([]*Launch, error) {
	cutoff := time.Now().Add(-since).Unix()
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches
		 WHERE status IN (?, ?, ?)
		 AND ended_at >= ?
		 AND provider_instance_id != ''
		 ORDER BY ended_at DESC`,
		LaunchStatusFailed, LaunchStatusCompleted, LaunchStatusCancelled,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// NormalizeTerminalLaunchJobs repairs unresolved jobs on a terminal cloud
// instance. Failed instances orphan active jobs; canceled instances cancel them.
// Completed instances are left unchanged so result sync can still finalize them.
func NormalizeTerminalLaunchJobs(database *sql.DB, instanceID int64) (int64, error) {
	ci, err := GetLaunch(database, instanceID)
	if err != nil {
		return 0, err
	}
	if ci == nil {
		return 0, nil
	}

	reset, outcome := terminalLaunchJobDisposition(ci)
	if outcome == "" {
		return 0, nil
	}
	if reset {
		return ResetLaunchJobs(database, instanceID, outcome)
	}
	return 0, CloseLaunchAttempts(database, instanceID, outcome)
}

func terminalLaunchJobDisposition(ci *Launch) (reset bool, outcome string) {
	if ci == nil {
		return false, ""
	}

	switch ci.Status {
	case LaunchStatusFailed:
		if IsRetryableTermination(ci) {
			if ci.TerminationReason == TerminationReasonPreempted {
				return true, AttemptOutcomePreempted
			}
			return true, AttemptOutcomeOrphaned
		}
		switch ci.TerminationReason {
		case TerminationReasonCancelled:
			return false, AttemptOutcomeCancelled
		case TerminationReasonCompleted:
			return false, AttemptOutcomeCompleted
		default:
			return false, AttemptOutcomeFailed
		}
	case LaunchStatusCancelled:
		return true, AttemptOutcomeCancelled
	default:
		return false, ""
	}
}

// ResetJobsOnTerminalLaunches finds non-terminal jobs associated with
// failed or canceled cloud instances and resets them to queued/unplaced.
// Completed instances are intentionally skipped until result sync finalizes them.
// This is a DB-only repair pass that doesn't require cloud provider clients.
// Returns a map of jobID → instanceID for each reset job, so callers can
// scope relaunch to only the actually-orphaned jobs and track which instance
// each job came from.
func ResetJobsOnTerminalLaunches(database *sql.DB) (map[int64]int64, error) {
	// Find non-terminal jobs on failed/cancelled instances.
	rows, err := database.Query(`
		SELECT js.id, ci.id, ci.status, COALESCE(ci.termination_reason, '')
		FROM launches ci
		JOIN job_status js ON js.launch_id = ci.id
		WHERE ci.status IN (?, ?)
		AND js.status NOT IN (?, ?, ?, ?, ?, ?)
		AND js.tombstoned = 0`,
		LaunchStatusFailed, LaunchStatusCancelled,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collect job→instance mapping before resetting.
	resetMap := make(map[int64]int64)
	instanceSet := make(map[int64]bool)
	for rows.Next() {
		var jobID, instanceID int64
		var launchStatus, terminationReason string
		if err := rows.Scan(&jobID, &instanceID, &launchStatus, &terminationReason); err != nil {
			return nil, err
		}
		if reset, _ := terminalLaunchJobDisposition(&Launch{
			Status:            launchStatus,
			TerminationReason: terminationReason,
		}); reset {
			resetMap[jobID] = instanceID
		}
		instanceSet[instanceID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for id := range instanceSet {
		if _, err := NormalizeTerminalLaunchJobs(database, id); err != nil {
			return resetMap, fmt.Errorf("normalize jobs on instance %d: %w", id, err)
		}
	}
	return resetMap, nil
}

// ListRunningLaunches returns all cloud instances in a live (non-terminal)
// state: running, launching, paused, or grace.
func ListRunningLaunches(database *sql.DB) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches WHERE status IN (`+sqlPlaceholders(len(liveLaunchStatuses))+`) ORDER BY created_at DESC`,
		anySliceArgs(liveLaunchStatuses)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*Launch
	for rows.Next() {
		c, err := scanLaunchFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// CountPausedLaunches returns the number of launches in the paused state.
func CountPausedLaunches(database *sql.DB) (int, error) {
	if database == nil {
		return 0, nil
	}
	var n int
	err := database.QueryRow(`SELECT COUNT(*) FROM launches WHERE status = ?`, LaunchStatusPaused).Scan(&n)
	return n, err
}

// SumActiveLaunchCostPerHourCents returns the aggregate cost_per_hour_cents
// across live cloud launches. Paused instances still accrue storage charges,
// so they are included.
func SumActiveLaunchCostPerHourCents(database *sql.DB) (int, error) {
	if database == nil {
		return 0, nil
	}
	var total int
	err := database.QueryRow(
		`SELECT COALESCE(SUM(COALESCE(cost_per_hour_cents, 0)), 0)
		 FROM launches
		 WHERE status IN (`+sqlPlaceholders(len(liveLaunchStatuses))+`)`,
		anySliceArgs(liveLaunchStatuses)...,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// LaunchLiveState holds ephemeral R2-sourced state for a running instance,
// cached in the DB so consumers never need to read R2 directly.
type LaunchLiveState struct {
	LaunchID         int64
	InstancePhase    string // e.g. "running:316"
	BootstrapStage   string // e.g. "deps_installed"
	HeartbeatJSON    string // raw JSON HeartbeatSample
	HeartbeatTS      int64  // unix epoch seconds
	JobProgressPct   int    // 0-100, or -1 if unavailable
	JobProgressID    int64
	JobProgressPhase int // 1-based phase number (0 = unknown/single-phase)
	AgentVersion     string
	PhaseChangedAt   *int64 // unix epoch when InstancePhase last changed
	UpdatedAt        int64
}

// UpsertLaunchLiveState writes the latest ephemeral R2 state for an instance.
// PhaseChangedAt is auto-managed: set to now when InstancePhase changes from
// the previously stored value, preserved otherwise. Returns the resolved
// PhaseChangedAt so callers don't need a separate read.
func UpsertLaunchLiveState(database *sql.DB, state LaunchLiveState) (*int64, error) {
	now := time.Now().Unix()

	// Detect phase change to auto-set PhaseChangedAt.
	var oldPhase, oldBootstrap sql.NullString
	if state.PhaseChangedAt == nil {
		_ = database.QueryRow(`SELECT instance_phase, bootstrap_stage FROM launch_live_state WHERE launch_id = ?`, state.LaunchID).Scan(&oldPhase, &oldBootstrap)
		if state.InstancePhase != oldPhase.String {
			state.PhaseChangedAt = &now
		}
	} else {
		_ = database.QueryRow(`SELECT bootstrap_stage FROM launch_live_state WHERE launch_id = ?`, state.LaunchID).Scan(&oldBootstrap)
	}

	_, err := database.Exec(`INSERT INTO launch_live_state
		(launch_id, instance_phase, bootstrap_stage, heartbeat_json, heartbeat_ts,
		 job_progress_pct, job_progress_id, job_progress_phase, agent_version, phase_changed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(launch_id) DO UPDATE SET
		 instance_phase=excluded.instance_phase, bootstrap_stage=excluded.bootstrap_stage,
		 heartbeat_json=excluded.heartbeat_json, heartbeat_ts=excluded.heartbeat_ts,
		 job_progress_pct=excluded.job_progress_pct, job_progress_id=excluded.job_progress_id,
		 job_progress_phase=excluded.job_progress_phase,
		 agent_version=excluded.agent_version,
		 phase_changed_at=excluded.phase_changed_at,
		 updated_at=excluded.updated_at`,
		state.LaunchID, state.InstancePhase, state.BootstrapStage,
		state.HeartbeatJSON, state.HeartbeatTS,
		nullableProgressPct(state.JobProgressPct), state.JobProgressID,
		state.JobProgressPhase,
		state.AgentVersion, state.PhaseChangedAt, now,
	)
	if err != nil {
		return state.PhaseChangedAt, err
	}
	if shouldRecordBootstrapTransition(oldBootstrap, state.BootstrapStage) {
		if err := recordBootstrapTransition(database, state.LaunchID, state.BootstrapStage, now); err != nil {
			return state.PhaseChangedAt, err
		}
	}
	return state.PhaseChangedAt, nil
}

func shouldRecordBootstrapTransition(oldStage sql.NullString, newStage string) bool {
	newStage = strings.TrimSpace(newStage)
	if newStage == "" {
		return false
	}
	return !oldStage.Valid || strings.TrimSpace(oldStage.String) != newStage
}

func recordBootstrapTransition(database *sql.DB, launchID int64, stage string, enteredAt int64) error {
	_, err := database.Exec(`
		INSERT OR IGNORE INTO bootstrap_transitions (launch_id, stage, entered_at)
		VALUES (?, ?, ?)`,
		launchID,
		strings.TrimSpace(stage),
		enteredAt,
	)
	return err
}

// SetLaunchLiveInstancePhase records an orchestrator-observed launch phase
// without overwriting R2-sourced bootstrap, heartbeat, or job progress fields.
func SetLaunchLiveInstancePhase(database *sql.DB, launchID int64, phase string) (*int64, error) {
	now := time.Now().Unix()
	phase = strings.TrimSpace(phase)

	var oldPhase sql.NullString
	_ = database.QueryRow(`SELECT instance_phase FROM launch_live_state WHERE launch_id = ?`, launchID).Scan(&oldPhase)
	phaseChangedAt := (*int64)(nil)
	if phase != oldPhase.String {
		phaseChangedAt = &now
	}

	_, err := database.Exec(`
		INSERT INTO launch_live_state
			(launch_id, instance_phase, updated_at, phase_changed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(launch_id) DO UPDATE SET
			instance_phase=excluded.instance_phase,
			phase_changed_at=COALESCE(excluded.phase_changed_at, launch_live_state.phase_changed_at),
			updated_at=excluded.updated_at`,
		launchID, phase, now, phaseChangedAt,
	)
	if err != nil {
		return phaseChangedAt, err
	}
	return phaseChangedAt, nil
}

// nullableProgressPct returns nil if pct is -1 (unavailable), otherwise the value.
func nullableProgressPct(pct int) any {
	if pct < 0 {
		return nil
	}
	return pct
}

// GetLaunchLiveState returns the cached ephemeral state for an instance, or nil.
func GetLaunchLiveState(database *sql.DB, launchID int64) (*LaunchLiveState, error) {
	row := database.QueryRow(`SELECT launch_id, instance_phase, bootstrap_stage,
		heartbeat_json, heartbeat_ts, job_progress_pct, job_progress_id,
		job_progress_phase, agent_version, phase_changed_at, updated_at
		FROM launch_live_state WHERE launch_id = ?`, launchID)

	var s LaunchLiveState
	var phase, bootstrap, hbJSON, agentVer sql.NullString
	var hbTS, progressID sql.NullInt64
	var progressPct sql.NullInt64
	var progressPhase sql.NullInt64
	var phaseChangedAt sql.NullInt64

	err := row.Scan(&s.LaunchID, &phase, &bootstrap, &hbJSON, &hbTS,
		&progressPct, &progressID, &progressPhase, &agentVer, &phaseChangedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	s.InstancePhase = phase.String
	s.BootstrapStage = bootstrap.String
	s.HeartbeatJSON = hbJSON.String
	s.HeartbeatTS = hbTS.Int64
	if progressPct.Valid {
		s.JobProgressPct = int(progressPct.Int64)
	} else {
		s.JobProgressPct = -1
	}
	s.JobProgressID = progressID.Int64
	s.JobProgressPhase = int(progressPhase.Int64)
	s.AgentVersion = agentVer.String
	if phaseChangedAt.Valid {
		s.PhaseChangedAt = &phaseChangedAt.Int64
	}
	return &s, nil
}

// GetLaunchLiveStates returns cached ephemeral state for the specified launch IDs.
func GetLaunchLiveStates(database *sql.DB, launchIDs []int64) (map[int64]*LaunchLiveState, error) {
	result := make(map[int64]*LaunchLiveState, len(launchIDs))
	if len(launchIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, 0, len(launchIDs))
	args := make([]any, 0, len(launchIDs))
	for _, launchID := range launchIDs {
		if launchID <= 0 {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, launchID)
	}
	if len(placeholders) == 0 {
		return result, nil
	}

	query := `SELECT launch_id, instance_phase, bootstrap_stage,
		heartbeat_json, heartbeat_ts, job_progress_pct, job_progress_id,
		job_progress_phase, agent_version, phase_changed_at, updated_at
		FROM launch_live_state
		WHERE launch_id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var s LaunchLiveState
		var phase, bootstrap, hbJSON, agentVer sql.NullString
		var hbTS, progressID sql.NullInt64
		var progressPct sql.NullInt64
		var progressPhase sql.NullInt64
		var phaseChangedAt sql.NullInt64

		if err := rows.Scan(&s.LaunchID, &phase, &bootstrap, &hbJSON, &hbTS,
			&progressPct, &progressID, &progressPhase, &agentVer, &phaseChangedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}

		s.InstancePhase = phase.String
		s.BootstrapStage = bootstrap.String
		s.HeartbeatJSON = hbJSON.String
		s.HeartbeatTS = hbTS.Int64
		if progressPct.Valid {
			s.JobProgressPct = int(progressPct.Int64)
		} else {
			s.JobProgressPct = -1
		}
		s.JobProgressID = progressID.Int64
		s.JobProgressPhase = int(progressPhase.Int64)
		s.AgentVersion = agentVer.String
		if phaseChangedAt.Valid {
			s.PhaseChangedAt = &phaseChangedAt.Int64
		}
		copyState := s
		result[s.LaunchID] = &copyState
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetLaunchStatuses returns launch statuses keyed by launch ID for the supplied
// launch IDs. Missing IDs are omitted from the result map.
func GetLaunchStatuses(database *sql.DB, launchIDs []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(launchIDs))
	if len(launchIDs) == 0 {
		return result, nil
	}

	seen := make(map[int64]struct{}, len(launchIDs))
	placeholders := make([]string, 0, len(launchIDs))
	args := make([]any, 0, len(launchIDs))
	for _, launchID := range launchIDs {
		if launchID <= 0 {
			continue
		}
		if _, ok := seen[launchID]; ok {
			continue
		}
		seen[launchID] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, launchID)
	}
	if len(placeholders) == 0 {
		return result, nil
	}

	rows, err := database.Query(
		`SELECT id, status FROM launches WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var launchID int64
		var status string
		if err := rows.Scan(&launchID, &status); err != nil {
			return nil, err
		}
		result[launchID] = strings.TrimSpace(status)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// MarkOpslogSynced records a successful opslog fetch for a launch.
func MarkOpslogSynced(database *sql.DB, launchID int64) error {
	_, err := database.Exec(
		`UPDATE launches SET oplog_synced_at = ?, oplog_not_found = 0, oplog_timeout = 0 WHERE id = ?`,
		time.Now().Unix(), launchID)
	return err
}

// MarkOpslogNotFound records that an opslog was not found for a terminal launch.
func MarkOpslogNotFound(database *sql.DB, launchID int64) error {
	_, err := database.Exec(
		`UPDATE launches SET oplog_synced_at = ?, oplog_not_found = 1 WHERE id = ?`,
		time.Now().Unix(), launchID)
	return err
}

// MarkOpslogTimeout records that an opslog fetch timed out for a launch.
func MarkOpslogTimeout(database *sql.DB, launchID int64) error {
	_, err := database.Exec(
		`UPDATE launches SET oplog_synced_at = ?, oplog_timeout = 1 WHERE id = ?`,
		time.Now().Unix(), launchID)
	return err
}

// ListLaunchIDsNeedingOpslogSync returns IDs of launches that need opslog sync.
// Includes running/grace launches and recently-terminal launches, excluding those
// confirmed not-found or timed-out (checked at least 10 minutes after termination).
func ListLaunchIDsNeedingOpslogSync(database *sql.DB, since time.Duration) ([]int64, error) {
	cutoff := time.Now().Add(-since).Unix()
	const safetyBuffer = 600 // 10 minutes
	rows, err := database.Query(`
		SELECT id FROM launches
		WHERE provider_instance_id != ''
		AND (
			-- Running/paused/grace instances always need sync
			status IN (?, ?, ?)
			OR (
				-- Recently terminal instances, excluding known-not-found or timed-out
				status IN (?, ?, ?)
				AND ended_at >= ?
				AND NOT (
					(oplog_not_found = 1 OR oplog_timeout = 1)
					AND oplog_synced_at >= IFNULL(ended_at, 0) + ?
				)
			)
		)
		ORDER BY id`,
		LaunchStatusRunning, LaunchStatusPaused, LaunchStatusGrace,
		LaunchStatusFailed, LaunchStatusCompleted, LaunchStatusCancelled,
		cutoff, safetyBuffer)
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
