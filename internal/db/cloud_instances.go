package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/instanceintent"
)

// launchSelectColumns is the column list for SELECT queries on launches.
const launchSelectColumns = `id, campaign_id, host_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
		max_spend_cents, max_time_seconds, actual_spend_cents,
		created_at, ready_at, launched_at, ended_at,
		resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		inet_down_mbps, inet_up_mbps, cuda_version,
		provider_instance_id, data_center,
		instance_role, donor_instance_id, seed_download_secs, seed_copy_secs, replaced_instance_id,
		grace_period_seconds, grace_started_at, grace_deadline,
		termination_reason, termination_detail,
		disk_gb, provisioned_inputs,
		termination_requested_at, termination_intent_json,
		results_verified,
		machine_id, docker_image,
		provider_running_at`

// Launch status constants (same values used for both Launch and Campaign).
const (
	LaunchStatusPlanned   = "planned"
	LaunchStatusLaunching = "launching"
	LaunchStatusRunning   = "running"
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
	TerminationReasonUnknown          = "unknown"
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
	case TerminationReasonProviderFailure, TerminationReasonInfraFailure, TerminationReasonBootstrapTimeout, TerminationReasonPhaseStall, TerminationReasonUnknown, "":
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
	DataCenter         string // data center / geolocation of the instance
	InstanceRole       string // "worker" or "donor"
	DonorInstanceID    *int64 // DB ID of the donor instance that seeded this worker (data locality)
	SeedDownloadSecs   *int   // on donor: total download duration (HF + uv)
	SeedCopySecs       *int   // on worker: copy-from-donor duration
	ReplacedInstanceID *int64 // DB ID of the failed instance this one replaces (relaunch chain)

	// Grace period (failure-tolerant rental sessions)
	GracePeriodSeconds int    // configured grace period duration (0 = disabled)
	GraceStartedAt     *int64 // when the grace period started
	GraceDeadline      *int64 // when the grace period expires

	// Termination classification
	TerminationReason      string // "completed", "provider_failure", "job_failure", "disk_full", "infra_failure", "canceled"
	TerminationDetail      string // human-readable detail for the termination reason
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

	// DockerImage is the Docker image used for this instance (e.g. "nvidia/cuda:12.4.1-runtime-ubuntu22.04").
	DockerImage string

	// ProviderRunningAt is when the cloud provider first reported the instance
	// as "running" (Docker container started, onstart can execute). Bootstrap
	// timeout is measured from this timestamp, not LaunchedAt.
	ProviderRunningAt *int64
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
	return c.Status == LaunchStatusCompleted ||
		c.Status == LaunchStatusFailed ||
		c.Status == LaunchStatusCancelled
}

// isActiveLaunchStatus reports whether a launch status represents an
// actively-progressing instance that should claim its jobs.
func isActiveLaunchStatus(status string) bool {
	switch status {
	case LaunchStatusLaunching, LaunchStatusRunning, LaunchStatusGrace, LaunchStatusCompleted:
		return true
	default:
		return false
	}
}

// HasActiveTerminationIntent reports whether the instance has started a
// terminal self-destruct flow that has not yet succeeded.
func (c *Launch) HasActiveTerminationIntent() bool {
	if c == nil || c.TerminationIntent == nil {
		return false
	}
	return c.TerminationIntent.TerminalStatus != "" && c.TerminationIntent.DestroySucceededAtUnix == 0
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

	result, err := db.Exec(
		`INSERT INTO launches (campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
		 max_spend_cents, max_time_seconds, created_at,
		 resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		 inet_down_mbps, inet_up_mbps, cuda_version,
		 disk_gb, provisioned_inputs, machine_id, docker_image)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CampaignID, c.Status, c.Provider, c.GPUSpec, c.GPUClass, c.GPUMemGB,
		c.MaxSpendCents, c.MaxTimeSeconds, now,
		c.ResolvedGPUName, c.CostPerHourCents, c.NumGPUs, c.DLPerf, c.Reliability,
		c.InetDownMbps, c.InetUpMbps, c.CUDAVersion,
		c.DiskGB, provisionedInputsJSON, c.MachineID, c.DockerImage,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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
	switch status {
	case LaunchStatusRunning:
		_, err := db.Exec(`UPDATE launches SET status = ?, launched_at = ? WHERE id = ?`, status, now, id)
		return err
	case LaunchStatusCompleted, LaunchStatusFailed, LaunchStatusCancelled:
		// Don't overwrite an already-terminal instance that has a termination
		// reason set (e.g., bootstrap_timeout → job_failure on next pass).
		var currentStatus, currentReason string
		if err := db.QueryRow(`SELECT status, COALESCE(termination_reason, '') FROM launches WHERE id = ?`, id).Scan(&currentStatus, &currentReason); err == nil {
			isTerminal := currentStatus == LaunchStatusCompleted || currentStatus == LaunchStatusFailed || currentStatus == LaunchStatusCancelled
			if isTerminal && currentReason != "" {
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
		_, err := db.Exec(`UPDATE launches SET status = ?, ended_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := db.Exec(`UPDATE launches SET status = ? WHERE id = ?`, status, id)
		return err
	}
}

// UpdateLaunchDockerImage sets the Docker image used for a launch.
func UpdateLaunchDockerImage(db *sql.DB, id int64, image string) error {
	_, err := db.Exec(`UPDATE launches SET docker_image = ? WHERE id = ?`, image, id)
	return err
}

// UpdateLaunchResultsVerified sets the results_verified flag on a cloud instance.
func UpdateLaunchResultsVerified(db *sql.DB, id int64, verified bool) error {
	_, err := db.Exec(`UPDATE launches SET results_verified = ? WHERE id = ?`, verified, id)
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

// SetLaunchProviderRunningAt records when the cloud provider first reported the
// instance as "running". Only writes if not already set (first transition wins).
func SetLaunchProviderRunningAt(db *sql.DB, id int64, t time.Time) error {
	_, err := db.Exec(
		`UPDATE launches SET provider_running_at = ? WHERE id = ? AND provider_running_at IS NULL`,
		t.Unix(), id)
	return err
}

// SetLaunchProviderID sets the provider-neutral instance ID for a cloud instance.
func SetLaunchProviderID(db *sql.DB, id int64, providerID string) error {
	_, err := db.Exec(`UPDATE launches SET provider_instance_id = ? WHERE id = ?`, providerID, id)
	return err
}

// UpdateLaunchOfferMetadata refreshes the offer-derived fields on a cloud
// instance row before a create-instance retry uses a replacement offer.
func UpdateLaunchOfferMetadata(database *sql.DB, id int64, offer cloud.Offer) error {
	_, err := database.Exec(
		`UPDATE launches
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
	return strings.HasPrefix(host, "vastai:") || strings.HasPrefix(host, "runpod:")
}

// SetJobLaunchID associates a job with a cloud instance by creating a new
// attempt with launch_id set. The job must have an open queued attempt
// (integrity violation otherwise). The open attempt is closed and a fresh
// attempt is created with the instance assignment.
func SetJobLaunchID(database *sql.DB, jobID, instanceID int64) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}

	// Check if the job is already claimed by an active launch.
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

	// Close any existing open attempt and create the placement attempt.
	now := time.Now().Unix()
	if err := closeOpenAttempts(tx, jobID, now); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := createAttemptTx(tx, jobID, "", &instanceID, StatusQueued); err != nil {
		tx.Rollback()
		return err
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
	_, err := db.Exec(`UPDATE jobs SET campaign_job_index = ? WHERE id = ?`, index, jobID)
	return err
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
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		  AND effective_target_kind = ?
		  AND status = ?
		  AND host = ''
		ORDER BY id ASC`, jobSelectColumns)
	return queryJobs(db, query, string(JobTargetUnplaced), StatusQueued)
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
	if attemptID == 0 {
		if _, err := CreateAttempt(database, jobID, host, nil, StatusQueued); err != nil {
			return false, err
		}
	} else {
		// Update host on the existing attempt (may be closed if pending retry)
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

	jobs, err := queryJobsTx(tx,
		fmt.Sprintf(`SELECT %s FROM job_status WHERE launch_id = ? AND status NOT IN (?, ?, ?, ?, ?, ?) AND tombstoned = 0 ORDER BY id ASC`, jobSelectColumns),
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

	now := time.Now().Unix()
	for _, jobID := range jobIDs {
		if err := closeAttemptsAndRequeue(tx, jobID, now); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("requeue job %d: %w", jobID, err)
		}
		// Also record cloud_outcome on the closed attempt.
		if _, err := tx.Exec(`UPDATE job_attempts SET cloud_outcome = ? WHERE job_id = ? AND end_time = ?`,
			outcome, jobID, now); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("set cloud_outcome for job %d: %w", jobID, err)
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
	}

	placementArgs := make([]any, 0, len(jobIDs)+1)
	placementArgs = append(placementArgs, placementReasons)
	for _, jobID := range jobIDs {
		placementArgs = append(placementArgs, jobID)
	}
	result, err := tx.Exec(
		fmt.Sprintf(`UPDATE jobs SET placement_reasons = ? WHERE id IN (%s)`, jobFilter),
		placementArgs...,
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
			AND ci.status IN (?, ?, ?, ?)
		)`,
		StatusQueued, StatusRunning,
		LaunchStatusRunning, LaunchStatusLaunching, LaunchStatusGrace, LaunchStatusCompleted,
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
	var resolvedGPUName sql.NullString
	var costPerHourCents, numGPUs sql.NullInt64
	var dlPerf, reliability, inetDown, inetUp, cudaVersion sql.NullFloat64
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

	err := s.Scan(
		&c.ID, &campaignID, &hostID, &c.Status, &c.Provider, &gpuSpec, &gpuClass, &gpuMemGB,
		&maxSpend, &maxTime, &actualSpend,
		&c.CreatedAt, &readyAt, &launchedAt, &endedAt,
		&resolvedGPUName, &costPerHourCents, &numGPUs, &dlPerf, &reliability,
		&inetDown, &inetUp, &cudaVersion,
		&providerInstanceID, &dataCenter,
		&instanceRole, &donorInstanceID, &seedDownloadSecs, &seedCopySecs, &replacedInstanceID,
		&gracePeriodSeconds, &graceStartedAt, &graceDeadline,
		&terminationReason, &terminationDetail,
		&diskGB, &provisionedInputsJSON,
		&terminationRequestedAt, &terminationIntentJSON,
		&resultsVerified,
		&machineID, &dockerImage,
		&providerRunningAt,
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

// CloseLaunchAttempts sets cloud_outcome on all open attempts
// associated with the given cloud instance.
func CloseLaunchAttempts(database *sql.DB, instanceID int64, outcome string) error {
	// Map cloud outcome to attempt status: "failed" → failed, "completed" → completed,
	// everything else (orphaned, superseded) → canceled.
	attemptStatus := StatusCanceled
	switch outcome {
	case AttemptOutcomeFailed:
		attemptStatus = StatusFailed
	case AttemptOutcomeCompleted:
		attemptStatus = StatusCompleted
	}
	now := time.Now().Unix()
	if outcome == AttemptOutcomeCompleted {
		_, err := database.Exec(
			`UPDATE job_attempts
			 SET status = ?, cloud_outcome = ?, end_time = COALESCE(end_time, ?),
			     exit_code = COALESCE(exit_code, 0), last_synced_status = ?, pending_status = NULL
			 WHERE launch_id = ? AND end_time IS NULL`,
			attemptStatus, outcome, now, StatusCompleted, instanceID,
		)
		return err
	}
	_, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, cloud_outcome = ?, end_time = COALESCE(end_time, ?), pending_status = NULL
		 WHERE launch_id = ? AND end_time IS NULL`,
		attemptStatus, outcome, now, instanceID,
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

// CountLaunchAttempts returns the number of cloud-associated attempts for a job.
func CountLaunchAttempts(database *sql.DB, jobID int64) (int, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts
		 WHERE job_id = ?
		   AND launch_id IS NOT NULL
		   AND COALESCE(cloud_outcome, '') != ?`,
		jobID, AttemptOutcomeSuperseded,
	).Scan(&count)
	return count, err
}

// CountLaunchAttemptsInCampaign returns the number of cloud-associated attempts
// for a job within a specific campaign. This prevents attempts from earlier
// campaigns (e.g. a previous `launch` command) from counting against the retry
// budget of the current campaign.
func CountLaunchAttemptsInCampaign(database *sql.DB, jobID int64, campaignID int64) (int, error) {
	var count int
	err := database.QueryRow(
		`SELECT COUNT(*) FROM job_attempts ja
		 JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.job_id = ?
		   AND ja.launch_id IS NOT NULL
		   AND l.campaign_id = ?
		   AND COALESCE(ja.cloud_outcome, '') != ?`,
		jobID, campaignID, AttemptOutcomeSuperseded,
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
	rows, err := database.Query(
		`SELECT job_id, cloud_outcome FROM job_attempts
		 WHERE launch_id = ? AND cloud_outcome IS NOT NULL
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

// SetLaunchGracePeriod stores the configured grace period for a cloud instance.
func SetLaunchGracePeriod(db *sql.DB, id int64, seconds int) error {
	_, err := db.Exec(`UPDATE launches SET grace_period_seconds = ? WHERE id = ?`, seconds, id)
	return err
}

// SetLaunchGraceStarted transitions an instance to grace status with a deadline.
func SetLaunchGraceStarted(db *sql.DB, id int64, deadline int64) error {
	now := time.Now().Unix()
	_, err := db.Exec(
		`UPDATE launches SET status = ?, grace_started_at = ?, grace_deadline = ? WHERE id = ?`,
		LaunchStatusGrace, now, deadline, id,
	)
	return err
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

// ListRunningLaunches returns all cloud instances with "running", "launching", or "grace" status.
func ListRunningLaunches(database *sql.DB) ([]*Launch, error) {
	rows, err := database.Query(
		`SELECT `+launchSelectColumns+` FROM launches WHERE status IN (?, ?, ?) ORDER BY created_at DESC`,
		LaunchStatusRunning, LaunchStatusLaunching, LaunchStatusGrace,
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
	if state.PhaseChangedAt == nil {
		var oldPhase sql.NullString
		_ = database.QueryRow(`SELECT instance_phase FROM launch_live_state WHERE launch_id = ?`, state.LaunchID).Scan(&oldPhase)
		if state.InstancePhase != oldPhase.String {
			state.PhaseChangedAt = &now
		}
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
	return state.PhaseChangedAt, err
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

// DeleteLaunchLiveState removes the ephemeral state for a terminal instance.
func DeleteLaunchLiveState(database *sql.DB, launchID int64) error {
	_, err := database.Exec(`DELETE FROM launch_live_state WHERE launch_id = ?`, launchID)
	return err
}

// MarkOpslogSynced records a successful opslog fetch for a launch.
func MarkOpslogSynced(database *sql.DB, launchID int64) error {
	_, err := database.Exec(
		`UPDATE launches SET oplog_synced_at = ?, oplog_not_found = 0 WHERE id = ?`,
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

// ListLaunchIDsNeedingOpslogSync returns IDs of launches that need opslog sync.
// Includes running/grace launches and recently-terminal launches, excluding those
// where oplog_not_found=1 and the check was performed at least 10 minutes after
// the instance terminated.
func ListLaunchIDsNeedingOpslogSync(database *sql.DB, since time.Duration) ([]int64, error) {
	cutoff := time.Now().Add(-since).Unix()
	const safetyBuffer = 600 // 10 minutes
	rows, err := database.Query(`
		SELECT id FROM launches
		WHERE provider_instance_id != ''
		AND (
			-- Running/grace instances always need sync
			status IN (?, ?)
			OR (
				-- Recently terminal instances, excluding known-not-found
				status IN (?, ?, ?)
				AND ended_at >= ?
				AND NOT (
					oplog_not_found = 1
					AND oplog_synced_at >= IFNULL(ended_at, 0) + ?
				)
			)
		)
		ORDER BY id`,
		LaunchStatusRunning, LaunchStatusGrace,
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
