package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var (
	describeDirectory string
	describeCommand   string
)

var describeCmd = &cobra.Command{
	Use:   "describe <job-id> [description]",
	Short: "Set or update job metadata",
	Long: `Set or update the description, directory, or command of a job.

For queued jobs, you can also update the working directory and command.
The remote queue file will be updated automatically.

Examples:
  remote-jobs describe 42 "Training GPT-2 with lr=0.001"
  remote-jobs describe 42 ""  # Clear description
  remote-jobs describe 42 --directory /new/path
  remote-jobs describe 42 --command "python train.py --epochs 100"
  remote-jobs describe 42 -d "New desc" --command "python new.py"`,
	Args: usageArgs(cobra.RangeArgs(1, 2)),
	RunE: runDescribe,
}

func init() {
	// Removed: Describe command is now only available as `job describe`
	// rootCmd.AddCommand(describeCmd)
	describeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	describeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command (queued jobs only)")
}

func runDescribe(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	// Description is optional second argument
	description := ""
	hasDescription := len(args) > 1
	if hasDescription {
		description = args[1]
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Check job exists
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Check if trying to update command/directory on non-queued job
	if (describeCommand != "" || describeDirectory != "") && job.Status != db.StatusQueued {
		return fmt.Errorf("can only update command/directory on queued jobs (job %d has status: %s)", jobID, job.Status)
	}

	// Track what was updated
	var updates []string

	// Update description if provided
	if hasDescription {
		if err := db.UpdateJobDescription(database, jobID, description); err != nil {
			return fmt.Errorf("update description: %w", err)
		}
		if description == "" {
			updates = append(updates, "cleared description")
		} else {
			updates = append(updates, fmt.Sprintf("description: %s", description))
		}
	}

	// Update working directory if provided (queued jobs only)
	if describeDirectory != "" {
		if err := db.UpdateJobWorkingDir(database, jobID, describeDirectory); err != nil {
			return fmt.Errorf("update directory: %w", err)
		}
		job.WorkingDir = describeDirectory
		updates = append(updates, fmt.Sprintf("directory: %s", describeDirectory))
	}

	// Update command if provided (queued jobs only)
	if describeCommand != "" {
		if err := db.UpdateJobCommand(database, jobID, describeCommand); err != nil {
			return fmt.Errorf("update command: %w", err)
		}
		job.Command = describeCommand
		updates = append(updates, fmt.Sprintf("command: %s", describeCommand))
	}

	// If we updated command or directory, also update the remote queue file
	if describeCommand != "" || describeDirectory != "" {
		queueName := job.QueueName
		if queueName == "" {
			queueName = "default"
		}
		if err := updateRemoteQueueEntry(job.Host, queueName, job); err != nil {
			fmt.Printf("Warning: could not update remote queue file: %v\n", err)
			fmt.Printf("The local database was updated. Run 'remote-jobs sync' when host is reachable.\n")
		}
	}

	if len(updates) == 0 {
		fmt.Printf("No changes made to job %d\n", jobID)
	} else {
		fmt.Printf("Updated job %d:\n", jobID)
		for _, u := range updates {
			fmt.Printf("  %s\n", u)
		}
	}

	return nil
}

// updateRemoteQueueEntry updates a job's entry in the remote queue file
func updateRemoteQueueEntry(host, queueName string, job *db.Job) error {
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)

	// Remove old entry and add new one
	removeCmd := fmt.Sprintf("sed -i '/^%d\\t/d' %s 2>/dev/null || true", job.ID, queueFile)
	if _, stderr, err := ssh.Run(host, removeCmd); err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("remove old entry: %s", strings.TrimSpace(stderr))
	}

	// Add updated entry
	queueLine := fmt.Sprintf("%d\t%s\t%s\t%s", job.ID, job.WorkingDir, job.Command, job.Description)
	addCmd := fmt.Sprintf("echo '%s' >> %s", ssh.EscapeForSingleQuotes(queueLine), queueFile)
	if _, stderr, err := ssh.Run(host, addCmd); err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("add updated entry: %s", strings.TrimSpace(stderr))
	}

	return nil
}
