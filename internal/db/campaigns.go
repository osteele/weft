package db

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Campaign status constants.
const (
	CampaignStatusPlanned   = "planned"
	CampaignStatusLaunching = "launching"
	CampaignStatusRunning   = "running"
	CampaignStatusCompleted = "completed"
	CampaignStatusFailed    = "failed"
	CampaignStatusCancelled = "canceled"
)

// Campaign represents a batch of cloud instances launched together.
type Campaign struct {
	ID                 int64
	Status             string
	CreatedAt          int64
	EndedAt            *int64
	EstimatedCostCents int // sum of per-instance cost estimates at launch time
	DistinctMachines   bool
	AvoidMachines      []string
	AffinityMachines   []string
}

// CreateCampaign inserts a new campaign batch record and returns its ID.
func CreateCampaign(db *sql.DB, c *Campaign) (int64, error) {
	now := time.Now().Unix()
	avoidMachinesJSON, err := encodeStringSliceForDB(c.AvoidMachines)
	if err != nil {
		return 0, err
	}
	affinityMachinesJSON, err := encodeStringSliceForDB(c.AffinityMachines)
	if err != nil {
		return 0, err
	}
	result, err := db.Exec(
		`INSERT INTO campaigns (status, created_at, estimated_cost_cents, distinct_machines, avoid_machines, affinity_machines) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Status, now, c.EstimatedCostCents, boolToInt(c.DistinctMachines), avoidMachinesJSON, affinityMachinesJSON,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetCampaign retrieves a campaign by ID.
func GetCampaign(db *sql.DB, id int64) (*Campaign, error) {
	row := db.QueryRow(
		campaignSelectSQL()+` WHERE id = ?`, id,
	)
	c, err := scanCampaign(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListCampaigns returns all campaigns ordered by creation time descending.
func ListCampaigns(db *sql.DB) ([]*Campaign, error) {
	rows, err := db.Query(
		campaignSelectSQL() + ` ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCampaigns(rows)
}

// GetMostRecentCampaign returns the newest campaign, or nil if none exist.
func GetMostRecentCampaign(db *sql.DB) (*Campaign, error) {
	return getMostRecentCampaignByQuery(
		db,
		campaignSelectSQL()+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
	)
}

// GetMostRecentCampaignByStatus returns the newest campaign with the given status,
// or nil if none exist.
func GetMostRecentCampaignByStatus(db *sql.DB, status string) (*Campaign, error) {
	return getMostRecentCampaignByQuery(
		db,
		campaignSelectSQL()+`
		 WHERE status = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
		status,
	)
}

// ListActiveCampaigns returns campaigns that are not in a terminal status.
func ListActiveCampaigns(db *sql.DB) ([]*Campaign, error) {
	rows, err := db.Query(
		campaignSelectSQL()+` WHERE status NOT IN (?, ?, ?) ORDER BY created_at DESC`,
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
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		campaigns = append(campaigns, c)
	}
	return campaigns, rows.Err()
}

func getMostRecentCampaignByQuery(db *sql.DB, query string, args ...any) (*Campaign, error) {
	row := db.QueryRow(query, args...)
	c, err := scanCampaign(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

type campaignScanner interface {
	Scan(dest ...any) error
}

func campaignSelectSQL() string {
	return `SELECT id, status, created_at, ended_at, COALESCE(estimated_cost_cents, 0),
		       COALESCE(distinct_machines, 0), COALESCE(avoid_machines, ''), COALESCE(affinity_machines, '')
		  FROM campaigns`
}

func scanCampaign(row campaignScanner) (*Campaign, error) {
	var c Campaign
	var endedAt sql.NullInt64
	var distinct int
	var avoidMachinesJSON string
	var affinityMachinesJSON string
	err := row.Scan(&c.ID, &c.Status, &c.CreatedAt, &endedAt, &c.EstimatedCostCents, &distinct, &avoidMachinesJSON, &affinityMachinesJSON)
	if err != nil {
		return nil, err
	}
	if endedAt.Valid {
		c.EndedAt = &endedAt.Int64
	}
	c.DistinctMachines = distinct != 0
	c.AvoidMachines = decodeStringSliceFromDB(avoidMachinesJSON)
	c.AffinityMachines = decodeStringSliceFromDB(affinityMachinesJSON)
	return &c, nil
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

func encodeStringSliceForDB(values []string) (any, error) {
	cleaned := normalizeStringSlice(values)
	if len(cleaned) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func decodeStringSliceFromDB(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil
	}
	return normalizeStringSlice(out)
}

func normalizeStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
