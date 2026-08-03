package db

import (
	"database/sql"
	"strings"
	"time"
)

// Artifact represents a cached artifact for a job.
type Artifact struct {
	ID         int64
	JobID      int64
	JobRunID   *int64
	Name       string
	Path       string
	StoredPath string
	SizeBytes  int64
	SHA256     string
	CreatedAt  int64
}

// UpsertArtifact inserts or updates an artifact entry.
func UpsertArtifact(db *sql.DB, art Artifact) error {
	now := time.Now().Unix()
	if art.CreatedAt == 0 {
		art.CreatedAt = now
	}
	// A non-positive run id names no attempt row; fall back to latest-run
	// attribution (spec: ArtifactRetrievalCoversAllRuns in job-lifecycle.allium).
	if art.JobRunID != nil && *art.JobRunID <= 0 {
		art.JobRunID = nil
	}
	if art.JobRunID == nil {
		runID, err := latestRunIDForJob(db, art.JobID)
		if err != nil {
			return err
		}
		art.JobRunID = runID
	}

	if art.JobRunID != nil {
		var existingID int64
		err := db.QueryRow(
			`SELECT id FROM artifacts WHERE attempt_id = ? AND name = ? AND path = ?`,
			*art.JobRunID, art.Name, art.Path,
		).Scan(&existingID)
		switch err {
		case nil:
			_, err = db.Exec(
				`UPDATE artifacts
				 SET job_id = ?, stored_path = ?, size_bytes = ?, sha256 = ?, created_at = ?
				 WHERE id = ?`,
				art.JobID, art.StoredPath, art.SizeBytes, art.SHA256, art.CreatedAt, existingID,
			)
			return err
		case sql.ErrNoRows:
			_, err = db.Exec(
				`INSERT INTO artifacts (job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				art.JobID, *art.JobRunID, art.Name, art.Path, art.StoredPath, art.SizeBytes, art.SHA256, art.CreatedAt,
			)
			return err
		default:
			return err
		}
	}

	var existingID int64
	err := db.QueryRow(
		`SELECT id FROM artifacts WHERE job_id = ? AND attempt_id IS NULL AND name = ? AND path = ?`,
		art.JobID, art.Name, art.Path,
	).Scan(&existingID)
	switch err {
	case nil:
		_, err = db.Exec(
			`UPDATE artifacts
			 SET stored_path = ?, size_bytes = ?, sha256 = ?, created_at = ?
			 WHERE id = ?`,
			art.StoredPath, art.SizeBytes, art.SHA256, art.CreatedAt, existingID,
		)
		return err
	case sql.ErrNoRows:
		_, err = db.Exec(
			`INSERT INTO artifacts (job_id, name, path, stored_path, size_bytes, sha256, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			art.JobID, art.Name, art.Path, art.StoredPath, art.SizeBytes, art.SHA256, art.CreatedAt,
		)
		return err
	default:
		return err
	}
}

// ListArtifactsByJob returns cached artifacts for a job.
func ListArtifactsByJob(db *sql.DB, jobID int64) ([]Artifact, error) {
	if runID, err := latestRunIDForJob(db, jobID); err == nil && runID != nil {
		arts, err := ListArtifactsByRun(db, *runID)
		if err != nil {
			return nil, err
		}
		if len(arts) > 0 {
			return arts, nil
		}
	}

	rows, err := db.Query(
		`SELECT id, job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at
		FROM artifacts WHERE job_id = ? ORDER BY id ASC`,
		jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []Artifact
	for rows.Next() {
		var art Artifact
		var jobRunID sql.NullInt64
		if err := rows.Scan(&art.ID, &art.JobID, &jobRunID, &art.Name, &art.Path, &art.StoredPath, &art.SizeBytes, &art.SHA256, &art.CreatedAt); err != nil {
			return nil, err
		}
		if jobRunID.Valid {
			art.JobRunID = &jobRunID.Int64
		}
		artifacts = append(artifacts, art)
	}
	return artifacts, rows.Err()
}

// ListArtifactsByRun returns cached artifacts for a specific execution attempt.
func ListArtifactsByRun(db *sql.DB, runID int64) ([]Artifact, error) {
	rows, err := db.Query(
		`SELECT id, job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at
		 FROM artifacts WHERE attempt_id = ? ORDER BY id ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []Artifact
	for rows.Next() {
		var art Artifact
		var jobRunID sql.NullInt64
		if err := rows.Scan(&art.ID, &art.JobID, &jobRunID, &art.Name, &art.Path, &art.StoredPath, &art.SizeBytes, &art.SHA256, &art.CreatedAt); err != nil {
			return nil, err
		}
		if jobRunID.Valid {
			art.JobRunID = &jobRunID.Int64
		}
		artifacts = append(artifacts, art)
	}
	return artifacts, rows.Err()
}

// FindArtifactByNameOrPath locates a cached artifact by name or path: an
// exact match on name or full path first, then a path-suffix match at a '/'
// boundary (covering both a bare basename and a multi-segment suffix like
// "data/exports/foo.jsonl").
func FindArtifactByNameOrPath(db *sql.DB, jobID int64, token string) (*Artifact, error) {
	if runID, err := latestRunIDForJob(db, jobID); err == nil && runID != nil {
		art, err := findArtifactWithPatterns(db, `attempt_id = ?`, *runID, token)
		if err == nil {
			return art, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}

	return findArtifactWithPatterns(db, `job_id = ?`, jobID, token)
}

// escapeLikePattern escapes SQL LIKE metacharacters in a user token so a
// literal '%' or '_' in an artifact name cannot match unintended rows. Use
// with `ESCAPE '\'`.
func escapeLikePattern(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// findArtifactWithPatterns searches for an artifact using exact match, then
// '/'-boundary suffix match. The scopeClause should be "attempt_id = ?" or
// "job_id = ?", with scopeVal being the corresponding ID.
func findArtifactWithPatterns(db *sql.DB, scopeClause string, scopeVal int64, token string) (*Artifact, error) {
	art, err := scanOneArtifact(db,
		`SELECT id, job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at
		 FROM artifacts WHERE `+scopeClause+` AND (name = ? OR path = ?)
		 ORDER BY id ASC LIMIT 1`,
		scopeVal, token, token,
	)
	if err == nil {
		return art, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	return scanOneArtifact(db,
		`SELECT id, job_id, attempt_id, name, path, stored_path, size_bytes, sha256, created_at
		 FROM artifacts WHERE `+scopeClause+` AND path LIKE ? ESCAPE '\'
		 ORDER BY id ASC LIMIT 1`,
		scopeVal, "%/"+escapeLikePattern(token),
	)
}

func scanOneArtifact(db *sql.DB, query string, args ...any) (*Artifact, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	var art Artifact
	var jobRunID sql.NullInt64
	if err := rows.Scan(&art.ID, &art.JobID, &jobRunID, &art.Name, &art.Path, &art.StoredPath, &art.SizeBytes, &art.SHA256, &art.CreatedAt); err != nil {
		return nil, err
	}
	if jobRunID.Valid {
		art.JobRunID = &jobRunID.Int64
	}
	return &art, nil
}

func latestRunIDForJob(db *sql.DB, jobID int64) (*int64, error) {
	var runID sql.NullInt64
	if err := db.QueryRow(`SELECT latest_run_id FROM job_status WHERE id = ?`, jobID).Scan(&runID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !runID.Valid {
		return nil, nil
	}
	return &runID.Int64, nil
}
