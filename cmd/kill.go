package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <job-id>...",
	Short: "Kill one or more running jobs",
	Long: `Kill running jobs by their IDs.

Examples:
  remote-jobs kill 42
  remote-jobs kill 42 43 44`,
	Args: cobra.MinimumNArgs(1),
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

	var errors []string
	for _, arg := range args {
		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		if job == nil {
			errors = append(errors, fmt.Sprintf("job %d not found", jobID))
			continue
		}

		tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
		fmt.Printf("Killing job %d on %s...\n", jobID, job.Host)

		if err := ssh.TmuxKillSession(job.Host, tmuxSession); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: kill session: %v", jobID, err))
			continue
		}

		// Mark job as dead in database
		if err := db.MarkDeadByID(database, jobID); err != nil {
			fmt.Printf("Warning: job %d: failed to update database: %v\n", jobID, err)
		}

		fmt.Printf("Job %d killed\n", jobID)
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}
