package db

import (
	"database/sql"
	"fmt"
)

// ProjectSpend summarizes prorated run spend for a project since a cutoff.
type ProjectSpend struct {
	Project  string
	SpentUSD float64
	RunCount int
}

// ListProjectSpendSince returns per-project spend after sinceUnix.
//
// Spend is prorated by overlap with the [sinceUnix, end_time] interval for
// runs that started before the cutoff and ended after it.
func ListProjectSpendSince(database *sql.DB, sinceUnix int64) ([]ProjectSpend, error) {
	query := `
		SELECT
			COALESCE(NULLIF(project, ''), '(none)') AS project_name,
			SUM(
				CASE
					WHEN cost IS NULL OR cost <= 0 THEN 0
					WHEN start_time IS NULL OR end_time IS NULL THEN 0
					WHEN end_time <= ? THEN 0
					WHEN end_time <= start_time THEN 0
					WHEN start_time >= ? THEN cost
					ELSE cost * (CAST(end_time - ? AS REAL) / CAST(end_time - start_time AS REAL))
				END
			) AS spent_usd,
			SUM(
				CASE
					WHEN cost IS NULL OR cost <= 0 THEN 0
					WHEN start_time IS NULL OR end_time IS NULL THEN 0
					WHEN end_time <= ? THEN 0
					WHEN end_time <= start_time THEN 0
					ELSE 1
				END
			) AS run_count
		FROM training_examples
		WHERE end_time > ?
		GROUP BY COALESCE(NULLIF(project, ''), '(none)')
		HAVING SUM(
			CASE
				WHEN cost IS NULL OR cost <= 0 THEN 0
				WHEN start_time IS NULL OR end_time IS NULL THEN 0
				WHEN end_time <= ? THEN 0
				WHEN end_time <= start_time THEN 0
				WHEN start_time >= ? THEN cost
				ELSE cost * (CAST(end_time - ? AS REAL) / CAST(end_time - start_time AS REAL))
			END
		) > 0
		ORDER BY spent_usd DESC, project_name ASC
	`

	rows, err := database.Query(query,
		sinceUnix, sinceUnix, sinceUnix,
		sinceUnix, sinceUnix,
		sinceUnix, sinceUnix, sinceUnix,
	)
	if err != nil {
		return nil, fmt.Errorf("query project spend: %w", err)
	}
	defer rows.Close()

	var out []ProjectSpend
	for rows.Next() {
		var row ProjectSpend
		if err := rows.Scan(&row.Project, &row.SpentUSD, &row.RunCount); err != nil {
			return nil, fmt.Errorf("scan project spend: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project spend rows: %w", err)
	}
	return out, nil
}
