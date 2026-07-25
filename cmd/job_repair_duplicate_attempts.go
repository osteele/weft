package cmd

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var jobRepairDuplicateAttemptsCmd = &cobra.Command{
	Use:   "duplicate-attempts",
	Short: "Abandon duplicate terminal cloud attempt rows",
	Long: `Find exact duplicate terminal attempt rows for the same job on the
same rental instance and mark all but the earliest matching row abandoned.

This repairs historical bookkeeping loops without deleting audit rows. Without
--apply, the command only reports the affected groups.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runJobRepairDuplicateAttempts,
}

var (
	jobRepairDuplicateAttemptsApply bool
	jobRepairDuplicateAttemptsLimit int
)

func init() {
	jobRepairCmd.AddCommand(jobRepairDuplicateAttemptsCmd)
	jobRepairDuplicateAttemptsCmd.Flags().BoolVar(&jobRepairDuplicateAttemptsApply, "apply", false, "Modify the database (default is dry-run)")
	jobRepairDuplicateAttemptsCmd.Flags().IntVar(&jobRepairDuplicateAttemptsLimit, "limit", 20, "Number of duplicate groups to print")
}

func runJobRepairDuplicateAttempts(cmd *cobra.Command, _ []string) error {
	if jobRepairDuplicateAttemptsLimit < 0 {
		return usageErrorf("--limit must be >= 0")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	groups, err := db.FindDuplicateTerminalCloudAttemptGroups(database)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No duplicate terminal cloud attempts found.")
		return nil
	}

	duplicateRows := 0
	for _, g := range groups {
		duplicateRows += g.Count - 1
	}

	printDuplicateAttemptGroups(cmd, groups, duplicateRows)
	if !jobRepairDuplicateAttemptsApply {
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d duplicate row(s) would be abandoned. Pass --apply to modify the database.\n", duplicateRows)
		return nil
	}

	snapshotPath := db.ManualSnapshotPath()
	if err := db.Snapshot(database, snapshotPath); err != nil {
		return fmt.Errorf("write pre-repair snapshot: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nWrote pre-repair snapshot: %s\n", snapshotPath)

	n, err := db.AbandonDuplicateTerminalCloudAttempts(database)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Abandoned %d duplicate terminal cloud attempt row(s).\n", n)
	return nil
}

func printDuplicateAttemptGroups(cmd *cobra.Command, groups []db.DuplicateTerminalAttemptGroup, duplicateRows int) {
	limit := jobRepairDuplicateAttemptsLimit
	if limit == 0 || limit > len(groups) {
		limit = len(groups)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tLAUNCH\tCOUNT\tKEEP\tATTEMPTS\tSTATUS\tSTART\tEND\tOUTCOME")
	for _, g := range groups[:limit] {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d-%d\t%s\t%s\t%s\t%s\n",
			ids.FormatJobID(g.JobID),
			ids.FormatInstanceID(g.LaunchID),
			g.Count,
			g.KeepAttemptID,
			g.FirstAttemptNumber,
			g.LastAttemptNumber,
			g.Status,
			formatOptionalUnix(g.StartTime),
			formatOptionalUnix(g.EndTime),
			nonEmptyOrDash(g.CloudOutcome),
		)
	}
	_ = w.Flush()
	if len(groups) > limit {
		fmt.Fprintf(cmd.OutOrStdout(), "... %d more group(s) omitted by --limit\n", len(groups)-limit)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Found %d duplicate group(s), %d duplicate row(s).\n", len(groups), duplicateRows)
}

func formatOptionalUnix(value *int64) string {
	if value == nil || *value == 0 {
		return "-"
	}
	return time.Unix(*value, 0).Format("2006-01-02 15:04:05")
}

func nonEmptyOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
