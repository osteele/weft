package db

import (
	"database/sql"
	"time"
)

// Artifact represents a cached artifact for a job.
type Artifact struct {
	ID         int64
	JobID      int64
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
	_, err := db.Exec(
		`INSERT INTO artifacts (job_id, name, path, stored_path, size_bytes, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id, name, path) DO UPDATE SET
			stored_path = excluded.stored_path,
			size_bytes = excluded.size_bytes,
			sha256 = excluded.sha256,
			created_at = excluded.created_at`,
		art.JobID, art.Name, art.Path, art.StoredPath, art.SizeBytes, art.SHA256, art.CreatedAt,
	)
	return err
}

// ListArtifactsByJob returns cached artifacts for a job.
func ListArtifactsByJob(db *sql.DB, jobID int64) ([]Artifact, error) {
	rows, err := db.Query(
		`SELECT id, job_id, name, path, stored_path, size_bytes, sha256, created_at
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
		if err := rows.Scan(&art.ID, &art.JobID, &art.Name, &art.Path, &art.StoredPath, &art.SizeBytes, &art.SHA256, &art.CreatedAt); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}
	return artifacts, rows.Err()
}

// FindArtifactByNameOrPath locates a cached artifact by name or path.
func FindArtifactByNameOrPath(db *sql.DB, jobID int64, token string) (*Artifact, error) {
	rows, err := db.Query(
		`SELECT id, job_id, name, path, stored_path, size_bytes, sha256, created_at
		FROM artifacts WHERE job_id = ? AND (name = ? OR path = ?)
		ORDER BY id ASC`,
		jobID, token, token,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	var art Artifact
	if err := rows.Scan(&art.ID, &art.JobID, &art.Name, &art.Path, &art.StoredPath, &art.SizeBytes, &art.SHA256, &art.CreatedAt); err != nil {
		return nil, err
	}
	return &art, nil
}
