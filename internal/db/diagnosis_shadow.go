package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// DiagnosisShadow is one recorded TypeSafe Jev judgment of a failed job,
// stored next to the regex diagnoses weft computed from the same log. It is
// measurement data: nothing reads it to act on a job.
//
// An empty pattern means that regex path produced no diagnosis. A nil
// RegexSupported / FatalSupported means the question was not asked because
// there was no pattern to ask about.
type DiagnosisShadow struct {
	JobID              int64           `json:"job_id"`
	AttemptID          *int64          `json:"attempt_id,omitempty"`
	JudgedAt           int64           `json:"judged_at"`
	Model              string          `json:"model"`
	DisplayPattern     string          `json:"display_pattern,omitempty"`
	RemediationPattern string          `json:"remediation_pattern,omitempty"`
	FatalPattern       string          `json:"fatal_pattern,omitempty"`
	JevCategory        string          `json:"jev_category"`
	JevConfidence      float64         `json:"jev_confidence"`
	JevProbabilities   json.RawMessage `json:"jev_probabilities"` // JSON object: category -> probability
	RegexSupported     *float64        `json:"regex_supported,omitempty"`
	FatalSupported     *float64        `json:"fatal_supported,omitempty"`
	LatencyMS          int64           `json:"latency_ms"`
	InputTokens        int             `json:"input_tokens"`
}

// InsertDiagnosisShadow stores one judgment. A job is judged at most once, so
// a second row for the same job is a conflict error rather than an overwrite.
func InsertDiagnosisShadow(db *sql.DB, row DiagnosisShadow) error {
	_, err := db.Exec(`INSERT INTO diagnosis_shadow (
		job_id, attempt_id, judged_at, model, display_pattern, remediation_pattern,
		fatal_pattern, jev_category, jev_confidence, jev_probabilities,
		regex_supported, fatal_supported, latency_ms, input_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.JobID, nullableInt64(row.AttemptID), row.JudgedAt, row.Model,
		nullIfEmpty(row.DisplayPattern), nullIfEmpty(row.RemediationPattern), nullIfEmpty(row.FatalPattern),
		row.JevCategory, row.JevConfidence, string(row.JevProbabilities),
		row.RegexSupported, row.FatalSupported, row.LatencyMS, row.InputTokens,
	)
	if err != nil {
		return fmt.Errorf("insert diagnosis_shadow for job %d: %w", row.JobID, err)
	}
	return nil
}

// ListDiagnosisShadowCandidates returns failed jobs that ended after
// endedAfter and have no diagnosis_shadow row, newest first. Failed means
// failed or dead, or completed with a non-zero exit — the predicate of
// ListRecentFailedUndiagnosed, without its retry-count and stored-diagnosis
// filters, since the shadow judges diagnosed jobs too. limit <= 0 means no
// limit.
func ListDiagnosisShadowCandidates(db *sql.DB, endedAfter time.Time, limit int) ([]*Job, error) {
	query := fmt.Sprintf(`SELECT %s FROM job_status
		WHERE tombstoned = 0
		AND end_time > ?
		AND (
			(status = ? AND exit_code IS NOT NULL AND exit_code != 0)
			OR status = ?
			OR status = ?
		)
		AND NOT EXISTS (SELECT 1 FROM diagnosis_shadow ds WHERE ds.job_id = job_status.id)
		ORDER BY end_time DESC`, jobSelectColumns)
	args := []any{endedAfter.Unix(), StatusCompleted, StatusFailed, StatusDead}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryJobs(db, query, args...)
}

// ListRecentDiagnosisShadows returns judgments made after judgedAfter,
// newest first.
func ListRecentDiagnosisShadows(db *sql.DB, judgedAfter time.Time) ([]DiagnosisShadow, error) {
	rows, err := db.Query(`SELECT job_id, attempt_id, judged_at, model, display_pattern,
		remediation_pattern, fatal_pattern, jev_category, jev_confidence, jev_probabilities,
		regex_supported, fatal_supported, latency_ms, input_tokens
		FROM diagnosis_shadow
		WHERE judged_at > ?
		ORDER BY judged_at DESC, job_id DESC`, judgedAfter.Unix())
	if err != nil {
		return nil, fmt.Errorf("list diagnosis_shadow: %w", err)
	}
	defer rows.Close()
	var out []DiagnosisShadow
	for rows.Next() {
		var (
			row                                          DiagnosisShadow
			attemptID, latency, inputTokens              sql.NullInt64
			model, display, remediation, fatal, category sql.NullString
			probabilities                                sql.NullString
			confidence, regexSupported, fatalSupported   sql.NullFloat64
		)
		if err := rows.Scan(&row.JobID, &attemptID, &row.JudgedAt, &model, &display,
			&remediation, &fatal, &category, &confidence, &probabilities,
			&regexSupported, &fatalSupported, &latency, &inputTokens); err != nil {
			return nil, fmt.Errorf("scan diagnosis_shadow: %w", err)
		}
		if attemptID.Valid {
			row.AttemptID = &attemptID.Int64
		}
		row.Model = model.String
		row.DisplayPattern = display.String
		row.RemediationPattern = remediation.String
		row.FatalPattern = fatal.String
		row.JevCategory = category.String
		row.JevConfidence = confidence.Float64
		if probabilities.Valid && probabilities.String != "" {
			row.JevProbabilities = json.RawMessage(probabilities.String)
		}
		if regexSupported.Valid {
			row.RegexSupported = &regexSupported.Float64
		}
		if fatalSupported.Valid {
			row.FatalSupported = &fatalSupported.Float64
		}
		row.LatencyMS = latency.Int64
		row.InputTokens = int(inputTokens.Int64)
		out = append(out, row)
	}
	return out, rows.Err()
}
