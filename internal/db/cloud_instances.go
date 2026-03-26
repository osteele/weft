package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/instanceintent"
)

// cloudInstanceSelectColumns is the column list for SELECT queries on cloud_instances.
const cloudInstanceSelectColumns = `id, campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb, vastai_instance_id,
		max_spend_cents, max_time_seconds, actual_spend_cents,
		created_at, ready_at, launched_at, ended_at,
		resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		inet_down_mbps, inet_up_mbps, cuda_version,
		provider_instance_id, data_center,
		instance_role, donor_instance_id, seed_download_secs, seed_copy_secs,
		grace_period_seconds, grace_started_at, grace_deadline,
		termination_reason,
		disk_gb, provisioned_inputs,
		termination_requested_at, termination_intent_json,
		results_verified,
		machine_id`

// CloudInstance status constants (same values used for both CloudInstance and Campaign).
const (
	CloudInstanceStatusPlanned   = "planned"
	CloudInstanceStatusLaunching = "launching"
	CloudInstanceStatusRunning   = "running"
	CloudInstanceStatusGrace     = "grace"
	CloudInstanceStatusCompleted = "completed"
	CloudInstanceStatusFailed    = "failed"
	CloudInstanceStatusCancelled = "canceled"
)

// Termination reason constants for CloudInstance.TerminationReason.
const (
	TerminationReasonCompleted        = "completed"
	TerminationReasonPreempted        = "preempted"
	TerminationReasonJobFailure       = "job_failure"
	TerminationReasonDiskFull         = "disk_full"
	TerminationReasonInfraFailure     = "infra_failure"
	TerminationReasonBootstrapTimeout = "bootstrap_timeout"
	TerminationReasonCancelled        = "canceled"
	TerminationReasonUnknown          = "unknown"
)

// IsRetryableTermination reports whether a failed cloud instance should be
// automatically relaunched. Returns true for infrastructure-level failures
// (preemption, infra failure, failed to launch) where retrying on a different
// instance is likely to succeed. Returns false for job-level failures, disk
// full, user cancellations, and completed instances.
func IsRetryableTermination(ci *CloudInstance) bool {
	if ci == nil || ci.Status != CloudInstanceStatusFailed {
		return false
	}
	switch ci.TerminationReason {
	case TerminationReasonPreempted, TerminationReasonInfraFailure, TerminationReasonBootstrapTimeout, TerminationReasonUnknown, "":
		return true
	case "destroyed", "error", "dead", "stopped":
		// Provider-level terminal statuses — worth retrying on a different instance.
		return true
	default:
		// "exited" (process exited), job_failure, disk_full, completed, canceled, etc.
		return false
	}
}

// CloudInstance represents a single cloud GPU deployment (e.g. one Vast.ai instance).
type CloudInstance struct {
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
	DataCenter         string // data center / geolocation of the instance
	InstanceRole       string // "worker" or "donor"
	DonorInstanceID    *int64 // DB ID of the donor instance that seeded this worker
	SeedDownloadSecs   *int   // on donor: total download duration (HF + uv)
	SeedCopySecs       *int   // on worker: copy-from-donor duration

	// Grace period (failure-tolerant rental sessions)
	GracePeriodSeconds int    // configured grace period duration (0 = disabled)
	GraceStartedAt     *int64 // when the grace period started
	GraceDeadline      *int64 // when the grace period expires

	// Termination classification
	TerminationReason      string // "completed", "preempted", "job_failure", "disk_full", "infra_failure", "canceled"
	TerminationRequestedAt *int64
	TerminationIntent      *instanceintent.Marker

	// Result verification (set by reconciler from R2 completion manifest)
	// nil = not checked (legacy marker), true = all uploads OK, false = uploads partial/failed
	ResultsVerified *bool

	// Instance capacity (for reuse matching)
	DiskGB            int      // Actual disk space from offer (may exceed requested)
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

	// Provider machine identifier (for reliability tracking)
	MachineID string
}

// DisplayGPUSpec returns a human-readable GPU spec string, falling back to
// GPUClass if GPUSpec is empty, and appending the resolved GPU name if known.
func (c *CloudInstance) DisplayGPUSpec() string {
	spec := c.GPUSpec
	if spec == "" {
		spec = c.GPUClass
	}
	if c.ResolvedGPUName != "" {
		return fmt.Sprintf("%s (%s)", c.ResolvedGPUName, spec)
	}
	return spec
}

// GraceStatusLabel returns a human-readable label for the grace period status.
// Returns empty string if the instance is not in grace status.
func (c *CloudInstance) GraceStatusLabel() string {
	if c.Status != CloudInstanceStatusGrace {
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
func (c *CloudInstance) IsTerminal() bool {
	return c.Status == CloudInstanceStatusCompleted ||
		c.Status == CloudInstanceStatusFailed ||
		c.Status == CloudInstanceStatusCancelled
}

// HasActiveTerminationIntent reports whether the instance has started a
// terminal self-destruct flow that has not yet succeeded.
func (c *CloudInstance) HasActiveTerminationIntent() bool {
	if c == nil || c.TerminationIntent == nil {
		return false
	}
	return c.TerminationIntent.TerminalStatus != "" && c.TerminationIntent.DestroySucceededAtUnix == 0
}

// EffectiveProviderID returns ProviderInstanceID, falling back to VastaiInstanceID for legacy records.
func (c *CloudInstance) EffectiveProviderID() string {
	if c.ProviderInstanceID != "" {
		return c.ProviderInstanceID
	}
	return c.VastaiInstanceID
}

// DisplayTerminationReason returns TerminationReason, falling back to Status
// when the termination reason is empty.
func (c *CloudInstance) DisplayTerminationReason() string {
	if c.TerminationReason != "" {
		return c.TerminationReason
	}
	return c.Status
}

// CreateCloudInstance inserts a new cloud instance record and returns its ID.
func CreateCloudInstance(db *sql.DB, c *CloudInstance) (int64, error) {
	now := time.Now().Unix()

	var provisionedInputsJSON *string
	if len(c.ProvisionedInputs) > 0 {
		data, _ := json.Marshal(c.ProvisionedInputs)
		s := string(data)
		provisionedInputsJSON = &s
	}

	result, err := db.Exec(
		`INSERT INTO cloud_instances (campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
		 max_spend_cents, max_time_seconds, created_at,
		 resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		 inet_down_mbps, inet_up_mbps, cuda_version,
		 disk_gb, provisioned_inputs, machine_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CampaignID, c.Status, c.Provider, c.GPUSpec, c.GPUClass, c.GPUMemGB,
		c.MaxSpendCents, c.MaxTimeSeconds, now,
		c.ResolvedGPUName, c.CostPerHourCents, c.NumGPUs, c.DLPerf, c.Reliability,
		c.InetDownMbps, c.InetUpMbps, c.CUDAVersion,
		c.DiskGB, provisionedInputsJSON, c.MachineID,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetCloudInstance retrieves a cloud instance by ID.
func GetCloudInstance(db *sql.DB, id int64) (*CloudInstance, error) {
	row := db.QueryRow(
		`SELECT `+cloudInstanceSelectColumns+` FROM cloud_instances WHERE id = ?`, id,
	)
	c, err := scanCloudInstanceFrom(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// ListCloudInstances returns all cloud instances ordered by creation time descending.
func ListCloudInstances(db *sql.DB) ([]*CloudInstance, error) {
	rows, err := db.Query(
		`SELECT ` + cloudInstanceSelectColumns + ` FROM cloud_instances ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*CloudInstance
	for rows.Next() {
		c, err := scanCloudInstanceFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// UpdateCloudInstanceStatus updates a cloud instance's status and optionally sets timestamps.
// For terminal statuses (completed, failed, canceled), an optional terminationReason
// classifies why the instance ended (e.g. "preempted", "infra_failure").
func UpdateCloudInstanceStatus(db *sql.DB, id int64, status string, terminationReason ...string) error {
	now := time.Now().Unix()
	reason := ""
	if len(terminationReason) > 0 {
		reason = terminationReason[0]
	}
	switch status {
	case CloudInstanceStatusRunning:
		_, err := db.Exec(`UPDATE cloud_instances SET status = ?, launched_at = ? WHERE id = ?`, status, now, id)
		return err
	case CloudInstanceStatusCompleted, CloudInstanceStatusFailed, CloudInstanceStatusCancelled:
		if reason != "" {
			_, err := db.Exec(`UPDATE cloud_instances SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`, status, now, reason, id)
			return err
		}
		_, err := db.Exec(`UPDATE cloud_instances SET status = ?, ended_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := db.Exec(`UPDATE cloud_instances SET status = ? WHERE id = ?`, status, id)
		return err
	}
}

// UpdateCloudInstanceResultsVerified sets the results_verified flag on a cloud instance.
func UpdateCloudInstanceResultsVerified(db *sql.DB, id int64, verified bool) error {
	_, err := db.Exec(`UPDATE cloud_instances SET results_verified = ? WHERE id = ?`, verified, id)
	return err
}

func UpdateCloudInstanceTerminationIntent(db *sql.DB, id int64, marker *instanceintent.Marker) error {
	if marker == nil {
		_, err := db.Exec(`UPDATE cloud_instances SET termination_requested_at = NULL, termination_intent_json = NULL WHERE id = ?`, id)
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
	_, err = db.Exec(`UPDATE cloud_instances SET termination_requested_at = ?, termination_intent_json = ? WHERE id = ?`,
		requestedAt, string(data), id)
	return err
}

// SetCloudInstanceReadyAt records when the Vast.ai instance became ready.
func SetCloudInstanceReadyAt(db *sql.DB, id int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(`UPDATE cloud_instances SET ready_at = ? WHERE id = ?`, now, id)
	return err
}

// SetCloudInstanceProviderID sets the provider-neutral instance ID for a cloud instance.
// Also sets vastai_instance_id for backwards compatibility when the provider is vastai.
func SetCloudInstanceProviderID(db *sql.DB, id int64, providerID string) error {
	_, err := db.Exec(`UPDATE cloud_instances SET provider_instance_id = ?, vastai_instance_id = ? WHERE id = ?`, providerID, providerID, id)
	return err
}

// UpdateCloudInstanceOfferMetadata refreshes the offer-derived fields on a cloud
// instance row before a create-instance retry uses a replacement offer.
func UpdateCloudInstanceOfferMetadata(database *sql.DB, id int64, offer cloud.Offer) error {
	_, err := database.Exec(
		`UPDATE cloud_instances
		 SET resolved_gpu_name = ?, cost_per_hour_cents = ?, num_gpus = ?, dl_perf = ?, reliability = ?,
		     inet_down_mbps = ?, inet_up_mbps = ?, cuda_version = ?, disk_gb = ?
		 WHERE id = ?`,
		offer.GPUName,
		int(offer.CostPerHour*100),
		offer.NumGPUs,
		offer.DLPerf,
		offer.Reliability,
		offer.DownloadBandwidth,
		offer.UploadBandwidth,
		offer.CUDAVersion,
		int(offer.DiskSpaceGB),
		id,
	)
	return err
}

// SetCloudInstanceVastaiID sets the Vast.ai instance ID for a cloud instance.
// Deprecated: use SetCloudInstanceProviderID instead.
func SetCloudInstanceVastaiID(db *sql.DB, id int64, vastaiID string) error {
	return SetCloudInstanceProviderID(db, id, vastaiID)
}

// SetCloudInstanceDataCenter sets the data center/geolocation for a cloud instance.
func SetCloudInstanceDataCenter(db *sql.DB, id int64, dc string) error {
	_, err := db.Exec(`UPDATE cloud_instances SET data_center = ? WHERE id = ?`, dc, id)
	return err
}

// SetCloudInstanceActualSpend updates the actual spend in cents.
func SetCloudInstanceActualSpend(db *sql.DB, id int64, cents int) error {
	_, err := db.Exec(`UPDATE cloud_instances SET actual_spend_cents = ? WHERE id = ?`, cents, id)
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
	err := database.QueryRow(`SELECT termination_reason FROM cloud_instances WHERE id = ?`, instanceID).Scan(&current)
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
			  AND ja.cloud_instance_id = ?
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
		`UPDATE cloud_instances
		 SET termination_reason = ?
		 WHERE id = ? AND termination_reason = ?`,
		TerminationReasonDiskFull, instanceID, TerminationReasonJobFailure,
	)
	return err
}

// SetCloudInstanceRole sets the instance role ("worker" or "donor").
func SetCloudInstanceRole(db *sql.DB, id int64, role string) error {
	_, err := db.Exec(`UPDATE cloud_instances SET instance_role = ? WHERE id = ?`, role, id)
	return err
}

// SetCloudInstanceDonorID sets the DB ID of the donor instance that seeded this worker.
func SetCloudInstanceDonorID(db *sql.DB, id int64, donorID int64) error {
	_, err := db.Exec(`UPDATE cloud_instances SET donor_instance_id = ? WHERE id = ?`, donorID, id)
	return err
}

// SetCloudInstanceSeedDownloadSecs records the total download duration on a donor instance.
func SetCloudInstanceSeedDownloadSecs(db *sql.DB, id int64, secs int) error {
	_, err := db.Exec(`UPDATE cloud_instances SET seed_download_secs = ? WHERE id = ?`, secs, id)
	return err
}

// SetCloudInstanceSeedCopySecs records the copy-from-donor duration on a worker instance.
func SetCloudInstanceSeedCopySecs(db *sql.DB, id int64, secs int) error {
	_, err := db.Exec(`UPDATE cloud_instances SET seed_copy_secs = ? WHERE id = ?`, secs, id)
	return err
}

// CloudInstanceHost returns the synthetic host name for a cloud instance.
// CloudInstanceHost returns the legacy synthetic host name used by older
// records. New code should prefer CloudInstanceID and TargetKind helpers.
func CloudInstanceHost(instanceID int64) string {
	return fmt.Sprintf("vastai:%d", instanceID)
}

// IsCloudHost reports whether a host string refers to a legacy synthetic
// rental host name (e.g. "vastai:123" or "runpod:456").
func IsCloudHost(host string) bool {
	return strings.HasPrefix(host, "vastai:") || strings.HasPrefix(host, "runpod:")
}

// SetJobCloudInstanceID associates a job with a cloud instance and records the attempt.
// New assignments clear jobs.host so cloud_instance_id remains the canonical
// rental target while preserving legacy synthetic host reads.
func SetJobCloudInstanceID(db *sql.DB, jobID, instanceID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// Update attempt's cloud_instance_id
	if err := SetAttemptCloudInstanceID(tx, jobID, instanceID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET cloud_instance_id = ?, host = '', placement_reasons = NULL WHERE id = ?`, instanceID, jobID); err != nil {
		tx.Rollback()
		return err
	}
	// Mark any previous cloud attempts as superseded
	if _, err := tx.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?
		 WHERE job_id = ? AND cloud_instance_id IS NOT NULL AND end_time IS NOT NULL AND cloud_outcome IS NULL`,
		AttemptOutcomeSuperseded, jobID,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("mark superseded attempts: %w", err)
	}
	// Also close+supersede the legacy job_cloud_attempts table (if it still exists)
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`UPDATE job_cloud_attempts SET ended_at = ?, outcome = ? WHERE job_id = ? AND ended_at IS NULL`,
		now, AttemptOutcomeSuperseded, jobID,
	); err != nil && !isNoSuchTable(err) {
		tx.Rollback()
		return fmt.Errorf("close legacy cloud attempt: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO job_cloud_attempts (job_id, cloud_instance_id, started_at) VALUES (?, ?, ?)`,
		jobID, instanceID, now,
	); err != nil && !isNoSuchTable(err) {
		tx.Rollback()
		return fmt.Errorf("insert legacy cloud attempt: %w", err)
	}
	return tx.Commit()
}

// SetJobCampaignIndex sets the 0-based position of a job within its campaign sequence.
func SetJobCampaignIndex(db *sql.DB, jobID int64, index int) error {
	_, err := db.Exec(`UPDATE jobs SET campaign_job_index = ? WHERE id = ?`, index, jobID)
	return err
}

// GetCloudInstanceJobs returns all jobs associated with a cloud instance.
func GetCloudInstanceJobs(db *sql.DB, instanceID int64) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE cloud_instance_id = ? AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, instanceID)
}

// GetCloudInstanceJobsIncludingAttempts returns jobs currently associated with a
// cloud instance, jobs with an open cloud attempt on that instance, or jobs
// that only have historical cloud attempt records for it. This is useful for
// display purposes: after ResetCloudInstanceJobs clears cloud_instance_id on
// re-queued jobs, those jobs are still visible via the job_cloud_attempts
// table.
func GetCloudInstanceJobsIncludingAttempts(database *sql.DB, instanceID int64) ([]*Job, error) {
	// Jobs with a campaign_job_index (the launched jobs) sort by that index;
	// historical-only jobs (no index) follow. Within each group, sort by id.
	// membership_rank is a final tiebreaker so the current row wins over the
	// historical row for the same job when deduplicating below.
	query := fmt.Sprintf(`SELECT %s FROM cloud_instance_job_membership
		WHERE membership_cloud_instance_id = ?
		  AND tombstoned = 0
		ORDER BY
			CASE WHEN campaign_job_index IS NOT NULL THEN 0 ELSE 1 END ASC,
			campaign_job_index ASC,
			id ASC,
			membership_rank ASC`, qualifiedJobSelectColumns("cloud_instance_job_membership"))
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

	// For historical jobs (no longer assigned to this instance), override the
	// display status with the attempt outcome so the UI shows what happened on
	// this instance rather than the job's current global status.
	var historical []*Job
	for _, job := range result {
		if job.CloudInstanceID == nil || *job.CloudInstanceID != instanceID {
			historical = append(historical, job)
		}
	}
	if len(historical) > 0 {
		outcomes, err := GetAttemptOutcomesByInstance(database, instanceID)
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
				// Leave status as-is (queued); JobDisplayStatus() will use
				// the attempt outcome for display.
			case AttemptOutcomeCompleted:
				job.Status = StatusCompleted
			case AttemptOutcomeCancelled:
				job.Status = StatusCanceled
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		  AND effective_target_kind = ?
		  AND status = ?
		  AND host = ''
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, string(JobTargetUnplaced), StatusQueued)
}

// AssignJobHost atomically assigns a host to an unplaced queued job.
// Returns true if the job was updated (false if it was already claimed or changed status).
// Uses COALESCE(pending_status, status) so that a failed job with
// pending_status='queued' (retry intent) is eligible for placement.
func AssignJobHost(database *sql.DB, jobID int64, host string) (bool, error) {
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
	// Update host on the attempt (may be closed if pending retry)
	result, err := database.Exec(
		`UPDATE job_attempts SET host = ?
		 WHERE id = `+latestAttemptSubquery+` AND host = ''`,
		host, jobID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := database.Exec(`UPDATE jobs SET placement_reasons = NULL WHERE id = ?`, jobID); err != nil {
		return false, err
	}
	return true, nil
}

// ResetCloudInstanceJobs resets non-terminal jobs in a cloud instance back to
// unplaced (queued with empty host) and clears their instance association.
// Records the attempt outcome before resetting. Returns the number of jobs reset.
func ResetCloudInstanceJobs(database *sql.DB, instanceID int64, outcome string) (int64, error) {
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	ci, err := scanCloudInstanceFrom(tx.QueryRow(`SELECT `+cloudInstanceSelectColumns+` FROM cloud_instances WHERE id = ?`, instanceID))
	if err != nil && err != sql.ErrNoRows {
		tx.Rollback()
		return 0, err
	}
	placementReasons := encodeStringSlice(cloudResetPlacementReasons(ci, outcome))

	jobs, err := queryJobsTx(tx,
		fmt.Sprintf(`SELECT %s FROM job_status WHERE cloud_instance_id = ? AND status NOT IN (?, ?, ?, ?, ?, ?) AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns),
		instanceID,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
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
	placeholders := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobIDs = append(jobIDs, job.ID)
		placeholders = append(placeholders, "?")
	}
	jobFilter := strings.Join(placeholders, ", ")
	updateArgs := make([]any, 0, len(jobIDs)+2)
	updateArgs = append(updateArgs, StatusQueued, placementReasons)
	for _, jobID := range jobIDs {
		updateArgs = append(updateArgs, jobID)
	}

	// Close the old attempt (preserves cloud_instance_id as historical record)
	// and create a fresh unplaced attempt for each reset job.
	// Must happen BEFORE updating jobs.cloud_instance_id to NULL, because the
	// trigger prevents clearing cloud_instance_id while live attempts exist.
	now := time.Now().Unix()
	for _, jobID := range jobIDs {
		if _, err := tx.Exec(`
			UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ?
			WHERE job_id = ? AND end_time IS NULL`,
			StatusCanceled, now, outcome, jobID,
		); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("close attempt for job %d: %w", jobID, err)
		}
		if _, err := createAttemptTx(tx, jobID, "", nil, StatusQueued); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("create fresh attempt for job %d: %w", jobID, err)
		}
	}

	// Close legacy job_cloud_attempts (if table still exists).
	if _, err := tx.Exec(
		`UPDATE job_cloud_attempts
		 SET ended_at = ?, outcome = ?
		 WHERE cloud_instance_id = ? AND ended_at IS NULL
		 AND job_id IN (
		 	SELECT id FROM jobs
		 	WHERE cloud_instance_id = ? AND status NOT IN (?, ?, ?, ?, ?, ?) AND tombstoned = 0
		 )`,
		now, outcome, instanceID, instanceID,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
	); err != nil && !isNoSuchTable(err) {
		tx.Rollback()
		return 0, fmt.Errorf("close legacy cloud attempts: %w", err)
	}

	result, err := tx.Exec(
		fmt.Sprintf(`UPDATE jobs SET status = ?, cloud_instance_id = NULL, host = '',
		 start_time = NULL, end_time = NULL, exit_code = NULL, error_message = NULL,
		 session_name = NULL, failure_reason = NULL, error_diagnosis = NULL,
		 remote_state = NULL, remote_id = NULL, placement_reasons = ?
		 WHERE id IN (%s)`, jobFilter),
		updateArgs...,
	)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		tx.Rollback()
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func cloudResetPlacementReasons(ci *CloudInstance, outcome string) []string {
	if ci == nil {
		return []string{"returned from cloud instance to unplaced queue"}
	}

	switch ci.Status {
	case CloudInstanceStatusFailed:
		if ci.TerminationReason != "" {
			return []string{fmt.Sprintf("cloud instance %d failed (%s)", ci.ID, ci.TerminationReason)}
		}
		return []string{fmt.Sprintf("cloud instance %d failed", ci.ID)}
	case CloudInstanceStatusCancelled:
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
		SELECT id, host FROM jobs
		WHERE status IN (?, ?) AND host LIKE 'vastai:%' AND tombstoned = 0
		AND NOT EXISTS (
			SELECT 1 FROM cloud_instances ci
			WHERE ci.id = CAST(SUBSTR(jobs.host, 8) AS INTEGER)
			AND ci.status IN (?, ?, ?, ?)
		)`,
		StatusQueued, StatusRunning,
		CloudInstanceStatusRunning, CloudInstanceStatusLaunching, CloudInstanceStatusGrace, CloudInstanceStatusCompleted,
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

	for _, job := range jobs {
		if _, err := tx.Exec(
			`UPDATE jobs SET status = ?, host = '', cloud_instance_id = NULL, placement_reasons = ?
			 WHERE id = ?`,
			StatusQueued, encodeStringSlice(orphanedCloudPlacementReasons(job.host)), job.id,
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

// GetCloudInstanceJobCounts returns a map from cloud instance ID to job count.
func GetCloudInstanceJobCounts(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT cloud_instance_id, COUNT(*) FROM jobs WHERE cloud_instance_id IS NOT NULL AND tombstoned = 0 GROUP BY cloud_instance_id`)
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

// GetActiveCloudInstanceJobCounts returns a map from cloud instance ID to the
// number of non-terminal jobs still assigned to that instance.
func GetActiveCloudInstanceJobCounts(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT cloud_instance_id, COUNT(*) FROM job_status
		WHERE cloud_instance_id IS NOT NULL
		  AND tombstoned = 0
		  AND effective_target_kind = ?
		  AND status NOT IN (?, ?, ?, ?, ?, ?)
		GROUP BY cloud_instance_id`,
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
func GetCampaignInstances(db *sql.DB, campaignID int64) ([]*CloudInstance, error) {
	rows, err := db.Query(
		`SELECT `+cloudInstanceSelectColumns+` FROM cloud_instances WHERE campaign_id = ? ORDER BY created_at ASC`, campaignID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*CloudInstance
	for rows.Next() {
		c, err := scanCloudInstanceFrom(rows)
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

func scanCloudInstanceFrom(s cloudInstanceScanner) (*CloudInstance, error) {
	var c CloudInstance
	var campaignID sql.NullInt64
	var gpuSpec, gpuClass, vastaiInstanceID sql.NullString
	var gpuMemGB, maxSpend, maxTime, actualSpend sql.NullInt64
	var readyAt, launchedAt, endedAt sql.NullInt64
	var resolvedGPUName sql.NullString
	var costPerHourCents, numGPUs sql.NullInt64
	var dlPerf, reliability, inetDown, inetUp, cudaVersion sql.NullFloat64
	var providerInstanceID, dataCenter sql.NullString
	var instanceRole sql.NullString
	var donorInstanceID sql.NullInt64
	var seedDownloadSecs, seedCopySecs sql.NullInt64
	var gracePeriodSeconds sql.NullInt64
	var graceStartedAt, graceDeadline sql.NullInt64
	var terminationReason sql.NullString
	var diskGB sql.NullInt64
	var provisionedInputsJSON sql.NullString
	var terminationRequestedAt sql.NullInt64
	var terminationIntentJSON sql.NullString
	var resultsVerified sql.NullBool
	var machineID sql.NullString

	err := s.Scan(
		&c.ID, &campaignID, &c.Status, &c.Provider, &gpuSpec, &gpuClass, &gpuMemGB,
		&vastaiInstanceID, &maxSpend, &maxTime, &actualSpend,
		&c.CreatedAt, &readyAt, &launchedAt, &endedAt,
		&resolvedGPUName, &costPerHourCents, &numGPUs, &dlPerf, &reliability,
		&inetDown, &inetUp, &cudaVersion,
		&providerInstanceID, &dataCenter,
		&instanceRole, &donorInstanceID, &seedDownloadSecs, &seedCopySecs,
		&gracePeriodSeconds, &graceStartedAt, &graceDeadline,
		&terminationReason,
		&diskGB, &provisionedInputsJSON,
		&terminationRequestedAt, &terminationIntentJSON,
		&resultsVerified,
		&machineID,
	)
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
	if vastaiInstanceID.Valid {
		c.VastaiInstanceID = vastaiInstanceID.String
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
	return &c, nil
}

// Job cloud attempt outcome constants.
const (
	AttemptOutcomeCompleted  = "completed"
	AttemptOutcomeFailed     = "failed"
	AttemptOutcomeCancelled  = "canceled"
	AttemptOutcomeOrphaned   = "orphaned"
	AttemptOutcomeSuperseded = "superseded"
)

// JobCloudAttempt records a single association between a job and a cloud instance.
type JobCloudAttempt struct {
	ID              int64
	JobID           int64
	CloudInstanceID int64
	StartedAt       int64
	EndedAt         *int64
	Outcome         string // "completed", "failed", "canceled", "orphaned"
}

// CloseJobCloudAttempt sets cloud_outcome on the latest cloud-associated attempt for a job.
func CloseJobCloudAttempt(database *sql.DB, jobID int64, outcome string) error {
	_, err := database.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?
		 WHERE id = (
			SELECT id FROM job_attempts
			WHERE job_id = ? AND cloud_instance_id IS NOT NULL
			ORDER BY attempt_number DESC LIMIT 1
		 )`,
		outcome, jobID,
	)
	return err
}

// CloseJobCloudAttemptsByInstance sets cloud_outcome on all open attempts
// associated with the given cloud instance.
func CloseJobCloudAttemptsByInstance(database *sql.DB, instanceID int64, outcome string) error {
	_, err := database.Exec(
		`UPDATE job_attempts SET cloud_outcome = ?
		 WHERE cloud_instance_id = ? AND end_time IS NULL`,
		outcome, instanceID,
	)
	return err
}

// CountJobCloudAttempts returns the number of cloud-associated attempts for a job.
func CountJobCloudAttempts(database *sql.DB, jobID int64) (int, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts WHERE job_id = ? AND cloud_instance_id IS NOT NULL`,
		jobID,
	).Scan(&count)
	return count, err
}

// GetJobCloudAttempts returns the cloud attempt history for a job, ordered by start time.
func GetJobCloudAttempts(database *sql.DB, jobID int64) ([]JobCloudAttempt, error) {
	rows, err := database.Query(
		`SELECT id, job_id, cloud_instance_id, COALESCE(queued_at, start_time, 0), end_time, cloud_outcome
		 FROM job_attempts
		 WHERE job_id = ? AND cloud_instance_id IS NOT NULL
		 ORDER BY attempt_number ASC`,
		jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []JobCloudAttempt
	for rows.Next() {
		var a JobCloudAttempt
		var endedAt sql.NullInt64
		var outcome sql.NullString
		if err := rows.Scan(&a.ID, &a.JobID, &a.CloudInstanceID, &a.StartedAt, &endedAt, &outcome); err != nil {
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

// GetAttemptOutcomesByInstance returns the cloud_outcome for each job attempt
// that ran on the given cloud instance. Returns map[jobID]outcome.
func GetAttemptOutcomesByInstance(database *sql.DB, cloudInstanceID int64) (map[int64]string, error) {
	rows, err := database.Query(
		`SELECT job_id, cloud_outcome FROM job_attempts
		 WHERE cloud_instance_id = ? AND cloud_outcome IS NOT NULL
		 ORDER BY attempt_number ASC`,
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

// SetCloudInstanceGracePeriod stores the configured grace period for a cloud instance.
func SetCloudInstanceGracePeriod(db *sql.DB, id int64, seconds int) error {
	_, err := db.Exec(`UPDATE cloud_instances SET grace_period_seconds = ? WHERE id = ?`, seconds, id)
	return err
}

// SetCloudInstanceGraceStarted transitions an instance to grace status with a deadline.
func SetCloudInstanceGraceStarted(db *sql.DB, id int64, deadline int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE cloud_instances SET status = ?, grace_started_at = ?, grace_deadline = ? WHERE id = ?`,
		CloudInstanceStatusGrace, now, deadline, id,
	)
	return err
}

// ExtendCloudInstanceGrace updates the grace deadline for an instance.
func ExtendCloudInstanceGrace(db *sql.DB, id int64, newDeadline int64) error {
	_, err := db.Exec(`UPDATE cloud_instances SET grace_deadline = ? WHERE id = ?`, newDeadline, id)
	return err
}

// ListRecentlyTerminalCloudInstances returns cloud instances that reached a terminal status
// (failed, completed, canceled) within the last `since` duration and have a provider ID.
// Used as a safety net to destroy leaked provider instances.
func ListRecentlyTerminalCloudInstances(database *sql.DB, since time.Duration) ([]*CloudInstance, error) {
	cutoff := time.Now().Add(-since).Unix()
	rows, err := database.Query(
		`SELECT `+cloudInstanceSelectColumns+` FROM cloud_instances
		 WHERE status IN (?, ?, ?)
		 AND ended_at >= ?
		 AND (provider_instance_id != '' OR vastai_instance_id != '')
		 ORDER BY ended_at DESC`,
		CloudInstanceStatusFailed, CloudInstanceStatusCompleted, CloudInstanceStatusCancelled,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*CloudInstance
	for rows.Next() {
		c, err := scanCloudInstanceFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}

// NormalizeTerminalCloudInstanceJobs repairs unresolved jobs on a terminal cloud
// instance. Failed instances orphan active jobs; canceled instances cancel them.
// Completed instances are left unchanged so result sync can still finalize them.
func NormalizeTerminalCloudInstanceJobs(database *sql.DB, instanceID int64) (int64, error) {
	ci, err := GetCloudInstance(database, instanceID)
	if err != nil {
		return 0, err
	}
	if ci == nil {
		return 0, nil
	}

	switch ci.Status {
	case CloudInstanceStatusFailed:
		return ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeOrphaned)
	case CloudInstanceStatusCancelled:
		return ResetCloudInstanceJobs(database, instanceID, AttemptOutcomeCancelled)
	default:
		return 0, nil
	}
}

// ResetJobsOnTerminalCloudInstances finds non-terminal jobs associated with
// failed or canceled cloud instances and resets them to queued/unplaced.
// Completed instances are intentionally skipped until result sync finalizes them.
// This is a DB-only repair pass that doesn't require cloud provider clients.
func ResetJobsOnTerminalCloudInstances(database *sql.DB) (int64, error) {
	// Find non-success terminal instances that still have non-terminal jobs.
	rows, err := database.Query(`
		SELECT DISTINCT ci.id
		FROM cloud_instances ci
		JOIN jobs j ON j.cloud_instance_id = ci.id
		WHERE ci.status IN (?, ?)
		AND j.status NOT IN (?, ?, ?, ?, ?, ?)
		AND j.tombstoned = 0`,
		CloudInstanceStatusFailed, CloudInstanceStatusCancelled,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled, StatusDraft,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var instanceIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		instanceIDs = append(instanceIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, id := range instanceIDs {
		n, err := NormalizeTerminalCloudInstanceJobs(database, id)
		if err != nil {
			return total, fmt.Errorf("normalize jobs on instance %d: %w", id, err)
		}
		total += n
	}
	return total, nil
}

// ListRunningCloudInstances returns all cloud instances with "running", "launching", or "grace" status.
func ListRunningCloudInstances(database *sql.DB) ([]*CloudInstance, error) {
	rows, err := database.Query(
		`SELECT `+cloudInstanceSelectColumns+` FROM cloud_instances WHERE status IN (?, ?, ?) ORDER BY created_at DESC`,
		CloudInstanceStatusRunning, CloudInstanceStatusLaunching, CloudInstanceStatusGrace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []*CloudInstance
	for rows.Next() {
		c, err := scanCloudInstanceFrom(rows)
		if err != nil {
			return nil, err
		}
		instances = append(instances, c)
	}
	return instances, rows.Err()
}
