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
	ID        int64
	Status    string
	CreatedAt int64
	EndedAt   *int64
}

// CreateCampaign inserts a new campaign batch record and returns its ID.
func CreateCampaign(db *sql.DB, c *Campaign) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec(
		`INSERT INTO campaigns (status, created_at) VALUES (?, ?)`,
		c.Status, now,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetCampaign retrieves a campaign by ID.
func GetCampaign(db *sql.DB, id int64) (*Campaign, error) {
	row := db.QueryRow(
		`SELECT id, status, created_at, ended_at FROM campaigns WHERE id = ?`, id,
	)
	var c Campaign
	var endedAt sql.NullInt64
	err := row.Scan(&c.ID, &c.Status, &c.CreatedAt, &endedAt)
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
		`SELECT id, status, created_at, ended_at FROM campaigns ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var campaigns []*Campaign
	for rows.Next() {
		var c Campaign
		var endedAt sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Status, &c.CreatedAt, &endedAt); err != nil {
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
