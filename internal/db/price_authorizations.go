package db

import (
	"database/sql"
	"fmt"
	"time"
)

// ClassPriceAuthorization is a sticky user-granted approval to spend up to
// `UpToCents` per hour for any future job matching (gpu_class, gpu_mem_gb).
// The CLI surface that creates these is `weft job authorize-price --for-class`.
type ClassPriceAuthorization struct {
	ID        int64
	GPUClass  string
	GPUMemGB  int
	UpToCents int
	CreatedAt time.Time
	CreatedBy string
	Note      string
}

// GetJobPriceAuthorization returns the per-job ceiling (in cents/hour) that
// the user has authorized for this specific job, or (0, false, nil) if no
// override is set.
func GetJobPriceAuthorization(database *sql.DB, jobID int64) (int, bool, error) {
	var cents sql.NullInt64
	err := database.QueryRow(`SELECT price_authorized_up_to_cents FROM jobs WHERE id = ?`, jobID).Scan(&cents)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !cents.Valid {
		return 0, false, nil
	}
	return int(cents.Int64), true, nil
}

// SetJobPriceAuthorization stores a per-job ceiling. Zero or negative input
// clears the override (so the class-level fallback or anchor gate decides).
func SetJobPriceAuthorization(database *sql.DB, jobID int64, cents int) error {
	if cents <= 0 {
		return ClearJobPriceAuthorization(database, jobID)
	}
	_, err := database.Exec(
		`UPDATE jobs SET price_authorized_up_to_cents = ? WHERE id = ?`,
		cents, jobID,
	)
	return err
}

// ClearJobPriceAuthorization removes any per-job ceiling override.
func ClearJobPriceAuthorization(database *sql.DB, jobID int64) error {
	_, err := database.Exec(
		`UPDATE jobs SET price_authorized_up_to_cents = NULL WHERE id = ?`,
		jobID,
	)
	return err
}

// GetClassPriceAuthorization returns the class-level ceiling for the bucket,
// or (0, false, nil) if none is set.
func GetClassPriceAuthorization(database *sql.DB, gpuClass string, gpuMemGB int) (int, bool, error) {
	var cents sql.NullInt64
	err := database.QueryRow(
		`SELECT up_to_cents FROM price_authorizations
		  WHERE gpu_class = ? AND gpu_mem_gb = ?`,
		gpuClass, gpuMemGB,
	).Scan(&cents)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !cents.Valid {
		return 0, false, nil
	}
	return int(cents.Int64), true, nil
}

// UpsertClassPriceAuthorization sets (or replaces) a sticky class-level
// ceiling. Per the UNIQUE constraint on (gpu_class, gpu_mem_gb), a new
// authorization for the same bucket overwrites the prior one.
func UpsertClassPriceAuthorization(database *sql.DB, gpuClass string, gpuMemGB, upToCents int, createdBy, note string) error {
	if gpuClass == "" || gpuMemGB <= 0 || upToCents <= 0 {
		return fmt.Errorf("invalid class authorization: class=%q mem=%d cents=%d", gpuClass, gpuMemGB, upToCents)
	}
	_, err := database.Exec(
		`INSERT INTO price_authorizations (gpu_class, gpu_mem_gb, up_to_cents, created_at, created_by, note)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(gpu_class, gpu_mem_gb) DO UPDATE SET
		     up_to_cents = excluded.up_to_cents,
		     created_at  = excluded.created_at,
		     created_by  = excluded.created_by,
		     note        = excluded.note`,
		gpuClass, gpuMemGB, upToCents, time.Now().Unix(), createdBy, note,
	)
	return err
}

// DeleteClassPriceAuthorization removes a class-level ceiling.
func DeleteClassPriceAuthorization(database *sql.DB, gpuClass string, gpuMemGB int) error {
	_, err := database.Exec(
		`DELETE FROM price_authorizations WHERE gpu_class = ? AND gpu_mem_gb = ?`,
		gpuClass, gpuMemGB,
	)
	return err
}

// ListClassPriceAuthorizations returns every sticky class-level authorization,
// ordered by creation time descending.
func ListClassPriceAuthorizations(database *sql.DB) ([]ClassPriceAuthorization, error) {
	rows, err := database.Query(
		`SELECT id, gpu_class, gpu_mem_gb, up_to_cents, created_at,
		        COALESCE(created_by, ''), COALESCE(note, '')
		   FROM price_authorizations
		  ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClassPriceAuthorization
	for rows.Next() {
		var a ClassPriceAuthorization
		var createdAt int64
		if err := rows.Scan(&a.ID, &a.GPUClass, &a.GPUMemGB, &a.UpToCents, &createdAt, &a.CreatedBy, &a.Note); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}
