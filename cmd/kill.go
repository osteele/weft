package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running or queued jobs",
	Long: `Kill running or queued jobs by their IDs.

Examples:
  weft kill 42
  weft kill 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runKill,
}

type killActorContextKey struct{}

func addKillFlags(cmd *cobra.Command) {
	cmd.Flags().String("reason", "", "Record why the job was killed")
}

func defaultKillAttribution() ops.StopAttribution {
	actor := submitterSession()
	if strings.TrimSpace(actor) == "" {
		actor = ops.LocalActor()
	}
	return ops.StopAttribution{
		Actor:       strings.TrimSpace(actor),
		RequestedAt: time.Now(),
	}
}

func killAttribution(cmd *cobra.Command) ops.StopAttribution {
	attribution := defaultKillAttribution()
	if commandContext := cmd.Context(); commandContext != nil {
		if actor, _ := commandContext.Value(killActorContextKey{}).(string); strings.TrimSpace(actor) != "" {
			attribution.Actor = strings.TrimSpace(actor)
		}
	}
	reason, _ := cmd.Flags().GetString("reason")
	attribution.Reason = strings.TrimSpace(reason)
	return attribution
}

func init() {
	addKillFlags(killCmd)
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	if activeEdgeSubmit != nil {
		return submitEdgeJobControls(cmd, args, edge.ControlKill, ParseJobIDs)
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	attribution := killAttribution(cmd)

	var errors []string
	for _, jobID := range jobIDs {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("kill"), oplog.WithJobID(jobID))
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if handled, err := cancelSkyJob(cmd, database, job); handled {
			if err != nil {
				errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			}
			continue
		}

		result, err := orchestration.KillOrCancelJob(database, jobID, db.StatusKilled, ops.TimeoutNormal, attribution)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		message := result.Message
		if message == "" {
			message = fmt.Sprintf("Job %s updated", ids.FormatJobID(jobID))
		}
		fmt.Println(message)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
