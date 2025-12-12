package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <job-id>",
	Short: "Kill a running job",
	Long: `Kill a running job by its ID.

Example:
  remote-jobs kill 42`,
	Args: cobra.ExactArgs(1),
	RunE: runKill,
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	tmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	fmt.Printf("Killing job %d on %s...\n", jobID, job.Host)

	if err := ssh.TmuxKillSession(job.Host, tmuxSession); err != nil {
		return fmt.Errorf("kill session: %w", err)
	}

	// Mark job as dead in database
	if err := db.MarkDeadByID(database, jobID); err != nil {
		fmt.Printf("Warning: failed to update database: %v\n", err)
	}

	fmt.Println("Job killed")
	return nil
}
