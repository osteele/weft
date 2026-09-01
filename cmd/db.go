package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/telemetryarchive"
	"github.com/spf13/cobra"
)

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage the local jobs database",
	Long: `Manage $XDG_STATE_HOME/weft/jobs.db: take snapshots, archive terminal
raw telemetry, prune old snapshots, and inspect the schema.

Automatic snapshots are written to $XDG_STATE_HOME/weft/backups/ before any schema
migration. Manual snapshots can be taken at any time with "weft db snapshot".`,
}

var dbSnapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Write a consistent snapshot of the jobs database",
	Long: `Take a consistent point-in-time snapshot of the jobs database using
SQLite's VACUUM INTO. The destination is a self-contained .db file with no
companion -wal/-shm; it can be opened directly by sqlite3 or by analysis tools.

Snapshots are written to $XDG_STATE_HOME/weft/backups/jobs.db.snapshot-<timestamp>.db
by default. Pass --out PATH to override.

Suitable for periodic backups (e.g. daily via launchd/cron) and ad-hoc dumps
for offline analysis.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDBSnapshot,
}

var dbGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Prune old database snapshots",
	Long: `Delete old snapshots from $XDG_STATE_HOME/weft/backups/, keeping the N
most recent. Defaults to keeping the 10 newest. Use --keep 0 to delete all.

Pre-migration backups, manual snapshots, and any other .db files in the
backup directory are all considered.

Without --apply, the command runs in dry-run mode and only prints what would
be deleted.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDBGC,
}

var dbArchiveTelemetryCmd = &cobra.Command{
	Use:   "archive-telemetry",
	Short: "Move terminal raw telemetry out of the operational database",
	Long: `Preserve terminal per-sample telemetry as verified job/run-scoped objects,
retain compact per-attempt summaries in SQLite, then prune the raw relational
rows. The command is resumable and runs as a dry run unless --apply is passed.

An apply run writes a consistent database snapshot before changing rows. Pass
--compact to reclaim freed SQLite pages after archival; compaction may need to
wait for other database writers.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runDBArchiveTelemetry,
}

var (
	dbSnapshotOut             string
	dbGCKeep                  int
	dbGCApply                 bool
	dbArchiveTelemetryApply   bool
	dbArchiveTelemetryCompact bool
	dbArchiveTelemetryLimit   int
)

func init() {
	rootCmd.AddCommand(dbCmd)
	dbCmd.AddCommand(dbSnapshotCmd)
	dbCmd.AddCommand(dbGCCmd)
	dbCmd.AddCommand(dbArchiveTelemetryCmd)

	dbSnapshotCmd.Flags().StringVar(&dbSnapshotOut, "out", "", "Destination path (default: $XDG_STATE_HOME/weft/backups/jobs.db.snapshot-<timestamp>.db)")
	dbGCCmd.Flags().IntVar(&dbGCKeep, "keep", 10, "Number of newest snapshots to keep")
	dbGCCmd.Flags().BoolVar(&dbGCApply, "apply", false, "Delete files (without this flag, only preview)")
	dbArchiveTelemetryCmd.Flags().BoolVar(&dbArchiveTelemetryApply, "apply", false, "Archive and prune rows (without this flag, only preview)")
	dbArchiveTelemetryCmd.Flags().BoolVar(&dbArchiveTelemetryCompact, "compact", false, "VACUUM the database after archival")
	dbArchiveTelemetryCmd.Flags().IntVar(&dbArchiveTelemetryLimit, "limit", 0, "Maximum attempts to process (0 means all)")
}

func runDBArchiveTelemetry(cmd *cobra.Command, _ []string) error {
	if dbArchiveTelemetryLimit < 0 {
		return usageErrorf("--limit must be >= 0")
	}
	if dbArchiveTelemetryCompact && !dbArchiveTelemetryApply {
		return usageErrorf("--compact requires --apply")
	}
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()
	candidates, err := db.ListTelemetryArchiveCandidates(database, dbArchiveTelemetryLimit)
	if err != nil {
		return err
	}
	var timeseriesRows, richRows int64
	for _, candidate := range candidates {
		timeseriesRows += candidate.TimeseriesSamples
		richRows += candidate.RichSamples
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Terminal attempts: %d\nTimeseries rows: %d\nRich telemetry rows: %d\n",
		len(candidates), timeseriesRows, richRows)
	if !dbArchiveTelemetryApply {
		fmt.Fprintln(cmd.OutOrStdout(), "Dry run: no rows changed. Pass --apply to archive and prune.")
		return nil
	}
	if len(candidates) == 0 && !dbArchiveTelemetryCompact {
		return nil
	}
	snapshot := db.ManualSnapshotPath()
	if err := db.Snapshot(database, snapshot); err != nil {
		return fmt.Errorf("pre-archive snapshot: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Snapshot: %s\n", snapshot)
	for i, candidate := range candidates {
		if err := archiveTelemetryCandidate(database, candidate); err != nil {
			return fmt.Errorf("archive job wj%d attempt %d: %w", candidate.JobID, candidate.AttemptID, err)
		}
		if (i+1)%100 == 0 || i+1 == len(candidates) {
			fmt.Fprintf(cmd.OutOrStdout(), "Archived %d/%d attempts\n", i+1, len(candidates))
		}
	}
	if dbArchiveTelemetryCompact {
		fmt.Fprintln(cmd.OutOrStdout(), "Compacting database...")
		if err := db.Compact(database); err != nil {
			return err
		}
	}
	return nil
}

func archiveTelemetryCandidate(database *sql.DB, candidate db.TelemetryArchiveCandidate) error {
	if candidate.TimeseriesSamples > 0 {
		samples, err := db.GetTimeseriesByRun(database, candidate.AttemptID)
		if err != nil {
			return err
		}
		raw, err := telemetryarchive.EncodeTimeseries(samples)
		if err != nil {
			return err
		}
		remote := telemetryarchive.RemoteCopy{}
		if obj, err := db.GetRawTelemetryObject(database, candidate.AttemptID, db.TimeseriesRawKind); err != nil {
			return err
		} else if obj != nil {
			remote = telemetryarchive.RemoteCopy{R2Key: obj.R2Key, ETag: obj.ETag}
		}
		if err := telemetryarchive.FinalizeTimeseries(database, candidate.JobID, candidate.AttemptID, raw, samples, remote); err != nil {
			return err
		}
	}
	if candidate.RichSamples > 0 {
		samples, err := db.GetTelemetryByRun(database, candidate.AttemptID)
		if err != nil {
			return err
		}
		raw, err := telemetryarchive.EncodeRich(samples)
		if err != nil {
			return err
		}
		rollup, err := db.BuildRichTelemetryRollupForJob(database, candidate.JobID, candidate.AttemptID, samples)
		if err != nil {
			return err
		}
		remote := telemetryarchive.RemoteCopy{}
		if obj, err := db.GetRawTelemetryObject(database, candidate.AttemptID, db.TelemetryRawKind); err != nil {
			return err
		} else if obj != nil {
			remote = telemetryarchive.RemoteCopy{R2Key: obj.R2Key, ETag: obj.ETag}
		}
		if err := telemetryarchive.FinalizeRich(database, candidate.JobID, candidate.AttemptID, raw, rollup, remote); err != nil {
			return err
		}
	}
	return nil
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
