package db

import (
	"database/sql"
	"time"
)

// Campaign status constants.
const (
	CampaignStatusPlanned   = "planned"
	CampaignStatusLaunching = "launching"
	CampaignStatusRunning   = "running"
	CampaignStatusCompleted = "completed"
	CampaignStatusFailed    = "failed"
	CampaignStatusCancelled = "cancelled"
)

// Campaign represents a batch of cloud instances launched together.
type Campaign struct {
	ID                 int64
	Status             string
	CreatedAt          int64
	EndedAt            *int64
	EstimatedCostCents int // sum of per-instance cost estimates at launch time
}

// CreateCampaign inserts a new campaign batch record and returns its ID.
func CreateCampaign(db *sql.DB, c *Campaign) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO campaigns (status, created_at, estimated_cost_cents) VALUES (?, ?, ?)`,
		c.Status, now, c.EstimatedCostCents,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetCampaign retrieves a campaign by ID.
func GetCampaign(db *sql.DB, id int64) (*Campaign, error) {
	row := db.QueryRow(
		`SELECT id, status, created_at, ended_at, COALESCE(estimated_cost_cents, 0) FROM campaigns WHERE id = ?`, id,
	)
	var c Campaign
	var endedAt sql.NullInt64
	err := row.Scan(&c.ID, &c.Status, &c.CreatedAt, &endedAt, &c.EstimatedCostCents)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if endedAt.Valid {
		c.EndedAt = &endedAt.Int64
	}
	return &c, nil
}

// ListCampaigns returns all campaigns ordered by creation time descending.
func ListCampaigns(db *sql.DB) ([]*Campaign, error) {
	rows, err := db.Query(
		`SELECT id, status, created_at, ended_at, COALESCE(estimated_cost_cents, 0) FROM campaigns ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCampaigns(rows)
}

// ListActiveCampaigns returns campaigns that are not in a terminal status.
func ListActiveCampaigns(db *sql.DB) ([]*Campaign, error) {
	rows, err := db.Query(
		`SELECT id, status, created_at, ended_at, COALESCE(estimated_cost_cents, 0) FROM campaigns WHERE status NOT IN (?, ?, ?) ORDER BY created_at DESC`,
		CampaignStatusCompleted, CampaignStatusFailed, CampaignStatusCancelled,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCampaigns(rows)
}

func scanCampaigns(rows *sql.Rows) ([]*Campaign, error) {
	var campaigns []*Campaign
	for rows.Next() {
		var c Campaign
		var endedAt sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Status, &c.CreatedAt, &endedAt, &c.EstimatedCostCents); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			c.EndedAt = &endedAt.Int64
		}
		campaigns = append(campaigns, &c)
	}
	return campaigns, rows.Err()
}

// UpdateCampaignStatus updates a campaign's status and optionally sets timestamps.
func UpdateCampaignStatus(db *sql.DB, id int64, status string) error {
	now := time.Now().Unix()
	switch status {
	case CampaignStatusCompleted, CampaignStatusFailed, CampaignStatusCancelled:
		_, err := db.Exec(`UPDATE campaigns SET status = ?, ended_at = ? WHERE id = ?`, status, now, id)
		return err
	default:
		_, err := db.Exec(`UPDATE campaigns SET status = ? WHERE id = ?`, status, id)
		return err
	}
}
