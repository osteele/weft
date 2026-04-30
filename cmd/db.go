package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage the local jobs database",
	Long: `Manage ~/.config/weft/jobs.db: take snapshots, prune old snapshots,
inspect the schema.

Automatic snapshots are written to ~/.config/weft/backups/ before any schema
migration. Manual snapshots can be taken at any time with "weft db snapshot".`,
}

var dbSnapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Write a consistent snapshot of the jobs database",
	Long: `Take a consistent point-in-time snapshot of the jobs database using
SQLite's VACUUM INTO. The destination is a self-contained .db file with no
companion -wal/-shm; it can be opened directly by sqlite3 or by analysis tools.

Snapshots are written to ~/.config/weft/backups/jobs.db.snapshot-<timestamp>.db
by default. Pass --out PATH to override.

Suitable for periodic backups (e.g. daily via launchd/cron) and ad-hoc dumps
for offline analysis.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDBSnapshot,
}

var dbGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Prune old database snapshots",
	Long: `Delete old snapshots from ~/.config/weft/backups/, keeping the N
most recent. Defaults to keeping the 10 newest. Use --keep 0 to delete all.

Pre-migration backups, manual snapshots, and any other .db files in the
backup directory are all considered.

Without --apply, the command runs in dry-run mode and only prints what would
be deleted.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDBGC,
}

var (
	dbSnapshotOut string
	dbGCKeep      int
	dbGCApply     bool
)

func init() {
	rootCmd.AddCommand(dbCmd)
	dbCmd.AddCommand(dbSnapshotCmd)
	dbCmd.AddCommand(dbGCCmd)

	dbSnapshotCmd.Flags().StringVar(&dbSnapshotOut, "out", "", "Destination path (default: ~/.config/weft/backups/jobs.db.snapshot-<timestamp>.db)")
	dbGCCmd.Flags().IntVar(&dbGCKeep, "keep", 10, "Number of newest snapshots to keep")
	dbGCCmd.Flags().BoolVar(&dbGCApply, "apply", false, "Delete files (without this flag, only preview)")
}

func runDBSnapshot(cmd *cobra.Command, _ []string) error {
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	dest := dbSnapshotOut
	if dest == "" {
		dest = db.ManualSnapshotPath()
	} else {
		abs, err := filepath.Abs(dest)
		if err != nil {
			return fmt.Errorf("resolve --out %q: %w", dest, err)
		}
		dest = abs
	}

	if err := db.Snapshot(database, dest); err != nil {
		return err
	}
	info, _ := os.Stat(dest)
	if info != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote snapshot: %s (%s)\n", dest, humanizeBytes(info.Size()))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote snapshot: %s\n", dest)
	}
	return nil
}

func runDBGC(cmd *cobra.Command, _ []string) error {
	if dbGCKeep < 0 {
		return usageErrorf("--keep must be >= 0")
	}

	all, err := db.ListSnapshots()
	if err != nil {
		return err
	}
	if len(all) <= dbGCKeep {
		fmt.Fprintf(cmd.OutOrStdout(), "Nothing to prune (%d snapshot(s) <= --keep=%d)\n", len(all), dbGCKeep)
		return nil
	}

	toDelete := all[dbGCKeep:]
	var totalBytes int64
	for _, p := range toDelete {
		if info, err := os.Stat(p); err == nil {
			totalBytes += info.Size()
		}
		verb := "Would delete"
		if dbGCApply {
			verb = "Deleting"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, p)
	}

	if !dbGCApply {
		fmt.Fprintf(cmd.OutOrStdout(), "Would free: %s (%d bytes)\n", humanizeBytes(totalBytes), totalBytes)
		fmt.Fprintf(cmd.OutOrStdout(), "Dry run: no files deleted. Pass --apply to delete.\n")
		return nil
	}

	deleted, err := db.PruneSnapshots(dbGCKeep)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d snapshot(s), reclaimed %s\n", len(deleted), humanizeBytes(totalBytes))
	return nil
}
