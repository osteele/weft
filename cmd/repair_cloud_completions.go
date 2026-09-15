package cmd

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/syncorch"
	"github.com/spf13/cobra"
)

var repairCloudCompletionsNestedCmd = &cobra.Command{
	Use:   "cloud-completions",
	Short: "Repair incomplete cloud completion metadata",
	Long: `Re-arm terminal cloud jobs whose start_time is 0 and end_time is set.
These rows carry timestamps created by an earlier marker-only sync rather than
the agent's authoritative completion JSON. Clearing last_synced_status makes
them eligible for completion backfill.

With --sync-r2, run the explicit historical completion sweep after re-arming.
The sweep covers incomplete terminal attempts outside the automatic 24-hour
routine-sync window and may scan historical R2 job prefixes.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runRepairCloudCompletions,
}

var repairCloudCompletionsDryRun bool
var repairCloudCompletionsSyncR2 bool

func init() {
	jobRepairCmd.AddCommand(repairCloudCompletionsNestedCmd)
	repairCloudCompletionsNestedCmd.Flags().BoolVar(&repairCloudCompletionsDryRun, "dry-run", false, "Print affected job IDs without modifying the database")
	repairCloudCompletionsNestedCmd.Flags().BoolVar(&repairCloudCompletionsSyncR2, "sync-r2", false, "Run the explicit historical R2 completion sweep after re-arming")
}

func runRepairCloudCompletions(_ *cobra.Command, _ []string) error {
	if repairCloudCompletionsDryRun && repairCloudCompletionsSyncR2 {
		return fmt.Errorf("--sync-r2 cannot be combined with --dry-run")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	affected, err := db.FindPoisonedCloudCompletions(database)
	if err != nil {
		return err
	}
	if len(affected) == 0 {
		fmt.Println("No poisoned cloud completions found.")
		if repairCloudCompletionsSyncR2 {
			return runRepairCloudCompletionsR2Sync(database)
		}
		return nil
	}

	for _, p := range affected {
		verb := "re-armed backfill"
		if repairCloudCompletionsDryRun {
			verb = "would re-arm backfill"
		}
		fmt.Printf("%s: job=%d attempt=%d\n", verb, p.JobID, p.AttemptID)
	}

	if repairCloudCompletionsDryRun {
		fmt.Printf("\n%d row(s) would be re-armed. Run without --dry-run to apply.\n", len(affected))
		return nil
	}

	n, err := db.RearmPoisonedCloudCompletions(database)
	if err != nil {
		return err
	}
	fmt.Printf("\nRe-armed backfill on %d row(s).", n)
	if !repairCloudCompletionsSyncR2 {
		fmt.Println(" Run `weft job repair cloud-completions --sync-r2` to fetch authoritative timestamps from R2.")
		return nil
	}
	fmt.Println()
	return runRepairCloudCompletionsR2Sync(database)
}

func runRepairCloudCompletionsR2Sync(database *sql.DB) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	updated := syncorch.SyncCloudJobResultsRepair(context.Background(), cfg, database, true)
	fmt.Printf("Historical R2 completion sweep updated %d job(s).\n", updated)
	return nil
}
