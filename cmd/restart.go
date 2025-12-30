package cmd

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <job-id>...",
	Short: "Restart one or more jobs using saved metadata",
	Long: `Restart jobs using their saved metadata or database info.

This kills the existing session (if any) and starts a new one
with the same command and working directory. Creates a new job ID for each.

Examples:
  remote-jobs restart 42
  remote-jobs restart 42 43 44

Also available as: remote-jobs job restart`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errors []string
	for i, arg := range args {
		if i > 0 {
			fmt.Println("---")
		}

		jobID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid job ID %s", arg))
			continue
		}

		if err := restartSingleJob(database, jobID); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errors, "; "))
	}
	return nil
}

func restartSingleJob(database *sql.DB, jobID int64) error {
	// Get job from database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("not found")
	}

	// Read metadata from remote (for additional info - best effort)
	metadataFile := session.JobMetadataFile(job.ID, job.StartTime, job.SessionName)
	content, _ := ssh.ReadRemoteFile(job.Host, metadataFile)

	workingDir := job.WorkingDir
	command := job.Command
	description := job.Description

	if content != "" {
		metadata := session.ParseMetadata(content)
		if metadata["working_dir"] != "" {
			workingDir = metadata["working_dir"]
		}
		if metadata["command"] != "" {
			command = metadata["command"]
		}
		if metadata["description"] != "" && description == "" {
			description = metadata["description"]
		}
	}

	if workingDir == "" || command == "" {
		return fmt.Errorf("missing working directory or command")
	}

	fmt.Printf("Restarting job %d on %s\n", jobID, job.Host)
	fmt.Printf("Working directory: %s\n", workingDir)
	fmt.Printf("Command: %s\n", command)
	if description != "" {
		fmt.Printf("Description: %s\n", description)
	}

	// Kill existing session if running (best effort)
	oldTmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, _ := ssh.TmuxSessionExistsQuick(job.Host, oldTmuxSession)
	if exists {
		fmt.Printf("Killing existing session...\n")
		ssh.TmuxKillSession(job.Host, oldTmuxSession)
	}

	// Use unified ops package for restarting jobs
	result, err := ops.RestartJob(database, ops.RestartJobParams{
		OriginalJob: job,
		WorkingDir:  workingDir,
		Command:     command,
		Description: description,
	}, ops.DefaultOptions())

	if err != nil {
		return err
	}

	if result.Deferred {
		fmt.Printf("Host %s unreachable, job will start on next sync\n", job.Host)
		fmt.Printf("New job ID: %d (queued)\n", result.JobID)
	} else {
		fmt.Println("✓ Job restarted successfully")
		fmt.Printf("New job ID: %d\n", result.JobID)
	}

	return nil
}

// Helper for parsing integer from metadata
func parseMetadataInt(metadata map[string]string, key string) int64 {
	if val, ok := metadata[key]; ok {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return 0
}
