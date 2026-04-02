package cmd

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:     "restart <job-id>...",
	Aliases: []string{"retry"},
	Short:   "Restart a killed, dead, failed, canceled, or completed job",
	Long: `Restart a job by requeuing it with the same ID.

The previous run is archived and the job is reset to queued status.

Examples:
  weft restart 42
  weft retry 42
  weft restart 42 43 44`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runRestart,
}

var (
	restartGPU      string
	restartGPUClass string
	restartGPUMem   int
)

type restartOverrides struct {
	GPU       string
	GPUClass  string
	GPUMemGB  *int
	HasAny    bool
	HasGPUMem bool
}

func init() {
	rootCmd.AddCommand(restartCmd)
	addRestartFlags(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		return err
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

	// Sync non-terminal jobs before restarting to get latest cloud status
	var jobsToSync []*db.Job
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil {
			continue
		}
		jobsToSync = append(jobsToSync, job)
	}
	if len(jobsToSync) > 0 {
		quickSyncJobs(database, jobsToSync, FastSyncTimeout, FastCloudSyncTimeout)
	}

	var errors []string
	for _, jobID := range jobIDs {
		if err := restartJob(database, jobID, overrides); err != nil {
			errors = append(errors, fmt.Sprintf("job %d: %v", jobID, err))
		}
	}

	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Fprintln(os.Stderr, e)
		}
		return fmt.Errorf("%d job(s) could not be restarted", len(errors))
	}
	return nil
}

func addRestartFlags(command *cobra.Command) {
	command.Flags().StringVar(&restartGPU, "gpu", "", "GPU constraint override: device index, class, or class>=NGB (e.g., 1, a100, nvidia>=24GB)")
	command.Flags().StringVar(&restartGPUClass, "gpu-class", "", "GPU class or generation override (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	command.Flags().IntVar(&restartGPUMem, "gpu-mem", 0, "GPU memory reservation override in GB per device (0 clears)")
}

func parseRestartOverrides(cmd *cobra.Command) (restartOverrides, error) {
	var out restartOverrides
	gpuValue := restartGPU
	gpuClassValue := restartGPUClass
	hasGPUMem := cmd.Flags().Changed("gpu-mem")
	out.HasGPUMem = hasGPUMem

	if gpuValue != "" && gpuClassValue != "" {
		return out, fmt.Errorf("--gpu and --gpu-class cannot be used together")
	}
	if gpuValue != "" && !isNumericGPU(gpuValue) {
		parsedClass, parsedMem, err := parseGPUFlag(gpuValue)
		if err != nil {
			return out, fmt.Errorf("--gpu: %w", err)
		}
		gpuClassValue = parsedClass
		if parsedMem > 0 {
			if hasGPUMem {
				return out, fmt.Errorf("--gpu with >=NGB and --gpu-mem cannot be used together")
			}
			restartGPUMem = parsedMem
			hasGPUMem = true
			out.HasGPUMem = true
		}
		gpuValue = ""
	}

	if hasGPUMem {
		mem := restartGPUMem
		if mem > 0 {
			out.GPUMemGB = &mem
		} else {
			out.GPUMemGB = nil
		}
	}
	out.GPU = gpuValue
	out.GPUClass = gpuClassValue
	out.HasAny = out.GPU != "" || out.GPUClass != "" || hasGPUMem
	return out, nil
}

func applyRestartOverrides(database *sql.DB, job *db.Job, overrides restartOverrides) ([]string, error) {
	updates, err := applyScriptGPUDefaults(database, job)
	if err != nil {
		return nil, err
	}

	if !overrides.HasAny {
		return updates, nil
	}
	if overrides.GPU != "" {
		if err := db.SetJobGPU(database, job.ID, overrides.GPU); err != nil {
			return nil, fmt.Errorf("update gpu: %w", err)
		}
		job.GPU = overrides.GPU
		updates = append(updates, fmt.Sprintf("gpu: %s", overrides.GPU))
	}
	if overrides.GPUClass != "" {
		if err := db.SetJobGPUClass(database, job.ID, overrides.GPUClass); err != nil {
			return nil, fmt.Errorf("update gpu-class: %w", err)
		}
		if job.GPU != "" {
			if err := db.SetJobGPU(database, job.ID, ""); err != nil {
				return nil, fmt.Errorf("clear gpu: %w", err)
			}
			job.GPU = ""
		}
		job.GPUClass = overrides.GPUClass
		updates = append(updates, fmt.Sprintf("gpu-class: %s", overrides.GPUClass))
	}
	if overrides.HasGPUMem {
		if err := db.SetJobGPUMemGB(database, job.ID, overrides.GPUMemGB); err != nil {
			return nil, fmt.Errorf("update gpu-mem: %w", err)
		}
		job.GPUMemGB = overrides.GPUMemGB
		if overrides.GPUMemGB == nil {
			updates = append(updates, "gpu-mem: cleared")
		} else {
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB", *overrides.GPUMemGB))
		}
	}
	return updates, nil
}

func applyScriptGPUDefaults(database *sql.DB, job *db.Job) ([]string, error) {
	localDir := workdir.ResolveLocal(job.WorkingDir)
	meta, err := dataloc.ScanScriptMeta(localDir, job.Command)
	if err != nil {
		slog.Warn("script metadata error", "error", err)
		return nil, nil
	}
	if meta == nil {
		return nil, nil
	}

	var updates []string
	// Match run semantics: gpu takes precedence over gpu-class when both are present.
	metaGPU := meta.GPU
	metaGPUClass := meta.GPUClass
	metaGPUMem := meta.GPUMemGB

	if metaGPU != "" {
		if !isNumericGPU(metaGPU) {
			parsedClass, parsedMem, err := parseGPUFlag(metaGPU)
			if err != nil {
				return nil, fmt.Errorf("parse script metadata gpu: %w", err)
			}
			metaGPU = ""
			metaGPUClass = parsedClass
			if parsedMem > 0 {
				metaGPUMem = parsedMem
			}
		}
	}
	if metaGPU != "" {
		if job.GPU != metaGPU {
			if err := db.SetJobGPU(database, job.ID, metaGPU); err != nil {
				return nil, fmt.Errorf("update gpu from script metadata: %w", err)
			}
			job.GPU = metaGPU
			updates = append(updates, fmt.Sprintf("gpu: %s (from script metadata)", metaGPU))
		}
	}
	if metaGPU == "" && metaGPUClass != "" {
		if !strings.EqualFold(job.GPUClass, metaGPUClass) {
			if err := db.SetJobGPUClass(database, job.ID, metaGPUClass); err != nil {
				return nil, fmt.Errorf("update gpu-class from script metadata: %w", err)
			}
			job.GPUClass = metaGPUClass
			updates = append(updates, fmt.Sprintf("gpu-class: %s (from script metadata)", metaGPUClass))
		}
		if job.GPU != "" {
			if err := db.SetJobGPU(database, job.ID, ""); err != nil {
				return nil, fmt.Errorf("clear gpu from script metadata: %w", err)
			}
			job.GPU = ""
		}
	}
	if metaGPUMem > 0 {
		if job.GPUMemGB == nil || *job.GPUMemGB != metaGPUMem {
			mem := metaGPUMem
			if err := db.SetJobGPUMemGB(database, job.ID, &mem); err != nil {
				return nil, fmt.Errorf("update gpu-mem from script metadata: %w", err)
			}
			job.GPUMemGB = &mem
			updates = append(updates, fmt.Sprintf("gpu-mem: %d GB (from script metadata)", mem))
		}
	}
	return updates, nil
}

func restartJob(database *sql.DB, jobID int64, overrides restartOverrides) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return db.ErrJobNotFound
	}

	// Validate job can be retried
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusQueued {
		updates, err := applyRestartOverrides(database, job, overrides)
		if err != nil {
			return err
		}
		if len(updates) == 0 {
			fmt.Printf("Job %d is already queued (no changes)\n", jobID)
			return nil
		}
		fmt.Printf("Updated queued job %d\n", jobID)
		for _, update := range updates {
			fmt.Printf("  %s\n", update)
		}
		return nil
	}
	if effectiveStatus == db.StatusRunning || effectiveStatus == db.StatusStarting {
		return fmt.Errorf("job is currently %s; kill it first if you want to retry", effectiveStatus)
	}

	if job.Command == "" {
		return fmt.Errorf("job missing command")
	}

	updates, err := applyRestartOverrides(database, job, overrides)
	if err != nil {
		return err
	}

	// Remove processed tag so the retried job appears in unprocessed listings
	if job.HasTag(db.ProcessedTag) {
		if err := db.RemoveJobTag(database, jobID, db.ProcessedTag); err != nil {
			return fmt.Errorf("remove processed tag: %w", err)
		}
	}

	// Cloud jobs: reset to unplaced (the original instance is gone)
	if job.IsLaunchJob() {
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			return err
		}
		if err := db.ResetJobToUnplaced(database, jobID); err != nil {
			return err
		}
		fmt.Printf("Reset job %d to queued (cloud instance no longer available)\n", jobID)
		fmt.Printf("  Use 'weft launch instances' to run on a new instance\n")
		for _, update := range updates {
			fmt.Printf("  %s\n", update)
		}
		return nil
	}

	if !job.HasInventoryHost() {
		return fmt.Errorf("job missing host")
	}

	// Requeue with same ID (archives the previous run)
	if !requeueableStatuses[effectiveStatus] {
		return fmt.Errorf("cannot retry job with status '%s'; only killed/dead/failed/canceled/completed jobs can be retried", effectiveStatus)
	}

	oldStatus := job.Status

	cfg, relayClient, err := loadCoordinatorRelay()
	if err != nil {
		return err
	}
	if relayEnabled(cfg, relayClient) {
		// Refresh here since the relay path bypasses ops.RequeueJob (which does its own refresh).
		if err := ops.RefreshProjectDerivedMetadata(database, job.ID, job.WorkingDir, job.Command, job.Inputs); err != nil {
			slog.Warn("failed to refresh metadata", "error", err)
		}
		if err := db.RequeueByID(database, jobID); err != nil {
			return fmt.Errorf("update status to queued: %w", err)
		}
		ack, err := relayRequeueJob(cfg, relayClient, job)
		if err != nil {
			return err
		}
		fmt.Printf("Restarted job %d via coordinator relay\n", jobID)
		fmt.Printf("  Status: %s → queued\n", oldStatus)
		if ack != nil && ack.Message != "" {
			fmt.Printf("  relay: %s\n", ack.Message)
		}
		return nil
	}

	result, err := ops.RequeueJob(database, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	if result.Deferred {
		fmt.Printf("Job saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", job.Host)
	}

	fmt.Printf("Restarted job %d on %s\n", jobID, job.Host)
	fmt.Printf("  Status: %s → queued\n", oldStatus)
	for _, update := range updates {
		fmt.Printf("  %s\n", update)
	}
	if job.Description != "" {
		fmt.Printf("  Description: %s\n", job.Description)
	}
	if len(job.EnvVars) > 0 {
		fmt.Printf("  Env vars: %s\n", strings.Join(job.EnvVars, ", "))
	}
	return nil
}
