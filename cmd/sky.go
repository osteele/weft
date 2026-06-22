package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/skypilot"
	"github.com/spf13/cobra"
)

var skyCmd = &cobra.Command{
	Use:   "sky",
	Short: "Mirror and submit SkyPilot jobs through Weft",
	Long: `Mirror SkyPilot jobs into Weft's local ledger.

SkyPilot owns cloud resource selection, cluster lifecycle, retries, and raw task
execution. Weft stores the local job ID, project association, tags, processed
bookkeeping, status cache, and query surfaces.`,
}

var skyImportProject string
var skyImportCWD string
var skySyncProject string

var skySubmitProject string
var skySubmitCWD string
var skySubmitDescription string
var skySubmitGPU string
var skySubmitGPUClass string
var skySubmitGPUCount int
var skySubmitGPUMem int
var skySubmitEnv []string
var skySubmitTags []string
var skySubmitDryRun string

var skyClient = skypilot.Client{}

func init() {
	rootCmd.AddCommand(skyCmd)

	importCmd := &cobra.Command{
		Use:   "import [flags] <sky-job-id>",
		Short: "Import an existing SkyPilot job into Weft",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE:  runSkyImport,
	}
	importCmd.Flags().StringVar(&skyImportProject, "project", "", "Project name for the imported Weft job")
	importCmd.Flags().StringVar(&skyImportCWD, "cwd", "", "Working directory to associate with the imported job")
	skyCmd.AddCommand(importCmd)

	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Refresh mirrored SkyPilot job statuses",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  runSkySync,
	}
	syncCmd.Flags().StringVar(&skySyncProject, "project", "", "Only sync mirrored jobs in this project")
	skyCmd.AddCommand(syncCmd)

	submitCmd := &cobra.Command{
		Use:   "submit [flags] <command>",
		Short: "Submit a SkyPilot managed job and mirror it in Weft",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE:  runSkySubmit,
	}
	submitCmd.Flags().StringVar(&skySubmitProject, "project", "", "Project name for the Weft job")
	submitCmd.Flags().StringVar(&skySubmitCWD, "cwd", "", "Working directory/source mount for the SkyPilot task")
	submitCmd.Flags().StringVarP(&skySubmitDescription, "message", "m", "", "Description for the Weft job")
	submitCmd.Flags().StringVar(&skySubmitDescription, "description", "", "Description for the Weft job")
	submitCmd.Flags().StringVar(&skySubmitGPU, "gpu", "", "GPU class shortcut, e.g. a100")
	submitCmd.Flags().StringVar(&skySubmitGPUClass, "gpu-class", "", "GPU class for SkyPilot accelerators")
	submitCmd.Flags().IntVar(&skySubmitGPUCount, "gpu-count", 1, "Number of GPUs")
	submitCmd.Flags().IntVar(&skySubmitGPUMem, "gpu-mem", 0, "GPU memory in GB")
	submitCmd.Flags().StringArrayVarP(&skySubmitEnv, "env", "e", nil, "Environment variable KEY=VALUE")
	submitCmd.Flags().StringArrayVarP(&skySubmitTags, "tag", "t", nil, "Tag to attach to the Weft job")
	submitCmd.Flags().StringVar(&skySubmitDryRun, "dry-run-task", "", "Write generated SkyPilot task YAML and do not submit")
	skyCmd.AddCommand(submitCmd)
}

func runSkyImport(cmd *cobra.Command, args []string) error {
	externalID := strings.TrimSpace(args[0])
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	jobs, err := skyClient.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("query SkyPilot jobs: %w", err)
	}
	job := skypilot.FindJob(jobs, externalID)
	if job == nil {
		return fmt.Errorf("SkyPilot job %q not found in `sky jobs queue --output json`", externalID)
	}
	obs := skypilot.ObservationFromJob(*job, skyImportProject, defaultSkyWorkingDir(skyImportCWD))
	binding, created, err := upsertSkyObservation(cmd, obs)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(cmd.OutOrStdout(), "Imported SkyPilot job %s as %s\n", obs.ExternalJobID, ids.FormatJobID(binding.JobID))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Updated SkyPilot job %s as %s\n", obs.ExternalJobID, ids.FormatJobID(binding.JobID))
	}
	return nil
}

func runSkySync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	updated, missing, err := syncSkyBindings(ctx, database, skySyncProject)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Synced %d SkyPilot job(s)", updated)
	if missing > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "; %d mirrored job(s) were not returned by SkyPilot", missing)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func syncSkyBindings(ctx context.Context, database *sql.DB, project string) (updated int, missing int, err error) {
	bindings, err := db.ListExternalJobBindings(database, db.ExternalExecutorSkyPilot, project)
	if err != nil {
		return 0, 0, fmt.Errorf("list external bindings: %w", err)
	}
	if len(bindings) == 0 {
		return 0, 0, nil
	}
	jobs, err := skyClient.ListJobs(ctx)
	if err != nil {
		for _, binding := range bindings {
			if markErr := db.MarkExternalSyncWarning(database, binding.JobID, "SkyPilot sync failed: "+err.Error()); markErr != nil {
				return 0, 0, fmt.Errorf("record stale SkyPilot observation for %s: %w", ids.FormatJobID(binding.JobID), markErr)
			}
		}
		return 0, 0, fmt.Errorf("query SkyPilot jobs: %w", err)
	}
	byKey := make(map[string]skypilot.Job, len(jobs)*3)
	for _, job := range jobs {
		for _, key := range []string{job.ID, job.TaskID, job.Name} {
			key = strings.TrimSpace(key)
			if key != "" {
				byKey[key] = job
			}
		}
	}
	for _, binding := range bindings {
		job, ok := byKey[binding.ExternalJobID]
		if !ok && binding.ExternalTaskID != "" {
			job, ok = byKey[binding.ExternalTaskID]
		}
		if !ok {
			if err := db.MarkExternalSyncWarning(database, binding.JobID, "SkyPilot job was not returned by sky jobs queue"); err != nil {
				return 0, 0, fmt.Errorf("record stale SkyPilot observation for %s: %w", ids.FormatJobID(binding.JobID), err)
			}
			missing++
			continue
		}
		obs := skypilot.ObservationFromJob(job, binding.SubmittedFromProject, binding.SubmittedFromWorkingDir)
		if err := db.UpdateExternalJobObservation(database, binding.JobID, obs); err != nil {
			return 0, 0, fmt.Errorf("update %s: %w", ids.FormatJobID(binding.JobID), err)
		}
		updated++
	}
	return updated, missing, nil
}

func runSkySubmit(cmd *cobra.Command, args []string) error {
	command := args[0]
	gpuClass := firstNonEmptySkyFlag(skySubmitGPUClass, skySubmitGPU)
	var gpuMem *int
	if skySubmitGPUMem > 0 {
		gpuMem = &skySubmitGPUMem
	}
	workingDir := defaultSkyWorkingDir(skySubmitCWD)
	obs := db.ExternalJobObservation{
		Executor:                db.ExternalExecutorSkyPilot,
		RawStatus:               "submitted",
		NormalizedStatus:        db.StatusQueued,
		SubmittedFromWorkingDir: workingDir,
		SubmittedFromProject:    skySubmitProject,
		Command:                 command,
		Description:             skySubmitDescription,
		GPUClass:                gpuClass,
		GPUMemGB:                gpuMem,
		EnvVars:                 skySubmitEnv,
		Tags:                    skySubmitTags,
	}
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobID, attemptID, err := db.CreateExternalPendingJob(database, obs)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("weft-%s", ids.FormatJobID(jobID))
	if skySubmitDryRun != "" {
		_, _, err := skyClient.Submit(cmd.Context(), skypilot.SubmitOptions{
			Name:       name,
			Command:    command,
			WorkDir:    workingDir,
			GPUClass:   gpuClass,
			GPUCount:   skySubmitGPUCount,
			GPUMemGB:   gpuMem,
			EnvVars:    skySubmitEnv,
			DryRunPath: skySubmitDryRun,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s and wrote SkyPilot task to %s\n", ids.FormatJobID(jobID), skySubmitDryRun)
		return nil
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	job, _, err := skyClient.Submit(ctx, skypilot.SubmitOptions{
		Name:     name,
		Command:  command,
		WorkDir:  workingDir,
		GPUClass: gpuClass,
		GPUCount: skySubmitGPUCount,
		GPUMemGB: gpuMem,
		EnvVars:  skySubmitEnv,
	})
	if err != nil {
		_ = db.MarkExternalSubmissionFailed(database, jobID, err.Error())
		return fmt.Errorf("submit SkyPilot job for %s: %w", ids.FormatJobID(jobID), err)
	}
	attachObs := skypilot.ObservationFromJob(*job, skySubmitProject, workingDir)
	attachObs.Command = command
	attachObs.Description = skySubmitDescription
	attachObs.GPUClass = gpuClass
	attachObs.GPUMemGB = gpuMem
	attachObs.EnvVars = skySubmitEnv
	attachObs.Tags = skySubmitTags
	if err := db.AttachExternalBinding(database, jobID, attemptID, attachObs); err != nil {
		return fmt.Errorf("record SkyPilot binding: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Job %s submitted to SkyPilot as %s\n", ids.FormatJobID(jobID), attachObs.ExternalJobID)
	return nil
}

func upsertSkyObservation(cmd *cobra.Command, obs db.ExternalJobObservation) (*db.ExternalJobBinding, bool, error) {
	database, err := db.Open()
	if err != nil {
		return nil, false, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	binding, created, err := db.UpsertExternalJobFromObservation(database, obs)
	if err != nil {
		return nil, false, err
	}
	return binding, created, nil
}

func defaultSkyWorkingDir(value string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return skypilot.DefaultWorkDir()
}

func firstNonEmptySkyFlag(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func runSkyLogForJob(cmd *cobra.Command, database *sql.DB, job *db.Job) error {
	binding, err := db.GetExternalJobBindingByJobID(database, job.ID)
	if err != nil {
		return fmt.Errorf("get SkyPilot binding: %w", err)
	}
	if binding == nil {
		return fmt.Errorf("job %s has no SkyPilot binding", ids.FormatJobID(job.ID))
	}
	if logFollow && (logGrep != "" || logFrom > 0 || logTo > 0 || logFull || cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail")) {
		return fmt.Errorf("SkyPilot follow mode cannot be combined with local log filtering flags")
	}
	ctx := cmd.Context()
	if !logFollow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	out, err := skyClient.Logs(ctx, binding.ExternalJobID, logFollow)
	if err != nil {
		return err
	}
	if logFollow {
		fmt.Print(string(out))
		return nil
	}
	output := filterLogContent(string(out), logFrom, logTo, logLines, logGrep)
	fmt.Print(processCarriageReturns(output))
	return nil
}

func cancelSkyJob(cmd *cobra.Command, database *sql.DB, job *db.Job) (bool, error) {
	if job == nil || job.Backend != db.BackendSkyPilot {
		return false, nil
	}
	binding, err := db.GetExternalJobBindingByJobID(database, job.ID)
	if err != nil {
		return true, err
	}
	if binding == nil {
		return true, fmt.Errorf("job %s has no SkyPilot binding", ids.FormatJobID(job.ID))
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := skyClient.Cancel(ctx, binding.ExternalJobID); err != nil {
		return true, err
	}
	if err := db.SetRequestedStatus(database, job.ID, db.StatusCanceled); err != nil {
		return true, err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cancel requested for SkyPilot job %s (%s); run `weft sky sync` to confirm terminal state\n",
		ids.FormatJobID(job.ID), binding.ExternalJobID)
	return true, nil
}
