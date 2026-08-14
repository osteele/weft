package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
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
var skyImportJob string
var skyImportTask string
var skyImportCommand string
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
	importCmd.Flags().StringVar(&skyImportJob, "job", "", "Rebind an unconfirmed Weft job (for example wj123) instead of creating one")
	importCmd.Flags().StringVar(&skyImportTask, "task", "", "Select a task ID or name within a multi-task managed job")
	importCmd.Flags().StringVar(&skyImportCommand, "command", "", "Task command when the SkyPilot queue does not expose it")
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
	job, err := skypilot.ResolveJobWithTask(jobs, externalID, skyImportTask)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("SkyPilot job %q not found in `sky jobs queue --all --output json`", externalID)
	}
	obs := skypilot.ObservationFromJob(*job, skyImportProject, defaultSkyWorkingDir(skyImportCWD))
	if strings.TrimSpace(skyImportCommand) != "" {
		obs.Command = strings.TrimSpace(skyImportCommand)
	}
	if strings.TrimSpace(skyImportJob) != "" {
		jobID, err := ids.ParseJobID(skyImportJob)
		if err != nil {
			return fmt.Errorf("parse --job: %w", err)
		}
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer database.Close()
		localJob, err := db.GetJobByID(database, jobID)
		if err != nil {
			return err
		}
		if localJob == nil {
			return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
		}
		obs.SubmittedFromWorkingDir = localJob.WorkingDir
		obs.SubmittedFromProject = localJob.Project
		binding, err := db.RebindUnconfirmedExternalJob(database, jobID, obs)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Rebound SkyPilot job %s to %s\n", obs.ExternalJobID, ids.FormatJobID(binding.JobID))
		return nil
	}
	if strings.TrimSpace(obs.Command) == "" {
		return fmt.Errorf("SkyPilot queue output does not expose the task command; pass --command with the command to record for the imported job")
	}
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
	return skyClient.SyncBindings(ctx, database, project)
}

func syncSkyBindingsAmbient(ctx context.Context, database *sql.DB, project string) (updated int, missing int, err error) {
	return skyClient.SyncBindingsAmbient(ctx, database, project)
}

// skyDryRunTaskName stands in for the weft-wj<id> name a real submit assigns
// after recording the job. A dry run has no job id because it deliberately
// creates no ledger row.
const skyDryRunTaskName = "weft-dry-run"

func runSkySubmit(cmd *cobra.Command, args []string) error {
	command := args[0]
	gpuClass := firstNonEmptySkyFlag(skySubmitGPUClass, skySubmitGPU)
	var gpuMem *int
	if skySubmitGPUMem > 0 {
		gpuMem = &skySubmitGPUMem
	}
	if gpuMem != nil {
		return fmt.Errorf("--gpu-mem is not supported for SkyPilot submissions; select a GPU class with --gpu instead")
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
	// A dry run renders the task and submits nothing, so it must not touch the
	// ledger. The ledger-before-external ordering exists so a crash mid-submit
	// leaves a recoverable record; there is nothing to recover from a render.
	// Creating a row here left a durable backend=skypilot job with no binding,
	// and SyncExternalExecutorJob is guarded on the binding existing, so it
	// could never advance and sat queued forever.
	if skySubmitDryRun != "" {
		if _, _, err := skyClient.Submit(cmd.Context(), skypilot.SubmitOptions{
			Name:       skyDryRunTaskName,
			Command:    command,
			WorkDir:    workingDir,
			GPUClass:   gpuClass,
			GPUCount:   skySubmitGPUCount,
			GPUMemGB:   gpuMem,
			EnvVars:    skySubmitEnv,
			DryRunPath: skySubmitDryRun,
		}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"Wrote SkyPilot task to %s (dry run: nothing submitted, no job recorded).\n"+
				"The task name is a placeholder; a real submit names it weft-wj<id> after recording the job.\n",
			skySubmitDryRun)
		return nil
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
		var unconfirmed *skypilot.SubmissionUnconfirmedError
		if !errors.As(err, &unconfirmed) {
			if markErr := db.MarkExternalSubmissionFailed(database, jobID, err.Error()); markErr != nil {
				return fmt.Errorf("submit SkyPilot job for %s: %v; record definitive submission failure: %w", ids.FormatJobID(jobID), err, markErr)
			}
		}
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
	if logLines < 0 {
		return fmt.Errorf("--lines/--tail must be nonnegative")
	}
	if logFollow && logLines == 0 {
		return fmt.Errorf("SkyPilot follow requires a positive --lines/--tail value; zero means the entire retained log to SkyPilot")
	}
	ctx := cmd.Context()
	if !logFollow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	remoteTail := logLines
	if logFull || logFrom > 0 || logTo > 0 {
		remoteTail = 0
	}
	if logFollow {
		stdout := io.Writer(cmd.OutOrStdout())
		var filter *streamingLogFilter
		if logFrom > 1 || logGrep != "" {
			filter = newStreamingLogFilterWithRange(stdout, logFrom, 0, logGrep)
			stdout = filter
		}
		err := skyClient.FollowLogs(ctx, binding.ExternalJobID, binding.ExternalTaskID, remoteTail, stdout, cmd.ErrOrStderr())
		if filter != nil {
			if flushErr := filter.Flush(); err == nil {
				err = flushErr
			}
		}
		return err
	}
	if logLines == 0 && logFrom == 0 && logTo == 0 && !logFull {
		return nil
	}
	stdout := io.Writer(cmd.OutOrStdout())
	var filter *streamingLogFilter
	if logFrom > 1 || logTo > 0 || logGrep != "" {
		filter = newStreamingLogFilterWithRange(stdout, logFrom, logTo, logGrep)
		stdout = filter
	}
	err = skyClient.StreamLogs(ctx, binding.ExternalJobID, binding.ExternalTaskID, false, remoteTail, stdout, cmd.ErrOrStderr())
	if filter != nil {
		if flushErr := filter.Flush(); err == nil {
			err = flushErr
		}
	}
	return err
}

const maxStreamingLogLineBytes = 1 << 20

type streamingLogFilter struct {
	dest    io.Writer
	from    int
	to      int
	line    int
	pattern string
	re      *regexp.Regexp
	pending bytes.Buffer
	failed  bool
}

func newStreamingLogFilter(dest io.Writer, from int, pattern string) *streamingLogFilter {
	return newStreamingLogFilterWithRange(dest, from, 0, pattern)
}

func newStreamingLogFilterWithRange(dest io.Writer, from, to int, pattern string) *streamingLogFilter {
	filter := &streamingLogFilter{dest: dest, from: from, to: to, pattern: pattern}
	filter.re, _ = regexp.Compile(pattern)
	return filter
}

func (w *streamingLogFilter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		segmentLen := len(p)
		terminated := false
		if newline >= 0 {
			segmentLen = newline
			terminated = true
		}
		if w.pending.Len()+segmentLen > maxStreamingLogLineBytes {
			w.failed = true
			return written, fmt.Errorf("SkyPilot log line exceeds %d bytes", maxStreamingLogLineBytes)
		}
		_, _ = w.pending.Write(p[:segmentLen])
		written += segmentLen
		p = p[segmentLen:]
		if !terminated {
			continue
		}
		written++
		p = p[1:]
		if err := w.emit(true); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *streamingLogFilter) Flush() error {
	if w.failed || w.pending.Len() == 0 {
		return nil
	}
	return w.emit(false)
}

func (w *streamingLogFilter) emit(terminated bool) error {
	w.line++
	line := append([]byte(nil), w.pending.Bytes()...)
	w.pending.Reset()
	if w.from > 0 && w.line < w.from {
		return nil
	}
	if w.to > 0 && w.line > w.to {
		return nil
	}
	if w.pattern != "" {
		matched := w.re != nil && w.re.Match(line)
		if w.re == nil {
			matched = bytes.Contains(line, []byte(w.pattern))
		}
		if !matched {
			return nil
		}
	}
	if err := writeStreamingLogBytes(w.dest, line); err != nil {
		return err
	}
	if terminated {
		return writeStreamingLogBytes(w.dest, []byte{'\n'})
	}
	return nil
}

func writeStreamingLogBytes(dest io.Writer, data []byte) error {
	n, err := dest.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
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
	requested, err := skyClient.CancelBinding(ctx, database, binding)
	if err != nil {
		return true, fmt.Errorf("record SkyPilot cancel intent: %w", err)
	}
	if !requested {
		fmt.Fprintf(cmd.OutOrStdout(), "SkyPilot job %s (%s) is already terminal\n",
			ids.FormatJobID(job.ID), binding.ExternalJobID)
		return true, nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cancel requested for SkyPilot job %s (%s); run `weft sky sync` to confirm terminal state\n",
		ids.FormatJobID(job.ID), binding.ExternalJobID)
	return true, nil
}
