package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var jobRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Repair local job bookkeeping",
	Long: `Repair local job bookkeeping inconsistencies that make sync or display
consider old rows actionable. These commands operate on the local database
unless a subcommand explicitly says it contacts hosts or cloud providers.`,
	Args: usageArgs(cobra.NoArgs),
}

var jobRepairSyncStateCmd = &cobra.Command{
	Use:   "sync-state",
	Short: "Clear stale terminal pending status from local inventory jobs",
	Long: `Clear pending_status from terminal inventory-host jobs.

This is a local database repair for rows where a job is already terminal but
still has a pending cancel/kill/requeue intent. Those rows keep host sync's
pending-reconciliation pass looking at old jobs even though no remote work can
apply the intent anymore.

The repair does not contact hosts and does not rewrite last_synced_status.
Nonzero-exit queue-runner jobs can legitimately appear as failed while their
last synced remote state is completed.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runJobRepairSyncState,
}

var (
	jobRepairSyncStateDryRun bool
	jobRepairSyncStateHost   string
)

func init() {
	jobCmd.AddCommand(jobRepairCmd)
	jobRepairCmd.AddCommand(jobRepairSyncStateCmd)
	jobRepairSyncStateCmd.Flags().BoolVar(&jobRepairSyncStateDryRun, "dry-run", false, "Print affected job IDs without modifying the database")
	jobRepairSyncStateCmd.Flags().StringVar(&jobRepairSyncStateHost, "host", "", "Limit local DB repair to jobs assigned to this host")
}

func runJobRepairSyncState(_ *cobra.Command, _ []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	found, err := db.FindStaleTerminalPendingStatuses(database, jobRepairSyncStateHost)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Println("No stale terminal pending statuses found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tHOST\tSTATUS\tPENDING\tLAST_SYNCED\tENDED")
	for _, row := range found {
		ended := time.Unix(row.EndTime, 0).Format("2006-01-02 15:04:05")
		lastSynced := row.LastSyncedStatus
		if lastSynced == "" {
			lastSynced = "-"
		}
		fmt.Fprintf(w, "wj%d\t%s\t%s\t%s\t%s\t%s\n",
			row.JobID,
			row.Host,
			row.Status,
			row.PendingStatus,
			lastSynced,
			ended,
		)
	}
	_ = w.Flush()

	if jobRepairSyncStateDryRun {
		fmt.Printf("\n%d row(s) would be repaired. Run without --dry-run to apply.\n", len(found))
		return nil
	}

	n, err := db.ClearStaleTerminalPendingStatuses(database, jobRepairSyncStateHost)
	if err != nil {
		return err
	}
	fmt.Printf("\nCleared stale pending_status on %d row(s).\n", n)
	return nil
}
