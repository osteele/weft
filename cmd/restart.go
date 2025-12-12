package cmd

import (
	"fmt"
	"strconv"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <job-id>",
	Short: "Restart a job using saved metadata",
	Long: `Restart a job using its saved metadata or database info.

This kills the existing session (if any) and starts a new one
with the same command and working directory. Creates a new job ID.

Example:
  remote-jobs restart 42`,
	Args: cobra.ExactArgs(1),
	RunE: runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get job from database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Read metadata from remote (for additional info)
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

	// Kill existing session if running
	oldTmuxSession := session.JobTmuxSession(job.ID, job.SessionName)
	exists, _ := ssh.TmuxSessionExists(job.Host, oldTmuxSession)
	if exists {
		fmt.Printf("Killing existing session...\n")
		if err := ssh.TmuxKillSession(job.Host, oldTmuxSession); err != nil {
			return fmt.Errorf("kill session: %w", err)
		}
	}

	// Create new job record to get ID
	newJobID, err := db.RecordJobStarting(database, job.Host, workingDir, command, description)
	if err != nil {
		return fmt.Errorf("create job record: %w", err)
	}

	// Get the new job to access start time
	newJob, err := db.GetJobByID(database, newJobID)
	if err != nil || newJob == nil {
		return fmt.Errorf("get new job: %w", err)
	}

	// Generate new file paths from job ID
	newTmuxSession := session.TmuxSessionName(newJobID)
	logFile := session.LogFile(newJobID, newJob.StartTime)
	statusFile := session.StatusFile(newJobID, newJob.StartTime)
	newMetadataFile := session.MetadataFile(newJobID, newJob.StartTime)

	// Create log directory on remote
	mkdirCmd := fmt.Sprintf("mkdir -p %s", session.LogDir)
	if _, stderr, err := ssh.RunWithRetry(job.Host, mkdirCmd); err != nil {
		db.UpdateJobFailed(database, newJobID, fmt.Sprintf("create log dir: %s", stderr))
		return fmt.Errorf("create log directory: %s", stderr)
	}

	// Save metadata
	newMetadata := session.FormatMetadata(newJobID, workingDir, command, job.Host, description, newJob.StartTime)
	metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", newMetadataFile, newMetadata)
	ssh.RunWithRetry(job.Host, metadataCmd)

	// Create the wrapped command with better error capture
	wrappedCommand := fmt.Sprintf(
		`echo "=== START $(date) ===" > '%s'; `+
			`echo "job_id: %d" >> '%s'; `+
			`echo "cd: %s" >> '%s'; `+
			`echo "cmd: %s" >> '%s'; `+
			`echo "===" >> '%s'; `+
			`cd '%s' && (%s) 2>&1 | tee -a '%s'; `+
			`EXIT_CODE=\${PIPESTATUS[0]}; `+
			`echo "=== END exit=\$EXIT_CODE $(date) ===" >> '%s'; `+
			`echo \$EXIT_CODE > '%s'`,
		logFile,
		newJobID, logFile,
		workingDir, logFile,
		command, logFile,
		logFile,
		workingDir, command, logFile,
		logFile,
		statusFile)

	// Start tmux session
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", newTmuxSession, wrappedCommand)
	if _, stderr, err := ssh.Run(job.Host, tmuxCmd); err != nil {
		db.UpdateJobFailed(database, newJobID, fmt.Sprintf("start tmux: %s", stderr))
		return fmt.Errorf("start session: %s", stderr)
	}

	// Mark job as running
	if err := db.UpdateJobRunning(database, newJobID); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}

	fmt.Println("✓ Job restarted successfully")
	fmt.Printf("New job ID: %d\n", newJobID)

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
