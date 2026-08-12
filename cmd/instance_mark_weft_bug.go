package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var instanceMarkWeftBugDetail string

var instanceMarkWeftBugCmd = &cobra.Command{
	Use:   "mark-weft-bug <instance-id> [instance-id...]",
	Short: "Exclude Weft-caused terminations from survival training",
	Long: `Reclassify failed or canceled cloud instances whose termination was caused by
a confirmed Weft defect. The original termination detail is preserved, and the
row remains available for lifecycle and cost accounting, but it is excluded
from provider, GPU, region, and machine survival training.

Only generic termination reasons (provider_failure, infra_failure, unknown, or
empty) can be reclassified. A maintainer detail is required so the correction
retains its evidence and rationale.

Example:
  weft instance mark-weft-bug wi42 --detail "false stale-heartbeat teardown; fixed in <revision>"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runInstanceMarkWeftBug,
}

func init() {
	instanceMarkWeftBugCmd.Flags().StringVar(&instanceMarkWeftBugDetail, "detail", "",
		"Evidence and rationale for attributing the termination to a Weft defect (required)")
}

func runInstanceMarkWeftBug(cmd *cobra.Command, args []string) error {
	detail := strings.TrimSpace(instanceMarkWeftBugDetail)
	if detail == "" {
		return usageErrorf("--detail is required when reclassifying an instance as a Weft bug")
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	idsToUpdate, err := collectWeftBugReclassifyTargets(database, args)
	if err != nil {
		return err
	}
	detailPrefix := fmt.Sprintf("reclassified as %s at %s: %s",
		db.TerminationReasonWeftBug, time.Now().UTC().Format(time.RFC3339), detail)
	for _, id := range idsToUpdate {
		if err := db.ReclassifyLaunchTerminationReason(database, id,
			db.TerminationReasonWeftBug, detailPrefix); err != nil {
			return fmt.Errorf("reclassify %s: %w", ids.FormatInstanceID(id), err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Reclassified %s as %s; excluded from survival training.\n",
			ids.FormatInstanceID(id), db.TerminationReasonWeftBug)
	}
	return nil
}

func collectWeftBugReclassifyTargets(database *sql.DB, args []string) ([]int64, error) {
	result := make([]int64, 0, len(args))
	seen := make(map[int64]struct{}, len(args))
	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return nil, fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		launch, err := db.GetLaunch(database, id)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", ids.FormatInstanceID(id), err)
		}
		if launch.Status != db.LaunchStatusFailed && launch.Status != db.LaunchStatusCancelled {
			return nil, fmt.Errorf("instance %s has status %q; reclassify only allowed on failed/canceled instances",
				ids.FormatInstanceID(id), launch.Status)
		}
		if !db.IsReclassifyEligibleReason(launch.TerminationReason) {
			return nil, fmt.Errorf("instance %s has termination_reason %q which is not eligible for reclassification",
				ids.FormatInstanceID(id), launch.TerminationReason)
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}
