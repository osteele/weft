package cmd

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

var (
	describeMessage   string
	describeDirectory string
	describeCommand   string
	describeGPU       string
	describeGPUs      string
	describeGPUMem    int
	describeCPU       int
	describeGPUClass  string
)

var describeCmd = &cobra.Command{
	Use:   "describe <job-id>",
	Short: "Set or update job metadata",
	Long: `Set or update the description, directory, command, GPU, or resource allotments of a job.

For queued jobs, you can also update the working directory, command, GPU, and resource
allotments (GPU memory, CPU). The remote queue file will be updated automatically.

Examples:
  weft describe 42 -m "Training GPT-2 with lr=0.001"
  weft describe 42 -m ""  # Clear description
  weft describe 42 --directory /new/path
  weft describe 42 --command "python train.py --epochs 100"
  weft describe 42 --gpu 1              # Set CUDA_VISIBLE_DEVICES=1
  weft describe 42 --gpus 0,1           # Set CUDA_VISIBLE_DEVICES=0,1
  weft describe 42 --gpu-mem 12          # Reserve 12 GB GPU memory per device
  weft describe 42 --cpu 50              # Set CPU allotment to 50%
  weft describe 42 -m "New desc" --command "python new.py"`,
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
	describeCmd.Flags().IntVar(&describeGPUMem, "gpu-mem", 0, "Set GPU memory reservation in GB per device")
	describeCmd.Flags().IntVar(&describeCPU, "cpu", 0, "Set CPU allotment percent")
	describeCmd.Flags().StringVar(&describeGPUClass, "gpu-class", "", "Set GPU class (e.g., A100); scheduler picks best available device")
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

	// Handle non-numeric --gpu as GPU class (e.g., "A100")
	gpuClassValue := describeGPUClass
	if gpuValue != "" && !isNumericGPU(gpuValue) {
		gpuClassValue = gpuValue
		gpuValue = ""
	}

	hasGPUMem := cmd.Flags().Changed("gpu-mem")
	hasCPU := cmd.Flags().Changed("cpu")
	hasGPUClass := gpuClassValue != ""

	// Check if trying to update command/directory/gpu/allotments on non-queued job
	effectiveStatus := job.EffectiveStatus()
	if (describeCommand != "" || describeDirectory != "" || gpuValue != "" || hasGPUClass || hasGPUMem || hasCPU) && effectiveStatus != db.StatusQueued {
		return fmt.Errorf("can only update command/directory/gpu/allotments on queued jobs (job %d has status: %s)", jobID, effectiveStatus)
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

	// Update GPU class if provided (queued jobs only)
	if hasGPUClass {
		if err := db.SetJobGPUClass(database, jobID, gpuClassValue); err != nil {
			return fmt.Errorf("update GPU class: %w", err)
		}
		// Clear explicit GPU when switching to class-based scheduling
		if job.GPU != "" {
			if err := db.SetJobGPU(database, jobID, ""); err != nil {
				return fmt.Errorf("clear GPU field: %w", err)
			}
			job.GPU = ""
		}
		job.GPUClass = gpuClassValue
		updates = append(updates, fmt.Sprintf("gpu-class: %s", gpuClassValue))
	}

	// Update GPU memory reservation if provided (queued jobs only)
	if hasGPUMem {
		mem := describeGPUMem
		var memPtr *int
		if mem > 0 {
			memPtr = &mem
		}
		if err := db.SetJobGPUMemGB(database, jobID, memPtr); err != nil {
			return fmt.Errorf("update GPU memory: %w", err)
		}
		job.GPUMemGB = memPtr
		if memPtr != nil {
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB", mem))
		} else {
			updates = append(updates, "gpu-mem: cleared")
		}
	}

	// Update CPU allotment if provided (queued jobs only)
	if hasCPU {
		cpu := describeCPU
		var cpuPtr *int
		if cpu > 0 {
			cpuPtr = &cpu
		}
		if err := db.SetJobCPUAllotment(database, jobID, cpuPtr); err != nil {
			return fmt.Errorf("update CPU allotment: %w", err)
		}
		job.CPUAllotment = cpuPtr
		if cpuPtr != nil {
			updates = append(updates, fmt.Sprintf("cpu: %d%%", cpu))
		} else {
			updates = append(updates, "cpu: cleared")
		}
	}

	// If we updated command, directory, GPU, or allotments, sync to remote queue file
	if describeCommand != "" || describeDirectory != "" || gpuValue != "" || hasGPUClass || hasGPUMem || hasCPU {
		result, err := ops.RequestQueueUpdate(database, job, ops.DefaultOptions())
		if err != nil {
			return err
		}
		if result.Deferred {
			fmt.Printf("Note: remote host not reachable; changes will be applied automatically when the host is reachable\n")
		}
		if syncErr := syncHostAfterQueueChange(database, job.Host); syncErr != nil && !result.Deferred {
			reportQueueChangeSyncFailure(job.Host, syncErr)
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
