package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// BackupDirName is the subdirectory under the DB's parent where automatic
	// snapshots are written. Manual `.bak*` files alongside jobs.db are left
	// alone.
	BackupDirName = "backups"

	// MigrationBackupRetention is the number of pre-migration snapshots to
	// keep. Older ones are pruned automatically after a successful migration.
	MigrationBackupRetention = 3

	migrationBackupPrefix = "jobs.db.pre-migration-"
	manualSnapshotPrefix  = "jobs.db.snapshot-"
)

// SnapshotDir returns the directory where automatic snapshots are stored.
func SnapshotDir() string {
	return filepath.Join(filepath.Dir(dbPath), BackupDirName)
}

// Snapshot writes a consistent copy of the database at dbPath to dest using
// SQLite's `VACUUM INTO`. The destination must not already exist; SQLite
// refuses to overwrite. The source DB is checkpointed first so the snapshot
// reflects the latest committed state and is independent of any WAL file.
func Snapshot(database *sql.DB, dest string) error {
	if database == nil {
		return fmt.Errorf("snapshot: nil database")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("snapshot: destination already exists: %s", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("snapshot: create dest dir: %w", err)
	}
	// Checkpoint to fold WAL contents into the main DB file. Best-effort:
	// VACUUM INTO works without it, but the snapshot is cleaner with it.
	_, _ = database.Exec(`PRAGMA wal_checkpoint(FULL)`)
	if _, err := database.Exec(`VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("snapshot: VACUUM INTO %s: %w", dest, err)
	}
	return nil
}

// backupBeforeMigration writes a pre-migration snapshot to the standard
// backup directory and prunes older ones to MigrationBackupRetention. Errors
// are logged but not returned: a backup failure must not block migration,
// since the alternative (refusing to start) is worse than running unbacked.
func backupBeforeMigration(database *sql.DB, fromVersion int) {
	dir := SnapshotDir()
	ts := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(dir, fmt.Sprintf("%sv%d-%s.db", migrationBackupPrefix, fromVersion, ts))
	if err := Snapshot(database, dest); err != nil {
		slog.Warn("pre-migration backup failed; continuing with migration", "error", err, "dest", dest)
		return
	}
	slog.Info("pre-migration backup written", "path", dest)
	if err := pruneOldBackups(dir, migrationBackupPrefix, MigrationBackupRetention); err != nil {
		slog.Warn("prune old migration backups failed", "error", err)
	}
}

// ListSnapshots returns the snapshot files in dir, newest first. Includes
// both pre-migration backups and manual snapshots. Returns absolute paths.
func ListSnapshots() ([]string, error) {
	return listBackupsByPrefix(SnapshotDir(), "")
}

// PruneSnapshots removes snapshots older than the keep-most-recent count.
// Returns the paths that were deleted. When keep <= 0, every snapshot is
// removed.
func PruneSnapshots(keep int) ([]string, error) {
	dir := SnapshotDir()
	all, err := listBackupsByPrefix(dir, "")
	if err != nil {
		return nil, err
	}
	if keep < 0 {
		keep = 0
	}
	if len(all) <= keep {
		return nil, nil
	}
	toDelete := all[keep:]
	deleted := make([]string, 0, len(toDelete))
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil {
			return deleted, err
		}
		deleted = append(deleted, p)
	}
	return deleted, nil
}

// listBackupsByPrefix returns files in dir whose basenames start with the
// given prefix (or all files if prefix is ""), sorted newest-first by mtime.
func listBackupsByPrefix(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type entryInfo struct {
		path  string
		mtime time.Time
	}
	matches := make([]entryInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		// Skip non-database side files (-wal, -shm).
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		matches = append(matches, entryInfo{
			path:  filepath.Join(dir, name),
			mtime: info.ModTime(),
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].mtime.After(matches[j].mtime)
	})
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.path
	}
	return out, nil
}

// pruneOldBackups keeps the N newest files matching prefix in dir, deleting
// the rest. Used by the automatic migration backup to bound disk usage.
func pruneOldBackups(dir, prefix string, keep int) error {
	matches, err := listBackupsByPrefix(dir, prefix)
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		return nil
	}
	for _, p := range matches[keep:] {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// ManualSnapshotPath returns a default path for a manual snapshot taken now.
func ManualSnapshotPath() string {
	ts := time.Now().UTC().Format("20060102-150405")
	return filepath.Join(SnapshotDir(), fmt.Sprintf("%s%s.db", manualSnapshotPrefix, ts))
}
