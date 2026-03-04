package db

import (
	"database/sql"
	"time"
)

// Campaign status constants
const (
	CampaignStatusPlanned   = "planned"
	CampaignStatusLaunching = "launching"
	CampaignStatusRunning   = "running"
	CampaignStatusCompleted = "completed"
	CampaignStatusFailed    = "failed"
	CampaignStatusCancelled = "cancelled"
)

// Campaign represents a batch cloud GPU launch grouping multiple jobs.
type Campaign struct {
	ID               int64
	Status           string
	Provider         string
	GPUSpec          string
	GPUClass         string
	GPUMemGB         int
	InstanceID       string
	MaxSpendCents    int
	MaxTimeSeconds   int
	ActualSpendCents int
	CreatedAt        int64
	LaunchedAt       *int64
	EndedAt          *int64
}

// CreateCampaign inserts a new campaign record and returns its ID.
func CreateCampaign(db *sql.DB, c *Campaign) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO campaigns (status, provider, gpu_spec, gpu_class, gpu_mem_gb, max_spend_cents, max_time_seconds, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Status, c.Provider, c.GPUSpec, c.GPUClass, c.GPUMemGB,
		c.MaxSpendCents, c.MaxTimeSeconds, now,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetCampaign retrieves a campaign by ID.
func GetCampaign(db *sql.DB, id int64) (*Campaign, error) {
	row := db.QueryRow(
		`SELECT id, status, provider, gpu_spec, gpu_class, gpu_mem_gb, instance_id,
		        max_spend_cents, max_time_seconds, actual_spend_cents,
		        created_at, launched_at, ended_at
		 FROM campaigns WHERE id = ?`, id,
	)
	c, err := scanCampaignFrom(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// ListCampaigns returns all campaigns ordered by creation time descending.
func ListCampaigns(db *sql.DB) ([]*Campaign, error) {
	rows, err := db.Query(
		`SELECT id, status, provider, gpu_spec, gpu_class, gpu_mem_gb, instance_id,
		        max_spend_cents, max_time_seconds, actual_spend_cents,
		        created_at, launched_at, ended_at
		 FROM campaigns ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var campaigns []*Campaign
	for rows.Next() {
		c, err := scanCampaignFrom(rows)
		if err != nil {
			return nil, err
		}
		campaigns = append(campaigns, c)
	}
	return campaigns, rows.Err()
}

// UpdateCampaignStatus updates a campaign's status and optionally sets timestamps.
func UpdateCampaignStatus(db *sql.DB, id int64, status string) error {
	now := time.Now().Unix()
	switch status {
	case CampaignStatusRunning:
		_, err := db.Exec(`UPDATE campaigns SET status = ?, launched_at = ? WHERE id = ?`, status, now, id)
		return err
	case CampaignStatusCompleted, CampaignStatusFailed, CampaignStatusCancelled:
		_, err := db.Exec(`UPDATE campaigns SET status = ?, ended_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := db.Exec(`UPDATE campaigns SET status = ? WHERE id = ?`, status, id)
		return err
	}
}

// SetCampaignInstanceID sets the cloud instance ID for a campaign.
func SetCampaignInstanceID(db *sql.DB, id int64, instanceID string) error {
	_, err := db.Exec(`UPDATE campaigns SET instance_id = ? WHERE id = ?`, instanceID, id)
	return err
}

// SetCampaignActualSpend updates the actual spend in cents.
func SetCampaignActualSpend(db *sql.DB, id int64, cents int) error {
	_, err := db.Exec(`UPDATE campaigns SET actual_spend_cents = ? WHERE id = ?`, cents, id)
	return err
}

// SetJobCampaignID associates a job with a campaign.
func SetJobCampaignID(db *sql.DB, jobID, campaignID int64) error {
	_, err := db.Exec(`UPDATE jobs SET campaign_id = ? WHERE id = ?`, campaignID, jobID)
	return err
}

// GetCampaignJobs returns all jobs associated with a campaign.
func GetCampaignJobs(db *sql.DB, campaignID int64) ([]*Job, error) {
	query := "SELECT " + jobSelectColumns + " FROM jobs WHERE campaign_id = ? AND tombstoned = 0 ORDER BY id ASC"
	return queryJobs(db, query, campaignID)
}

// ListNeedsRentalJobs returns all jobs with needs_rental status.
func ListNeedsRentalJobs(db *sql.DB) ([]*Job, error) {
	query := "SELECT " + jobSelectColumns + " FROM jobs WHERE status = ? AND tombstoned = 0 ORDER BY id ASC"
	return queryJobs(db, query, StatusNeedsRental)
}

// GetCampaignJobCounts returns a map from campaign ID to job count.
func GetCampaignJobCounts(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT campaign_id, COUNT(*) FROM jobs WHERE campaign_id IS NOT NULL AND tombstoned = 0 GROUP BY campaign_id`)
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

// campaignScanner is implemented by both *sql.Row and *sql.Rows.
type campaignScanner interface {
	Scan(dest ...any) error
}

func scanCampaignFrom(s campaignScanner) (*Campaign, error) {
	var c Campaign
	var gpuSpec, gpuClass, instanceID sql.NullString
	var gpuMemGB, maxSpend, maxTime, actualSpend sql.NullInt64
	var launchedAt, endedAt sql.NullInt64

	err := s.Scan(
		&c.ID, &c.Status, &c.Provider, &gpuSpec, &gpuClass, &gpuMemGB,
		&instanceID, &maxSpend, &maxTime, &actualSpend,
		&c.CreatedAt, &launchedAt, &endedAt,
	)
	if err != nil {
		return nil, err
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
	if instanceID.Valid {
		c.InstanceID = instanceID.String
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
	if launchedAt.Valid {
		c.LaunchedAt = &launchedAt.Int64
	}
	if endedAt.Valid {
		c.EndedAt = &endedAt.Int64
	}
	return &c, nil
}
