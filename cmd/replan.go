package cmd

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/status"
	"github.com/spf13/cobra"
)

var replanCmd = &cobra.Command{
	Use:   "replan <wj-id>...",
	Short: "Cancel current placement and return jobs to planning",
	Long: `Cancel a job's current placement and return it to the unplaced queue.

This is explicit replanning: for a rental job, the current instance placement
is canceled first; for a queued inventory job, the remote queue entry is
removed and the job becomes unplaced. Terminal jobs should use retry instead.`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runReplan,
}

func init() {
	rootCmd.AddCommand(replanCmd)
}

func runReplan(_ *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDsForJobCommand(args)
	if err != nil {
		return err
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for _, jobID := range jobIDs {
		if err := replanOneJob(database, jobID); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("%s: %v", ids.FormatJobID(jobID), err))
		}
	}
	if len(errorsList) > 0 {
		return fmt.Errorf("replan failed: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func replanOneJob(database *sql.DB, jobID int64) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return db.ErrJobNotFound
	}
	if status.IsTerminal(job.EffectiveStatus()) {
		return fmt.Errorf("job is %s; use retry instead", job.EffectiveStatus())
	}

	switch {
	case job.IsRentalJob():
		if _, err := orchestration.KillOrCancelJob(database, jobID, db.StatusCanceled, ops.TimeoutNormal); err != nil {
			return err
		}
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return err
		}
		_ = db.SetJobPlacementReasons(database, jobID, []string{"replan requested; previous rental placement canceled"})
		fmt.Printf("Replanned %s from rental placement; job returned to unplaced queue\n", ids.FormatJobID(jobID))
	case job.HasInventoryHost():
		if job.EffectiveStatus() != db.StatusQueued {
			return fmt.Errorf("inventory job is %s; kill it first or use retry", job.EffectiveStatus())
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.DefaultOptions())
		if err != nil {
			return err
		}
		if result.Message != "" {
			fmt.Println(result.Message)
		} else {
			fmt.Printf("Replanned %s; job returned to unplaced queue\n", ids.FormatJobID(jobID))
		}
	default:
		fmt.Printf("Replanned %s; job is already unplaced\n", ids.FormatJobID(jobID))
	}
	return nil
}
