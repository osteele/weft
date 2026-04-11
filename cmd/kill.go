package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running jobs",
	Long: `Kill running jobs by their IDs.

Examples:
  weft kill 42
  weft kill 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runKill,
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	var errors []string
	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("kill"), oplog.WithJobID(jobID))

		result, err := orchestration.KillOrCancelJob(database, jobID, db.StatusKilled, ops.TimeoutNormal)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		message := result.Message
		if message == "" {
			message = fmt.Sprintf("Job %d updated", jobID)
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
