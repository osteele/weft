package cmd

import (
	"fmt"
	"regexp"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var (
	describeMessage   string
	describeProject   string
	describeDirectory string
	describeCommand   string
	describeGPU       string
	describeGPUs      string
	describeGPUMem    int
	describeCPU       int
	describeGPUClass  string
	describeProvider  string
)

var describeCmd = &cobra.Command{
	Use:   "describe <job-id>",
	Short: "Set or update job metadata",
	Long: `Set or update the description, directory, command, GPU, or resource allotments of a job.

For queued and draft jobs, you can also update the working directory, command, GPU, and resource
allotments (GPU memory, CPU). Queued jobs that are already placed on a host update the remote queue file automatically.

Examples:
  weft describe wj42 -m "Training GPT-2 with lr=0.001"
  weft describe wj42 -m ""  # Clear description
  weft describe wj42 --directory /new/path
  weft describe wj42 --command "python train.py --epochs 100"
  weft describe wj42 --gpu 1              # Set CUDA_VISIBLE_DEVICES=1
  weft describe wj42 --gpus 0,1           # Set CUDA_VISIBLE_DEVICES=0,1
  weft describe wj42 --gpu-mem 12          # Reserve 12 GB GPU memory per device
  weft describe wj42 --cpu 50              # Set CPU allotment to 50%
  weft describe wj42 -m "New desc" --command "python new.py"`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runDescribe,
}

func init() {
	// Removed: Describe command is now only available as `job describe`
	// rootCmd.AddCommand(describeCmd)
	describeCmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	describeCmd.Flags().StringVar(&describeProject, "project", "", "Set project name")
	describeCmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory")
	describeCmd.Flags().StringVar(&describeCommand, "command", "", "Set command")
	describeCmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU: device index, class, or class>=NGB (e.g., 1, a100, nvidia>=24GB)")
	describeCmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs (CUDA_VISIBLE_DEVICES)")
	describeCmd.Flags().IntVar(&describeGPUMem, "gpu-mem", 0, "Set GPU memory reservation in GB per device")
	describeCmd.Flags().IntVar(&describeCPU, "cpu", 0, "Set CPU allotment percent")
	describeCmd.Flags().StringVar(&describeGPUClass, "gpu-class", "", "GPU class or generation (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	describeCmd.Flags().StringVar(&describeProvider, "provider", "", "Cloud provider preference for rental placement (vastai or runpod)")
}

func runDescribe(cmd *cobra.Command, args []string) error {
	jobID, err := ids.ParseJobID(args[0])
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
		return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if job.Backend == db.BackendSkyPilot {
		for _, flag := range []string{"directory", "command", "gpu", "gpus", "gpu-mem", "cpu", "gpu-class", "provider"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("SkyPilot execution fields cannot be changed by Weft; submit a new external job instead")
			}
		}
	}

	// Normalize GPU flags (--gpu and --gpus are aliases)
	gpuValue := describeGPU
	if describeGPUs != "" {
		gpuValue = describeGPUs
	}

	// Handle non-numeric --gpu as GPU class, with optional >=NGB syntax
	gpuClassValue := describeGPUClass
	if gpuValue != "" && !isNumericGPU(gpuValue) {
		parsedClass, parsedMem, err := parseGPUFlag(gpuValue)
		if err != nil {
			return fmt.Errorf("--gpu: %w", err)
		}
		gpuClassValue = parsedClass
		if parsedMem > 0 {
			if describeGPUMem > 0 {
				return fmt.Errorf("--gpu with >=NGB and --gpu-mem cannot be used together")
			}
			describeGPUMem = parsedMem
		}
		gpuValue = ""
	}

	hasGPUMem := cmd.Flags().Changed("gpu-mem")
	hasCPU := cmd.Flags().Changed("cpu")
	hasGPUClass := gpuClassValue != ""
	hasProvider := cmd.Flags().Changed("provider")

	// Check if trying to update command/directory/gpu/allotments on a job that
	// may already have run.
	effectiveStatus := job.EffectiveStatus()
	if (describeProject != "" || describeCommand != "" || describeDirectory != "" || gpuValue != "" || hasGPUClass || hasGPUMem || hasCPU || hasProvider) && effectiveStatus != db.StatusQueued && effectiveStatus != db.StatusDraft {
		return fmt.Errorf("can only update command/directory/project/gpu/allotments on queued or draft jobs (job %s has status: %s)", ids.FormatJobID(jobID), effectiveStatus)
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

	// Update working directory if provided.
	if describeDirectory != "" {
		if err := db.UpdateJobWorkingDir(database, jobID, describeDirectory); err != nil {
			return fmt.Errorf("update directory: %w", err)
		}
		job.WorkingDir = describeDirectory
		updates = append(updates, fmt.Sprintf("directory: %s", describeDirectory))
	}

	// Update command if provided.
	if describeCommand != "" {
		if err := db.UpdateJobCommand(database, jobID, describeCommand); err != nil {
			return fmt.Errorf("update command: %w", err)
		}
		job.Command = describeCommand
		updates = append(updates, fmt.Sprintf("command: %s", describeCommand))
	}

	if cmd.Flags().Changed("project") || describeDirectory != "" || describeCommand != "" {
		projectName := describeProject
		if !cmd.Flags().Changed("project") {
			var err error
			projectName, err = workdir.ResolveProjectName("", job.WorkingDir)
			if err != nil {
				return fmt.Errorf("resolve project: %w", err)
			}
		}
		if err := db.SetJobProject(database, jobID, projectName); err != nil {
			return fmt.Errorf("update project: %w", err)
		}
		job.Project = projectName
		if projectName == "" {
			updates = append(updates, "project cleared")
		} else {
			updates = append(updates, fmt.Sprintf("project: %s", projectName))
		}
	}

	// Update GPU (CUDA_VISIBLE_DEVICES) if provided.
	if gpuValue != "" {
		// Update the GPU field in database
		if err := db.SetJobGPU(database, jobID, gpuValue); err != nil {
			return fmt.Errorf("update GPU field: %w", err)
		}
		job.GPU = gpuValue
		if err := db.SetJobGPUClass(database, jobID, ""); err != nil {
			return fmt.Errorf("clear GPU class: %w", err)
		}
		job.GPUClass = ""
		if err := setJobCLIGPUOverride(database, job, gpuValue); err != nil {
			return fmt.Errorf("update GPU override: %w", err)
		}

		// Also update the command to include CUDA_VISIBLE_DEVICES for backwards compatibility
		newCommand := updateCudaVisibleDevices(job.Command, gpuValue)
		if err := db.UpdateJobCommand(database, jobID, newCommand); err != nil {
			return fmt.Errorf("update command with GPU: %w", err)
		}
		job.Command = newCommand
		updates = append(updates, fmt.Sprintf("gpu: %s", gpuValue))
	}

	// Update GPU class if provided.
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
		if err := setJobCLIGPUClassOverride(database, job, gpuClassValue); err != nil {
			return fmt.Errorf("update GPU class override: %w", err)
		}
		updates = append(updates, fmt.Sprintf("gpu-class: %s", gpuClassValue))
	}

	// Update GPU memory reservation if provided.
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
		if err := setJobCLIGPUMemOverride(database, job, memPtr); err != nil {
			return fmt.Errorf("update GPU memory override: %w", err)
		}
		if memPtr != nil {
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB", mem))
		} else {
			updates = append(updates, "gpu-mem: cleared")
		}
	}

	// Update CPU allotment if provided.
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
	if hasProvider {
		normalizedProvider, providerErr := normalizeProviderFlag(describeProvider)
		if providerErr != nil {
			return fmt.Errorf("--provider: %w", providerErr)
		}
		if normalizedProvider != "" && job.HasInventoryHost() {
			return fmt.Errorf("--provider=%s cannot be set while job is assigned to inventory host %q; unplace the job first", normalizedProvider, job.Host)
		}
		newTags, providerErr := withProviderTag(job.Tags, normalizedProvider)
		if providerErr != nil {
			return fmt.Errorf("--provider: %w", providerErr)
		}
		if err := db.SetJobTags(database, jobID, newTags); err != nil {
			return fmt.Errorf("update provider tag: %w", err)
		}
		job.Tags = newTags
		if normalizedProvider == "" {
			updates = append(updates, "provider: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("provider: %s", normalizedProvider))
		}
	}

	needsRemoteUpdate := effectiveStatus == db.StatusQueued && job.Host != "" &&
		(hasDescription || cmd.Flags().Changed("project") || describeCommand != "" || describeDirectory != "" || gpuValue != "" || hasGPUClass || hasGPUMem || hasCPU)
	if needsRemoteUpdate {
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
		fmt.Printf("No changes made to job %s\n", ids.FormatJobID(jobID))
	} else {
		fmt.Printf("Updated job %s:\n", ids.FormatJobID(jobID))
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
