package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NamedAsset is a row in the named_assets table — a stable name pointing to
// a content-addressed blob in R2 under the assets/<content_hash> key.
type NamedAsset struct {
	ID          int64
	Name        string
	ContentHash string // sha256 hex
	SizeBytes   int64
	ContentType string
	TargetPath  string // workspace-relative path at consumer-job staging time
	SourceJobID *int64 // non-nil when minted from an existing job artifact
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrNamedAssetNotFound is returned when a name has no row in named_assets.
var ErrNamedAssetNotFound = errors.New("named asset not found")

// UpsertNamedAsset inserts or replaces a row keyed on name. Republishing the
// same name with new content overwrites content_hash and target_path; the
// caller is responsible for any R2-side cleanup of the prior blob.
func UpsertNamedAsset(db *sql.DB, a NamedAsset) error {
	if a.Name == "" {
		return fmt.Errorf("named asset: name is required")
	}
	if a.ContentHash == "" {
		return fmt.Errorf("named asset %q: content_hash is required", a.Name)
	}
	if a.TargetPath == "" {
		return fmt.Errorf("named asset %q: target_path is required", a.Name)
	}
	if a.ContentType == "" {
		a.ContentType = "file"
	}
	now := time.Now().Unix()
	created := a.CreatedAt.Unix()
	if created == 0 {
		created = now
	}
	_, err := db.Exec(`
		INSERT INTO named_assets (name, content_hash, size_bytes, content_type, target_path, source_job_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			content_hash = excluded.content_hash,
			size_bytes = excluded.size_bytes,
			content_type = excluded.content_type,
			target_path = excluded.target_path,
			source_job_id = excluded.source_job_id,
			updated_at = excluded.updated_at
	`, a.Name, a.ContentHash, a.SizeBytes, a.ContentType, a.TargetPath, a.SourceJobID, created, now)
	if err != nil {
		return fmt.Errorf("upsert named asset %q: %w", a.Name, err)
	}
	return nil
}

// GetNamedAssetByName returns the row for the given name, or
// ErrNamedAssetNotFound if no such row exists.
func GetNamedAssetByName(db *sql.DB, name string) (*NamedAsset, error) {
	var (
		a           NamedAsset
		createdAt   int64
		updatedAt   int64
		sourceJobID sql.NullInt64
	)
	err := db.QueryRow(`
		SELECT id, name, content_hash, size_bytes, content_type, target_path, source_job_id, created_at, updated_at
		FROM named_assets WHERE name = ?
	`, name).Scan(&a.ID, &a.Name, &a.ContentHash, &a.SizeBytes, &a.ContentType, &a.TargetPath, &sourceJobID, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNamedAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup named asset %q: %w", name, err)
	}
	if sourceJobID.Valid {
		v := sourceJobID.Int64
		a.SourceJobID = &v
	}
	a.CreatedAt = time.Unix(createdAt, 0)
	a.UpdatedAt = time.Unix(updatedAt, 0)
	return &a, nil
}

// GetNamedAssetByContentHash returns one published name for contentHash, or
// ErrNamedAssetNotFound if no name points at that content.
func GetNamedAssetByContentHash(db *sql.DB, contentHash string) (*NamedAsset, error) {
	var name string
	err := db.QueryRow(`
		SELECT name FROM named_assets WHERE content_hash = ? ORDER BY updated_at DESC, name LIMIT 1
	`, contentHash).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, ErrNamedAssetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup named asset hash %q: %w", contentHash, err)
	}
	return GetNamedAssetByName(db, name)
}

// ListNamedAssets returns all named assets, ordered by name.
func ListNamedAssets(db *sql.DB) ([]NamedAsset, error) {
	rows, err := db.Query(`
		SELECT id, name, content_hash, size_bytes, content_type, target_path, source_job_id, created_at, updated_at
		FROM named_assets ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list named assets: %w", err)
	}
	defer rows.Close()
	var out []NamedAsset
	for rows.Next() {
		var (
			a           NamedAsset
			createdAt   int64
			updatedAt   int64
			sourceJobID sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &a.Name, &a.ContentHash, &a.SizeBytes, &a.ContentType, &a.TargetPath, &sourceJobID, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if sourceJobID.Valid {
			v := sourceJobID.Int64
			a.SourceJobID = &v
		}
		a.CreatedAt = time.Unix(createdAt, 0)
		a.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}
