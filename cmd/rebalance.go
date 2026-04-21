package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var (
	rebalanceMaxIncrease float64
	rebalanceInstances   string
	rebalanceJobs        string
	rebalanceYes         bool
)

var rebalanceCmd = &cobra.Command{
	Use:   "rebalance",
	Short: "Re-balance queued jobs across existing instances",
	Long: `Move queued jobs between existing cloud instances to start sooner
without exceeding a cost ratio ceiling.

By default this command is a dry-run and prints what would move.
Use --yes to apply the moves.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runRebalance,
}

func init() {
	rebalanceCmd.Flags().Float64Var(&rebalanceMaxIncrease, "max-increase", 0, "Maximum dst/src cost ratio (e.g. 1.10)")
	rebalanceCmd.Flags().StringVar(&rebalanceInstances, "instances", "", "Comma-separated instance IDs to scope (e.g. wi101,wi102)")
	rebalanceCmd.Flags().StringVar(&rebalanceJobs, "jobs", "", "Job IDs/ranges to scope (e.g. wj10,wj12:wj15)")
	rebalanceCmd.Flags().BoolVarP(&rebalanceYes, "yes", "y", false, "Apply moves (default is dry-run)")
	rootCmd.AddCommand(rebalanceCmd)
}

func runRebalance(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	instanceScope, err := parseRebalanceInstanceScope(rebalanceInstances)
	if err != nil {
		return err
	}
	jobScope, err := parseRebalanceJobScope(rebalanceJobs)
	if err != nil {
		return err
	}

	result, err := orchestration.RebalanceQueuedJobsAcrossInstances(cmd.Context(), database, orchestration.QueueRebalanceOptions{
		Apply:               rebalanceYes,
		CostCeilingOverride: rebalanceMaxIncrease,
		InstanceScope:       instanceScope,
		JobScope:            jobScope,
		Operation:           "cli.rebalance",
	})
	if err != nil {
		return err
	}

	sort.Slice(result.Moves, func(i, j int) bool {
		if result.Moves[i].JobID != result.Moves[j].JobID {
			return result.Moves[i].JobID < result.Moves[j].JobID
		}
		if result.Moves[i].FromInstanceID != result.Moves[j].FromInstanceID {
			return result.Moves[i].FromInstanceID < result.Moves[j].FromInstanceID
		}
		return result.Moves[i].ToInstanceID < result.Moves[j].ToInstanceID
	})

	printRebalanceMovesTable(cmd.OutOrStdout(), result.Moves, rebalanceYes)
	if len(result.Moves) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No rebalance moves found.")
		return nil
	}
	if rebalanceYes {
		fmt.Fprintf(cmd.OutOrStdout(), "Applied %d rebalance move(s).\n", len(result.Moves))
	}
	return nil
}

func parseRebalanceInstanceScope(raw string) (map[int64]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[int64]struct{})
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		instanceID, err := ids.ParseInstanceID(token)
		if err != nil {
			return nil, usageErrorf("invalid --instances value %q", token)
		}
		out[instanceID] = struct{}{}
	}
	if len(out) == 0 {
		return nil, usageErrorf("invalid --instances: no instance IDs parsed")
	}
	return out, nil
}

func parseRebalanceJobScope(raw string) (map[int64]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	jobIDs, err := ParseJobIDs([]string{raw})
	if err != nil {
		return nil, err
	}
	if len(jobIDs) == 0 {
		return nil, usageErrorf("invalid --jobs: no job IDs parsed")
	}
	out := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		out[jobID] = struct{}{}
	}
	return out, nil
}

func printRebalanceMovesTable(out io.Writer, moves []orchestration.QueueRebalanceMove, applied bool) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if applied {
		fmt.Fprintln(w, "OK\tJOB\tFROM\tTO\tRATIO\tSAVING\tREASON")
	} else {
		fmt.Fprintln(w, "JOB\tFROM\tTO\tRATIO\tSAVING\tREASON")
	}
	for _, move := range moves {
		jobID := ids.FormatJobID(move.JobID)
		fromID := ids.FormatInstanceID(move.FromInstanceID)
		toID := ids.FormatInstanceID(move.ToInstanceID)
		ratio := fmt.Sprintf("%.2f", move.CostRatio)
		if applied {
			fmt.Fprintf(w, "✓\t%s\t%s\t%s\t%s\t-\t%s\n", jobID, fromID, toID, ratio, move.Reason)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t-\t%s\n", jobID, fromID, toID, ratio, move.Reason)
		}
	}
	_ = w.Flush()
}
