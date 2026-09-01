package db

import (
	"database/sql"
	"fmt"
)

type TelemetryArchiveCandidate struct {
	JobID             int64
	AttemptID         int64
	AttemptStatus     string
	TimeseriesSamples int64
	RichSamples       int64
}

func AttemptHasRawTelemetryRows(database *sql.DB, attemptID int64) (bool, error) {
	var present int
	err := database.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM job_timeseries WHERE attempt_id = ?)
		    OR EXISTS(SELECT 1 FROM job_telemetry_samples WHERE attempt_id = ?)`, attemptID, attemptID).Scan(&present)
	return present != 0, err
}

// ListTelemetryArchiveCandidates returns terminal attempts that still retain
// raw relational telemetry. Active attempts are deliberately excluded.
func ListTelemetryArchiveCandidates(database *sql.DB, limit int) ([]TelemetryArchiveCandidate, error) {
	rows, err := database.Query(`
		SELECT a.job_id, a.id, a.status,
		       (SELECT COUNT(*) FROM job_timeseries jt WHERE jt.attempt_id = a.id),
		       (SELECT COUNT(*) FROM job_telemetry_samples ts WHERE ts.attempt_id = a.id)
		  FROM job_attempts a
		 WHERE EXISTS (SELECT 1 FROM job_timeseries jt WHERE jt.attempt_id = a.id)
		    OR EXISTS (SELECT 1 FROM job_telemetry_samples ts WHERE ts.attempt_id = a.id)
		 ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("list telemetry archive candidates: %w", err)
	}
	defer rows.Close()
	var candidates []TelemetryArchiveCandidate
	for rows.Next() {
		var candidate TelemetryArchiveCandidate
		if err := rows.Scan(&candidate.JobID, &candidate.AttemptID, &candidate.AttemptStatus,
			&candidate.TimeseriesSamples, &candidate.RichSamples); err != nil {
			return nil, err
		}
		if !IsTerminalStatus(candidate.AttemptStatus) {
			continue
		}
		candidates = append(candidates, candidate)
		if limit > 0 && len(candidates) >= limit {
			break
		}
	}
	return candidates, rows.Err()
}

func Compact(database *sql.DB) error {
	if _, err := database.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint before compact: %w", err)
	}
	if _, err := database.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("compact database: %w", err)
	}
	return nil
}
