package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JobPayload is an immutable, logical-job-scoped input artifact. Unlike an
// output Artifact, it has no attempt ID and latest-attempt selection never
// changes which bytes a payload name denotes.
type JobPayload struct {
	JobID      int64
	Name       string
	StoredPath string
	SizeBytes  int64
	SHA256     string
	R2Key      string
	CreatedAt  int64
}

var ErrJobPayloadNotFound = errors.New("job payload not found")

// InsertJobPayload associates already-captured immutable bytes with a job.
// Callers normally pass the admission transaction so the job and every
// promised payload become visible together.
func InsertJobPayload(database dbExecer, payload JobPayload) error {
	if payload.JobID <= 0 || payload.Name == "" || payload.StoredPath == "" || payload.SHA256 == "" || payload.R2Key == "" {
		return fmt.Errorf("job payload: incomplete metadata")
	}
	if payload.CreatedAt == 0 {
		payload.CreatedAt = time.Now().Unix()
	}
	_, err := database.Exec(`
		INSERT INTO job_payloads (job_id, name, stored_path, size_bytes, sha256, r2_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, payload.JobID, payload.Name, payload.StoredPath, payload.SizeBytes, payload.SHA256, payload.R2Key, payload.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert payload %q for job %d: %w", payload.Name, payload.JobID, err)
	}
	return nil
}

type payloadQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func ListJobPayloads(database payloadQuerier, jobID int64) ([]JobPayload, error) {
	rows, err := database.Query(`
		SELECT job_id, name, stored_path, size_bytes, sha256, r2_key, created_at
		FROM job_payloads WHERE job_id = ? ORDER BY name
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobPayload
	for rows.Next() {
		var payload JobPayload
		if err := rows.Scan(&payload.JobID, &payload.Name, &payload.StoredPath, &payload.SizeBytes, &payload.SHA256, &payload.R2Key, &payload.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, payload)
	}
	return out, rows.Err()
}

func GetJobPayload(database dbExecer, jobID int64, name string) (*JobPayload, error) {
	var payload JobPayload
	err := database.QueryRow(`
		SELECT job_id, name, stored_path, size_bytes, sha256, r2_key, created_at
		FROM job_payloads WHERE job_id = ? AND name = ?
	`, jobID, name).Scan(&payload.JobID, &payload.Name, &payload.StoredPath, &payload.SizeBytes, &payload.SHA256, &payload.R2Key, &payload.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobPayloadNotFound
	}
	if err != nil {
		return nil, err
	}
	return &payload, nil
}
