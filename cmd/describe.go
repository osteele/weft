package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var (
	describeMessage   string
	describeDirectory string
	describeCommand   string
	describeGPU       string
	describeGPUs      string
)

type deferredUpdatePayload struct {
	WorkingDir  string `json:"working_dir,omitempty"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
}

var describeCmd = &cobra.Command{
	Use:   "describe <job-id>",
	Short: "Set or update job metadata",
	Long: `Set or update the description, directory, command, or GPU of a job.

For queued jobs, you can also update the working directory, command, and GPU.
The remote queue file will be updated automatically.

Examples:
  remote-jobs describe 42 -m "Training GPT-2 with lr=0.001"
  remote-jobs describe 42 -m ""  # Clear description
  remote-jobs describe 42 --directory /new/path
  remote-jobs describe 42 --command "python train.py --epochs 100"
  remote-jobs describe 42 --gpu 1              # Set CUDA_VISIBLE_DEVICES=1
  remote-jobs describe 42 --gpus 0,1           # Set CUDA_VISIBLE_DEVICES=0,1
  remote-jobs describe 42 -m "New desc" --command "python new.py"`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runDescribe,
}

func init() {
	// Removed: Describe command is now only available as `job describe`
	// rootCmd.AddCommand(describeCmd)
	describeCmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	describeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory (queued jobs only)")
	describeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command (queued jobs only)")
	describeCmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU (CUDA_VISIBLE_DEVICES) - queued jobs only")
	describeCmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs (CUDA_VISIBLE_DEVICES) - queued jobs only")
}

func runDescribe(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	// Description from -m flag
	description := describeMessage
	hasDescription := cmd.Flags().Changed("message")

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

	// Normalize GPU flags (--gpu and --gpus are aliases)
	gpuValue := describeGPU
	if describeGPUs != "" {
		gpuValue = describeGPUs
	}

	// Check if trying to update command/directory/gpu on non-queued job
	if (describeCommand != "" || describeDirectory != "" || gpuValue != "") && job.Status != db.StatusQueued {
		return fmt.Errorf("can only update command/directory/gpu on queued jobs (job %d has status: %s)", jobID, job.Status)
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

	// Update GPU (CUDA_VISIBLE_DEVICES) if provided (queued jobs only)
	if gpuValue != "" {
		// Update the GPU field in database
		if err := db.SetJobGPU(database, jobID, gpuValue); err != nil {
			return fmt.Errorf("update GPU field: %w", err)
		}
		job.GPU = gpuValue

		// Also update the command to include CUDA_VISIBLE_DEVICES for backwards compatibility
		newCommand := updateCudaVisibleDevices(job.Command, gpuValue)
		if err := db.UpdateJobCommand(database, jobID, newCommand); err != nil {
			return fmt.Errorf("update command with GPU: %w", err)
		}
		job.Command = newCommand
		updates = append(updates, fmt.Sprintf("gpu: %s", gpuValue))
	}

	// If we updated command, directory, or GPU, sync to remote queue file
	if describeCommand != "" || describeDirectory != "" || gpuValue != "" {
		queueName := job.QueueName
		if queueName == "" {
			queueName = "default"
		}

		// Check if job has a pending queue_job operation (not yet synced to remote)
		hasPendingQueue, _ := db.HasPendingOperation(database, jobID, db.OpQueueJob)
		if hasPendingQueue {
			// Update the pending queue_job payload with new values
			existingPayload, err := db.GetDeferredOperationPayload(database, jobID, db.OpQueueJob)
			if err == nil && existingPayload != "" {
				var payload map[string]interface{}
				if err := json.Unmarshal([]byte(existingPayload), &payload); err == nil {
					payload["working_dir"] = job.WorkingDir
					payload["command"] = job.Command
					payload["description"] = job.Description
					if newPayloadJSON, err := json.Marshal(payload); err == nil {
						db.UpdatePendingOperationPayload(database, jobID, db.OpQueueJob, string(newPayloadJSON))
					}
				}
			}
		} else {
			// Job is already in remote queue - add update operation and try immediate sync
			payload := deferredUpdatePayload{
				WorkingDir:  job.WorkingDir,
				Command:     job.Command,
				Description: job.Description,
			}
			payloadJSON, _ := json.Marshal(payload)
			if err := db.AddDeferredOperation(database, job.Host, db.OpUpdateQueuedJob, jobID, queueName, string(payloadJSON)); err != nil {
				fmt.Printf("Warning: could not queue update operation: %v\n", err)
			}

			// Try immediate sync
			if err := updateRemoteQueueEntry(job.Host, queueName, job); err != nil {
				fmt.Printf("Note: remote host not reachable; changes will sync when host is available\n")
			} else {
				// Immediate sync succeeded - remove the deferred operation
				db.DeletePendingOperation(database, jobID, db.OpUpdateQueuedJob)
			}
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

	// Remove old entry and add new one (under flock to prevent race with queue runner)
	lockFile := queueFile + ".lock"
	queueLine := fmt.Sprintf("%d\t%s\t%s\t%s", job.ID, job.WorkingDir, job.Command, job.Description)
	updateCmd := fmt.Sprintf("flock %s bash -c \"sed -i '/^%d\\t/d' %s 2>/dev/null || true; echo '%s' >> %s\"",
		lockFile, job.ID, queueFile, ssh.EscapeForSingleQuotes(queueLine), queueFile)
	if _, stderr, err := ssh.Run(host, updateCmd); err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host unreachable")
		}
		return fmt.Errorf("update queue entry: %s", strings.TrimSpace(stderr))
	}

	return nil
}

// updateCudaVisibleDevices updates or adds CUDA_VISIBLE_DEVICES to a command
func updateCudaVisibleDevices(cmd, gpuValue string) string {
	// Pattern to match CUDA_VISIBLE_DEVICES=X at the start of the command
	// Handles both "CUDA_VISIBLE_DEVICES=0 cmd" and "export CUDA_VISIBLE_DEVICES=0 && cmd"
	cudaPattern := regexp.MustCompile(`^(export\s+)?CUDA_VISIBLE_DEVICES=[^\s]+(\s+&&\s+|\s+)`)

	if cudaPattern.MatchString(cmd) {
		// Replace existing CUDA_VISIBLE_DEVICES
		return cudaPattern.ReplaceAllString(cmd, fmt.Sprintf("CUDA_VISIBLE_DEVICES=%s ", gpuValue))
	}

	// Prepend CUDA_VISIBLE_DEVICES to the command
	return fmt.Sprintf("CUDA_VISIBLE_DEVICES=%s %s", gpuValue, cmd)
}
