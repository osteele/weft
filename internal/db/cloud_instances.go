package db

import (
	"database/sql"
	"fmt"
	"time"
)

// cloudInstanceSelectColumns is the column list for SELECT queries on cloud_instances.
const cloudInstanceSelectColumns = `id, campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb, vastai_instance_id,
		max_spend_cents, max_time_seconds, actual_spend_cents,
		created_at, ready_at, launched_at, ended_at,
		resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		inet_down_mbps, inet_up_mbps, cuda_version,
		provider_instance_id, data_center,
		instance_role, donor_instance_id, seed_download_secs, seed_copy_secs,
		grace_period_seconds, grace_started_at, grace_deadline`

// CloudInstance status constants (same values used for both CloudInstance and Campaign).
const (
	CloudInstanceStatusPlanned   = "planned"
	CloudInstanceStatusLaunching = "launching"
	CloudInstanceStatusRunning   = "running"
	CloudInstanceStatusGrace     = "grace"
	CloudInstanceStatusCompleted = "completed"
	CloudInstanceStatusFailed    = "failed"
	CloudInstanceStatusCancelled = "cancelled"
)

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

	// Offer metadata (captured at launch)
	ResolvedGPUName  string
	CostPerHourCents int
	NumGPUs          int
	DLPerf           float64
	Reliability      float64
	InetDownMbps     float64
	InetUpMbps       float64
	CUDAVersion      float64
}

// EffectiveProviderID returns ProviderInstanceID, falling back to VastaiInstanceID for legacy records.
func (c *CloudInstance) EffectiveProviderID() string {
	if c.ProviderInstanceID != "" {
		return c.ProviderInstanceID
	}
	return c.VastaiInstanceID
}

// CreateCloudInstance inserts a new cloud instance record and returns its ID.
func CreateCloudInstance(db *sql.DB, c *CloudInstance) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO cloud_instances (campaign_id, status, provider, gpu_spec, gpu_class, gpu_mem_gb,
		 max_spend_cents, max_time_seconds, created_at,
		 resolved_gpu_name, cost_per_hour_cents, num_gpus, dl_perf, reliability,
		 inet_down_mbps, inet_up_mbps, cuda_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CampaignID, c.Status, c.Provider, c.GPUSpec, c.GPUClass, c.GPUMemGB,
		c.MaxSpendCents, c.MaxTimeSeconds, now,
		c.ResolvedGPUName, c.CostPerHourCents, c.NumGPUs, c.DLPerf, c.Reliability,
		c.InetDownMbps, c.InetUpMbps, c.CUDAVersion,
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
func UpdateCloudInstanceStatus(db *sql.DB, id int64, status string) error {
	now := time.Now().Unix()
	switch status {
	case CloudInstanceStatusRunning:
		_, err := db.Exec(`UPDATE cloud_instances SET status = ?, launched_at = ? WHERE id = ?`, status, now, id)
		return err
	case CloudInstanceStatusCompleted, CloudInstanceStatusFailed, CloudInstanceStatusCancelled:
		_, err := db.Exec(`UPDATE cloud_instances SET status = ?, ended_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := db.Exec(`UPDATE cloud_instances SET status = ? WHERE id = ?`, status, id)
		return err
	}
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
func CloudInstanceHost(instanceID int64) string {
	return fmt.Sprintf("vastai:%d", instanceID)
}

// SetJobCloudInstanceID associates a job with a cloud instance and records the attempt.
// Also sets the job's host to "vastai:<instanceID>" so cloud jobs are visible in host-based views.
func SetJobCloudInstanceID(db *sql.DB, jobID, instanceID int64) error {
	_, err := db.Exec(`UPDATE jobs SET cloud_instance_id = ?, host = ? WHERE id = ?`, instanceID, CloudInstanceHost(instanceID), jobID)
	if err != nil {
		return err
	}
	return InsertJobCloudAttempt(db, jobID, instanceID)
}

// SetJobCampaignIndex sets the 0-based position of a job within its campaign sequence.
func SetJobCampaignIndex(db *sql.DB, jobID int64, index int) error {
	_, err := db.Exec(`UPDATE jobs SET campaign_job_index = ? WHERE id = ?`, index, jobID)
	return err
}

// GetCloudInstanceJobs returns all jobs associated with a cloud instance.
func GetCloudInstanceJobs(db *sql.DB, instanceID int64) ([]*Job, error) {
	query := "SELECT " + jobSelectColumns + " FROM jobs WHERE cloud_instance_id = ? AND tombstoned = 0 ORDER BY id ASC"
	return queryJobs(db, query, instanceID)
}

// ListNeedsRentalJobs returns all jobs with needs_rental status.
func ListNeedsRentalJobs(db *sql.DB) ([]*Job, error) {
	query := "SELECT " + jobSelectColumns + " FROM jobs WHERE status = ? AND tombstoned = 0 ORDER BY id ASC"
	return queryJobs(db, query, StatusNeedsRental)
}

// PromoteNeedsRentalToQueued atomically promotes a needs_rental job to queued
// with a host assignment. Returns true if the job was updated (false if it was
// already claimed or changed status).
func PromoteNeedsRentalToQueued(database *sql.DB, jobID int64, host string) (bool, error) {
	result, err := database.Exec(
		`UPDATE jobs SET status = ?, host = ? WHERE id = ? AND status = ? AND tombstoned = 0`,
		StatusQueued, host, jobID, StatusNeedsRental,
	)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ResetCloudInstanceJobs resets non-terminal jobs in a cloud instance back to needs_rental
// and clears their instance association. Records the attempt outcome before resetting.
// Returns the number of jobs reset.
func ResetCloudInstanceJobs(database *sql.DB, instanceID int64, outcome string) (int64, error) {
	// Close open attempts for all non-terminal jobs on this instance
	if err := CloseJobCloudAttemptsByInstance(database, instanceID, outcome); err != nil {
		return 0, fmt.Errorf("close attempts: %w", err)
	}

	result, err := database.Exec(
		`UPDATE jobs SET status = ?, cloud_instance_id = NULL, host = ''
		 WHERE cloud_instance_id = ? AND status NOT IN (?, ?) AND tombstoned = 0`,
		StatusNeedsRental, instanceID, StatusCompleted, StatusFailed,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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

	err := s.Scan(
		&c.ID, &campaignID, &c.Status, &c.Provider, &gpuSpec, &gpuClass, &gpuMemGB,
		&vastaiInstanceID, &maxSpend, &maxTime, &actualSpend,
		&c.CreatedAt, &readyAt, &launchedAt, &endedAt,
		&resolvedGPUName, &costPerHourCents, &numGPUs, &dlPerf, &reliability,
		&inetDown, &inetUp, &cudaVersion,
		&providerInstanceID, &dataCenter,
		&instanceRole, &donorInstanceID, &seedDownloadSecs, &seedCopySecs,
		&gracePeriodSeconds, &graceStartedAt, &graceDeadline,
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
	return &c, nil
}

// Job cloud attempt outcome constants.
const (
	AttemptOutcomeCompleted = "completed"
	AttemptOutcomeFailed    = "failed"
	AttemptOutcomeCancelled = "cancelled"
	AttemptOutcomeOrphaned  = "orphaned"
)

// JobCloudAttempt records a single association between a job and a cloud instance.
type JobCloudAttempt struct {
	ID              int64
	JobID           int64
	CloudInstanceID int64
	StartedAt       int64
	EndedAt         *int64
	Outcome         string // "completed", "failed", "cancelled", "orphaned"
}

// InsertJobCloudAttempt records a new job ↔ cloud instance association.
func InsertJobCloudAttempt(database *sql.DB, jobID, cloudInstanceID int64) error {
	now := time.Now().Unix()
	_, err := database.Exec(
		`INSERT INTO job_cloud_attempts (job_id, cloud_instance_id, started_at) VALUES (?, ?, ?)`,
		jobID, cloudInstanceID, now,
	)
	return err
}

// CloseJobCloudAttempt closes the open attempt for a specific job with the given outcome.
func CloseJobCloudAttempt(database *sql.DB, jobID int64, outcome string) error {
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE job_cloud_attempts SET ended_at = ?, outcome = ? WHERE job_id = ? AND ended_at IS NULL`,
		now, outcome, jobID,
	)
	return err
}

// CloseJobCloudAttemptsByInstance closes all open attempts for jobs on the given instance.
func CloseJobCloudAttemptsByInstance(database *sql.DB, instanceID int64, outcome string) error {
	now := time.Now().Unix()
	_, err := database.Exec(
		`UPDATE job_cloud_attempts SET ended_at = ?, outcome = ?
		 WHERE cloud_instance_id = ? AND ended_at IS NULL`,
		now, outcome, instanceID,
	)
	return err
}

// GetJobCloudAttempts returns the attempt history for a job, ordered by start time.
func GetJobCloudAttempts(database *sql.DB, jobID int64) ([]JobCloudAttempt, error) {
	rows, err := database.Query(
		`SELECT id, job_id, cloud_instance_id, started_at, ended_at, outcome
		 FROM job_cloud_attempts WHERE job_id = ? ORDER BY started_at ASC`,
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
