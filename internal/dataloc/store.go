package dataloc

import (
	"crypto/md5"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// InitSchema creates the data locality tables if they don't exist.
func InitSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS host_data (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		asset_kind TEXT NOT NULL,
		asset_id TEXT NOT NULL,
		path TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		content_hash TEXT DEFAULT '',
		content_type TEXT DEFAULT '',
		last_seen INTEGER NOT NULL,
		UNIQUE(host, asset_kind, asset_id)
	);
	CREATE INDEX IF NOT EXISTS idx_host_data_host ON host_data(host);
	CREATE INDEX IF NOT EXISTS idx_host_data_asset ON host_data(asset_kind, asset_id);
	CREATE TABLE IF NOT EXISTS data_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		asset_kind TEXT NOT NULL,
		asset_id TEXT NOT NULL,
		revision TEXT NOT NULL DEFAULT 'main',
		status TEXT NOT NULL,
		error_message TEXT DEFAULT '',
		remote_path TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		requested_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_data_requests_host ON data_requests(host);
	CREATE INDEX IF NOT EXISTS idx_data_requests_status ON data_requests(status);
	CREATE INDEX IF NOT EXISTS idx_data_requests_asset ON data_requests(asset_kind, asset_id);
	`
	_, err := db.Exec(schema)
	return err
}

// RecordAsset upserts a host data entry, updating last_seen if it already exists.
func RecordAsset(db *sql.DB, entry HostDataEntry) error {
	contentType := string(entry.ContentType)
	_, err := db.Exec(`
		INSERT INTO host_data (host, asset_kind, asset_id, path, size_bytes, content_hash, content_type, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host, asset_kind, asset_id) DO UPDATE SET
			path = excluded.path,
			size_bytes = excluded.size_bytes,
			content_hash = excluded.content_hash,
			content_type = excluded.content_type,
			last_seen = excluded.last_seen
	`, entry.Host, string(entry.Asset.Kind), entry.Asset.ID,
		entry.Path, entry.SizeBytes, entry.ContentHash, contentType, entry.LastSeen.Unix())
	return err
}

// ListHostAssets returns all data assets known to exist on a host.
func ListHostAssets(db *sql.DB, host string) ([]HostDataEntry, error) {
	rows, err := db.Query(`
		SELECT host, asset_kind, asset_id, path, size_bytes, content_hash, content_type, last_seen
		FROM host_data WHERE host = ? ORDER BY asset_kind, asset_id
	`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

// HostAssetSnapshotHash returns an md5 of the host and resident asset IDs.
func HostAssetSnapshotHash(db *sql.DB, host string) (string, int, error) {
	entries, err := ListHostAssets(db, host)
	if err != nil {
		return "", 0, err
	}
	assets := make([]string, 0, len(entries))
	for _, entry := range entries {
		assets = append(assets, string(entry.Asset.Kind)+":"+entry.Asset.ID)
	}
	sort.Strings(assets)
	payload := host + "\n" + strings.Join(assets, "\n")
	return fmt.Sprintf("%x", md5.Sum([]byte(payload))), len(assets), nil
}

// FindAssetHosts returns all hosts that have a given asset.
func FindAssetHosts(db *sql.DB, asset DataAsset) ([]HostDataEntry, error) {
	rows, err := db.Query(`
		SELECT host, asset_kind, asset_id, path, size_bytes, content_hash, content_type, last_seen
		FROM host_data WHERE asset_kind = ? AND asset_id = ?
		ORDER BY last_seen DESC
	`, string(asset.Kind), asset.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

// HostDataEntryWithUsage extends HostDataEntry with the last time the asset
// was used in a job (zero if it has never appeared in a job's inputs).
type HostDataEntryWithUsage struct {
	HostDataEntry
	LastUsedAt time.Time
}

// FindAssetsNotUsedSince returns host_data entries whose last job usage (via
// the jobs.inputs column) is before the cutoff, or that have never been used
// in a job at all. If host is non-empty, only entries for that host are
// returned.
func FindAssetsNotUsedSince(db *sql.DB, host string, cutoff time.Time) ([]HostDataEntryWithUsage, error) {
	hostClause := "1=1"
	args := []any{cutoff.Unix()}
	if host != "" {
		hostClause = "hd.host = ?"
		args = append([]any{host}, args...)
	}

	// The subquery finds the most recent start_time of any job on the same host
	// whose inputs JSON array contains the matching asset ref.
	// COALESCE(..., 0) treats "never used" as epoch 0, so it's always < cutoff.
	query := `
		SELECT hd.host, hd.asset_kind, hd.asset_id, hd.path, hd.size_bytes, hd.content_hash, hd.content_type, hd.last_seen,
		       COALESCE((
		           SELECT MAX(j.start_time)
		           FROM jobs j, json_each(j.inputs) je
		           WHERE j.host = hd.host
		             AND j.inputs IS NOT NULL
		             AND je.value = CASE hd.asset_kind
		                 WHEN 'hf-model'   THEN 'hf:' || hd.asset_id
		                 WHEN 'hf-dataset' THEN 'hf-dataset:' || hd.asset_id
		                 WHEN 'checkpoint' THEN 'checkpoint:' || hd.asset_id
		                 WHEN 'job-output' THEN 'job-output:' || hd.asset_id
		                 ELSE NULL
		               END
		       ), 0) AS last_used_at
		FROM host_data hd
		WHERE ` + hostClause + `
		  AND last_used_at < ?
		ORDER BY last_used_at, hd.host, hd.asset_kind, hd.asset_id
	`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []HostDataEntryWithUsage
	for rows.Next() {
		var e HostDataEntryWithUsage
		var kind string
		var lastSeen, lastUsedAt int64
		var contentType string
		if err := rows.Scan(&e.Host, &kind, &e.Asset.ID, &e.Path, &e.SizeBytes, &e.ContentHash, &contentType, &lastSeen, &lastUsedAt); err != nil {
			return nil, err
		}
		e.Asset.Kind = AssetKind(kind)
		e.ContentType = ContentType(contentType)
		e.LastSeen = time.Unix(lastSeen, 0)
		if lastUsedAt > 0 {
			e.LastUsedAt = time.Unix(lastUsedAt, 0)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// RemoveStaleEntries deletes entries that haven't been seen since the given time.
func RemoveStaleEntries(db *sql.DB, host string, before time.Time) (int64, error) {
	result, err := db.Exec(`
		DELETE FROM host_data WHERE host = ? AND last_seen < ?
	`, host, before.Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RemoveStaleEntriesForKinds deletes entries for the given host whose
// last_seen is older than `before` AND whose kind is one of `kinds`. Use
// this after a scan that only covers a subset of asset kinds, so that
// checkpoints/job-outputs (which scans don't surface) are not removed.
func RemoveStaleEntriesForKinds(db *sql.DB, host string, before time.Time, kinds []AssetKind) (int64, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(kinds))
	args := make([]any, 0, len(kinds)+2)
	args = append(args, host, before.Unix())
	for i, k := range kinds {
		placeholders[i] = "?"
		args = append(args, string(k))
	}
	query := `DELETE FROM host_data WHERE host = ? AND last_seen < ? AND asset_kind IN (` +
		strings.Join(placeholders, ",") + `)`
	result, err := db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListAllAssets returns all data assets across all hosts, ordered by kind and ID.
func ListAllAssets(db *sql.DB) ([]HostDataEntry, error) {
	rows, err := db.Query(`
		SELECT host, asset_kind, asset_id, path, size_bytes, content_hash, content_type, last_seen
		FROM host_data ORDER BY asset_kind, asset_id, host
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) ([]HostDataEntry, error) {
	var entries []HostDataEntry
	for rows.Next() {
		var e HostDataEntry
		var kind string
		var contentType string
		var lastSeen int64
		if err := rows.Scan(&e.Host, &kind, &e.Asset.ID, &e.Path, &e.SizeBytes, &e.ContentHash, &contentType, &lastSeen); err != nil {
			return nil, err
		}
		e.Asset.Kind = AssetKind(kind)
		e.ContentType = ContentType(contentType)
		e.LastSeen = time.Unix(lastSeen, 0)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
