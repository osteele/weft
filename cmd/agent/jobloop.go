package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// jobSequenceConfig holds parameters for runJobSequence.
type jobSequenceConfig struct {
	R2Bucket            string
	InstanceID          int64
	PhaseKey            string
	LogDir              string
	DiskPath            string
	MaxTime             time.Duration // 0 = no limit
	StartTime           time.Time     // for time budget accounting
	OnPhase             func(string)  // update current phase string (for heartbeat)
	SkipWorkdirDeletion bool          // disable background workdir cleanup (for debugging)
	GPUWarmup           bool          // run CUDA warmup before first benchmark job
	// CostPerHourCents drives hang-watchdog tier selection; 0 means on-prem
	// or unknown, which picks conservative thresholds.
	CostPerHourCents int

	// Provider and InstanceType are exposed to job processes via
	// WEFT_PROVIDER / WEFT_INSTANCE_TYPE env vars. Resumed indicates this
	// container has restarted after a docker stop on the same disk (typical
	// of a pause/resume on interruptible Vast.ai instances).
	Provider     string
	InstanceType string
	Resumed      bool
}

// agentRentalEnv builds the rental-context env vars set by the agent for
// every job it runs. These let job processes detect that they're on a weft
// rental, identify the provider and rental type, and notice that they've
// been restarted after a container pause (the typical preemption symptom on
// Vast.ai interruptible instances).
func agentRentalEnv(cfg jobSequenceConfig) []string {
	env := []string{
		"WEFT_TARGET_KIND=rental",
		fmt.Sprintf("WEFT_LAUNCH_ID=%d", cfg.InstanceID),
	}
	if cfg.Provider != "" {
		env = append(env, "WEFT_PROVIDER="+cfg.Provider)
	}
	if cfg.InstanceType != "" {
		env = append(env, "WEFT_INSTANCE_TYPE="+cfg.InstanceType)
	}
	if cfg.Resumed {
		env = append(env, "WEFT_RESUMED=1")
	}
	return env
}

func hasDeclaredHFInput(inputs []string) bool {
	for _, input := range inputs {
		if strings.HasPrefix(input, "hf:") || strings.HasPrefix(input, "hf-dataset:") {
			return true
		}
	}
	return false
}

func hfOfflineEnv(inputs []string) []string {
	if !hasDeclaredHFInput(inputs) {
		return nil
	}
	return nil
}

// pickWatchdogTimeouts selects GPU-idle and stdout-silence timeouts based on
// the cost-per-hour of the current host. Expensive cloud hosts (>= $2/hr) get
// aggressive thresholds so hangs are caught before they burn significant money.
// See specs/job-lifecycle.allium rules GPUIdleKillsJob and StdoutSilenceKillsJob.
func pickWatchdogTimeouts(costPerHourCents int) (gpuIdle, stdoutSilence time.Duration) {
	const expensiveCents = 200 // $2.00/hr
	if costPerHourCents >= expensiveCents {
		return 8 * time.Minute, 12 * time.Minute
	}
	return 20 * time.Minute, 30 * time.Minute
}

// jobSequenceResult holds the outcome of running a sequence of jobs.
type jobSequenceResult struct {
	FailedJobs []int64
	AnyFailed  bool
}

// runJobSequence runs a slice of agent jobs sequentially, overlapping post-job
// uploads with the next job's execution. Benchmark jobs act as a barrier —
// all background work must finish before a benchmark job starts.
func runJobSequence(jobs []cloud.AgentJob, cfg jobSequenceConfig) jobSequenceResult {
	var result jobSequenceResult
	bgm := newBGWorkManager(jobs, cfg.SkipWorkdirDeletion)
	gpuWarmedUp := false
	canceledAttempts := map[int64]struct{}{}
	var lastPostJobID int64

	drainCancels := func() {
		canceled, err := drainGraceCancelAttemptRequests(cfg.R2Bucket, cfg.InstanceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drain cancel-attempts: %v\n", err)
			return
		}
		for id := range canceled {
			canceledAttempts[id] = struct{}{}
		}
	}
	drainCancels()

	for i := 0; i < len(jobs); i++ {
		job := jobs[i]
		drainCancels()
		if _, canceled := canceledAttempts[job.RunID]; canceled {
			fmt.Printf("--- Job %d (run %d) canceled by orchestrator; skipping ---\n", job.ID, job.RunID)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetailf("attempt %d canceled by orchestrator", job.RunID))
			continue
		}

		// Benchmark barrier: wait for all background uploads/deletions
		if slices.Contains(job.Tags, "benchmark") {
			bgm.Barrier()
			if lastPostJobID > 0 {
				setSequencePhase(cfg, fmt.Sprintf("post_job_uploads_drained:%d", lastPostJobID), lastPostJobID)
			}
		}

		// GPU warmup (opt-in via config): prime system-level CUDA caches
		// before the first benchmark job to avoid cold-start bias.
		if cfg.GPUWarmup && job.UsesGPU && slices.Contains(job.Tags, "benchmark") && !gpuWarmedUp {
			runGPUWarmup(cfg.R2Bucket, cfg.PhaseKey, job.ID, cfg.OnPhase)
			gpuWarmedUp = true
		}

		// Check time budget
		if cfg.MaxTime > 0 {
			remaining := cfg.MaxTime - time.Since(cfg.StartTime)
			if remaining <= 0 {
				fmt.Println("Instance time budget exhausted, skipping remaining jobs")
				break
			}
		}

		fmt.Printf("--- Job %d ---\n", job.ID)
		oplog.LogJob(oplog.OpJobStart, job.ID, "", oplog.WithDetailf("cmd=%s", job.Command))

		// Cloud-after gate: if a same-instance producer this job depends on
		// has failed, skip the consumer with a clear reason.
		if skip, reason := checkCloudAfter(job, result.FailedJobs); skip {
			fmt.Fprintf(os.Stderr, "job %d skipped: %s\n", job.ID, reason)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetail(reason))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)

			ei := runner.ExitInfo{ExitCode: 1}
			paths := runner.NewJobPaths(cfg.LogDir, job.ID)
			_ = runner.WriteStatusFile(paths, ei)
			now := time.Now().Unix()
			_ = runner.WriteCompletionRecord(paths, ei, runner.RunningJobState{}, "", reason, now, now, nil)
			r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", ei.ExitCode))
			continue
		}

		workDir := job.Dir
		// Recover from a missing/empty workdir (e.g. previous campaign-mid
		// cleanup race, manual rm, or aborted source extract) by re-staging
		// from the locally cached source tarball before stageCloudNeeds runs.
		ensureSourceFresh(cfg.R2Bucket, runner.ExpandTilde(workDir))
		if err := stageCloudNeeds(cfg.R2Bucket, job.ID, workDir, job.CloudNeeds); err != nil {
			fmt.Fprintf(os.Stderr, "cloud artifact staging failed for job %d: %v\n", job.ID, err)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithError(err))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)

			// Mark job complete with failure even when command did not start.
			ei := runner.ExitInfo{ExitCode: 1}
			paths := runner.NewJobPaths(cfg.LogDir, job.ID)
			_ = runner.WriteStatusFile(paths, ei)
			now := time.Now().Unix()
			_ = runner.WriteCompletionRecord(paths, ei, runner.RunningJobState{}, "", "artifact_stage_failed", now, now, nil)
			r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", ei.ExitCode))
			continue
		}

		// Write .started marker to R2
		r2Put(cfg.R2Bucket, r2keys.JobAttemptStarted(job.ID, job.RunID), fmt.Sprintf("%d", time.Now().Unix()))
		paths := runner.NewJobPaths(cfg.LogDir, job.ID)
		stopTimeseriesUploader := startTimeseriesUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Timeseries)
		stopTelemetryUploader := startTelemetryUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Telemetry)

		// Compute per-job max time from remaining budget
		var jobMaxTime time.Duration
		if cfg.MaxTime > 0 {
			jobMaxTime = cfg.MaxTime - time.Since(cfg.StartTime)
		}

		jobCfg := singleJobConfigForAgentJob(job, cfg, workDir, jobMaxTime)

		ei, err := runJobWithProgress(cfg.R2Bucket, job.ID, job.RunID, cfg.InstanceID, cfg.LogDir, jobCfg)
		if job.UsesGPU {
			gpuWarmedUp = true
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "run-job %d failed: %v\n", job.ID, err)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithError(err))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else if ei.ExitCode != 0 {
			fmt.Printf("Job %d failed (exit %d)\n", job.ID, ei.ExitCode)
			oplog.LogJob(oplog.OpJobFail, job.ID, "", oplog.WithDetailf("exit=%d", ei.ExitCode))
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else {
			fmt.Printf("Job %d completed successfully\n", job.ID)
			oplog.LogJob(oplog.OpJobComplete, job.ID, "", oplog.WithDetail("exit=0"))
		}

		// Compute the exit code that will be written to .complete and used in
		// any fallback completion record. err != nil from runJobWithProgress
		// means the runner returned early without finishing normally — treat
		// as exit=1 if it didn't already give us a non-zero code.
		exitCode := ei.ExitCode
		if err != nil && exitCode == 0 {
			exitCode = 1
		}

		// Defense in depth: if the runner returned an error and didn't leave a
		// failure_reason / completion.json behind, synthesize stubs so the
		// "why" reaches the coordinator (and the DB) instead of being silently
		// dropped. Without this, a runner-level start failure produces an
		// instance whose status reads "exit 1" with nothing else.
		ensureFailureArtifacts(cfg.LogDir, job.ID, ei, err)

		// Upload the opslog before writing .complete so a self-destruct that
		// races the post-job work still leaves the agent's diagnostic trail
		// (which captures the stderr message from runJobWithProgress) in R2.
		uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)

		// Write .complete marker synchronously so the coordinator sees
		// this job as finished before the next job's .started marker.
		r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", exitCode))

		// === Synchronous post-job work ===
		cleanupPhase := fmt.Sprintf("post_job_cleanup:%d", job.ID)
		setSequencePhase(cfg, cleanupPhase, job.ID)

		uploadStartedUnix := time.Now().Unix()
		if err := patchPhaseUploadWindow(cfg.LogDir, job.ID, uploadStartedUnix, 0); err != nil {
			fmt.Fprintf(os.Stderr, "patch phase timing for job %d: upload start: %v\n", job.ID, err)
		}

		// Stop per-job live uploaders before next job starts its own
		stopTimeseriesUploader()
		stopTelemetryUploader()
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTimeseries(job.ID, job.RunID))
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTelemetry(job.ID, job.RunID))

		promoteUVManifest(cfg.R2Bucket, cfg.LogDir)
		uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)

		// Snapshot log dir so background uploads can read from it
		// while the live log dir is cleaned for the next job
		logSnapshot, snapErr := snapshotLogDir(cfg.LogDir, job.ID)
		if snapErr != nil {
			fmt.Fprintf(os.Stderr, "snapshot log dir for job %d: %v\n", job.ID, snapErr)
		}

		if i < len(jobs)-1 {
			cleanLogDir(cfg.LogDir)
			oplog.Init(filepath.Join(cfg.LogDir, agentOpslogFile), 0)
		}

		// Update phase so TUI shows "uploading" during background uploads
		uploadingPhase := fmt.Sprintf("uploading:%d", job.ID)

		// === Background post-job work (uploads + workdir cleanup) ===
		bgm.StartPostJobWork(postJobWork{
			r2Bucket:          cfg.R2Bucket,
			instanceID:        cfg.InstanceID,
			jobID:             job.ID,
			runID:             job.RunID,
			exitCode:          exitCode,
			workDir:           runner.ExpandTilde(workDir),
			logSnapshot:       logSnapshot,
			diskPath:          cfg.DiskPath,
			phase:             uploadingPhase,
			uploadStartedUnix: uploadStartedUnix,
		})

		if cfg.OnPhase != nil {
			cfg.OnPhase(uploadingPhase)
		}
		writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadingPhase)
		lastPostJobID = job.ID

		// Check for newly submitted jobs via R2 (between-job reuse)
		setSequencePhase(cfg, "ready_for_next_job", job.ID)
		if newJobs := checkForNewJobs(cfg.R2Bucket, cfg.InstanceID, func(phase string) {
			if cfg.OnPhase != nil {
				cfg.OnPhase(phase)
			}
			writePhase(cfg.R2Bucket, cfg.PhaseKey, phase)
		}); len(newJobs) > 0 {
			fmt.Printf("Picked up %d new job(s) from R2\n", len(newJobs))
			bgm.RegisterNewJobs(newJobs)
			jobs = append(jobs, newJobs...)
		}
	}

	// Wait for remaining background work, then clean up workdirs.
	// Cleanup is intentionally deferred to here (after the new-job pickup
	// loop has exited) to avoid racing with checkForNewJobs.
	bgm.Barrier()
	if lastPostJobID > 0 {
		setSequencePhase(cfg, fmt.Sprintf("post_job_uploads_drained:%d", lastPostJobID), lastPostJobID)
	}
	bgm.CleanupWorkdirs()
	return result
}

func setSequencePhase(cfg jobSequenceConfig, phase string, jobID int64) {
	if cfg.OnPhase != nil {
		cfg.OnPhase(phase)
	}
	oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(jobID), oplog.WithDetail(phase))
	writePhase(cfg.R2Bucket, cfg.PhaseKey, phase)
}

func singleJobConfigForAgentJob(job cloud.AgentJob, cfg jobSequenceConfig, workDir string, jobMaxTime time.Duration) runner.SingleJobConfig {
	gpuIdle, stdoutSilence := pickWatchdogTimeouts(cfg.CostPerHourCents)
	env := agentRentalEnv(cfg)
	env = append(env, job.Env...)
	env = append(env, hfOfflineEnv(job.Inputs)...)
	return runner.SingleJobConfig{
		JobID: job.ID,
		Job: ops.CommandJob{
			Cmd:        job.Command,
			Tags:       append([]string(nil), job.Tags...),
			OutputDirs: append([]string(nil), job.OutputDirs...),
			Produces:   append([]string(nil), job.Produces...),
			Needs:      append([]string(nil), job.Needs...),
			Env:        env,
		},
		LogDir:               cfg.LogDir,
		WorkingDir:           workDir,
		MaxTime:              jobMaxTime,
		SetupTimeout:         inventory.DefaultSetupTimeout,
		GPUIdleTimeout:       gpuIdle,
		StdoutSilenceTimeout: stdoutSilence,
		OnPhase:              phaseCallback(cfg.R2Bucket, cfg.PhaseKey, job.ID, cfg.OnPhase),
	}
}

func patchCompletionUpload(logDir string, jobID int64, upload *runner.OutputUploadResult) {
	patchCompletionRecord(logDir, jobID, func(rec *runner.CompletionRecord) {
		rec.OutputUpload = upload
	})
}

func patchCompletionResultsUpload(logDir string, jobID int64, upload *runner.UploadSummary) {
	patchCompletionRecord(logDir, jobID, func(rec *runner.CompletionRecord) {
		rec.ResultsUpload = upload
	})
}

// collectCompletionManifest reads per-job completion records from the log
// directory and assembles an InstanceCompletionManifest suitable for writing
// to the R2 completion marker.
func collectCompletionManifest(logDir string, jobs []cloud.AgentJob) *runner.InstanceCompletionManifest {
	manifest := &runner.InstanceCompletionManifest{
		CompletedAtUnix: time.Now().Unix(),
	}
	allOK := true
	for _, job := range jobs {
		summary, ok := summarizeJobCompletion(logDir, job.ID)
		if !ok {
			allOK = false
		}
		manifest.Jobs = append(manifest.Jobs, summary)
	}
	if !allOK {
		manifest.ExitCode = 1
	}
	return manifest
}

// summarizeJobCompletion reads a job's completion record and returns a summary.
// Returns (summary, ok) where ok is false if the job failed or the record is unreadable.
func summarizeJobCompletion(logDir string, jobID int64) (runner.JobCompletionSummary, bool) {
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		return runner.JobCompletionSummary{JobID: jobID, ExitCode: -1, UploadStatus: "unknown"}, false
	}
	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return runner.JobCompletionSummary{JobID: jobID, ExitCode: -1, UploadStatus: "unknown"}, false
	}
	uploadStatus := "ok"
	if rec.OutputUpload != nil && rec.OutputUpload.Status != "ok" {
		uploadStatus = rec.OutputUpload.Status
	}
	if rec.ResultsUpload != nil && rec.ResultsUpload.Status != "ok" {
		if uploadStatus == "ok" {
			uploadStatus = rec.ResultsUpload.Status
		} else {
			uploadStatus = "partial"
		}
	}
	var outputBytes int64
	if rec.OutputUpload != nil {
		outputBytes = rec.OutputUpload.Bytes
	}
	return runner.JobCompletionSummary{
		JobID:        jobID,
		ExitCode:     rec.ExitCode,
		UploadStatus: uploadStatus,
		OutputBytes:  outputBytes,
	}, rec.ExitCode == 0
}

func patchCompletionRecord(logDir string, jobID int64, mutate func(*runner.CompletionRecord)) {
	if mutate == nil {
		return
	}
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: read: %v\n", jobID, err)
		return
	}

	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: parse: %v\n", jobID, err)
		return
	}

	mutate(&rec)
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: encode: %v\n", jobID, err)
		return
	}
	out = append(out, '\n')
	if err := os.WriteFile(paths.Completion, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "patch completion record for job %d: write: %v\n", jobID, err)
	}
}

func patchPhaseUploadWindow(logDir string, jobID, uploadStart, uploadEnd int64) error {
	paths := runner.NewJobPaths(logDir, jobID)
	data, err := os.ReadFile(paths.Phases)
	if err != nil {
		return err
	}

	var phases runner.PhaseTiming
	if err := json.Unmarshal(data, &phases); err != nil {
		return err
	}
	if uploadStart > 0 {
		phases.UploadStart = uploadStart
	}
	if uploadEnd > 0 {
		phases.UploadEnd = uploadEnd
	}

	out, err := json.MarshalIndent(phases, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(paths.Phases, out, 0o644)
}

// snapshotLogDir copies the log directory contents to a temp dir so that
// the main log dir can be cleaned for the next job while background uploads
// read from the snapshot. The caller must os.RemoveAll the returned path.
func snapshotLogDir(logDir string, jobID int64) (string, error) {
	snapshot := filepath.Join(os.TempDir(), fmt.Sprintf("weft-logs-job-%d", jobID))
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return snapshot, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue // log dir is flat
		}
		name := entry.Name()
		src := filepath.Join(logDir, name)
		dst := filepath.Join(snapshot, name)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		os.WriteFile(dst, data, 0o644)
	}
	return snapshot, nil
}

// ensureFailureArtifacts writes a stub failure_reason and completion.json when
// the runner returned an error or non-zero exit code without leaving them
// behind itself. This catches early returns from RunSingleJob (e.g. setup
// failures that didn't reach the normal completion path) so the coordinator
// has something to ingest into job_attempts.failure_reason instead of
// surfacing only "exit 1" with no diagnosis.
func ensureFailureArtifacts(logDir string, jobID int64, ei runner.ExitInfo, runErr error) {
	if runErr == nil && ei.ExitCode == 0 {
		return
	}
	paths := runner.NewJobPaths(logDir, jobID)

	reason := ""
	if runErr != nil {
		reason = runErr.Error()
	}
	if reason == "" {
		reason = runner.DetectFailureReasonFromExitInfo(ei)
	}

	if existing := runner.ReadFailureReasonFile(paths.FailureReason); existing == "" {
		_ = runner.WriteFailureReasonFile(paths, reason)
	}

	if _, err := os.Stat(paths.Completion); err != nil {
		exitCode := ei.ExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		now := time.Now().Unix()
		_ = runner.WriteCompletionRecord(paths, runner.ExitInfo{ExitCode: exitCode}, runner.RunningJobState{}, "", reason, now, now, nil)
	}
}

// runGPUWarmup runs a lightweight Python command to prime the CUDA context
// (context init, cuBLAS handle, memory allocator) so that benchmark jobs
// don't pay cold-start overhead in their first measured config.
func runGPUWarmup(r2Bucket, phaseKey string, nextJobID int64, onPhase func(string)) {
	phase := fmt.Sprintf("gpu_warmup:%d", nextJobID)
	if onPhase != nil {
		onPhase(phase)
	}
	writePhase(r2Bucket, phaseKey, phase)
	fmt.Println("Running GPU warmup (CUDA context + cuBLAS init)...")

	start := time.Now()
	cmd := exec.Command("python3", "-c",
		"import torch; torch.zeros(1, device='cuda'); torch.mm(torch.randn(2,2, device='cuda'), torch.randn(2,2, device='cuda'))")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "GPU warmup failed: %v (benchmark measurements may include cold-start overhead)\n", err)
	} else {
		fmt.Printf("GPU warmup completed in %s\n", time.Since(start).Round(time.Millisecond))
	}
}

// checkCloudAfter returns (skip=true, reason) when any CloudAfter ref points
// at a producer job that failed earlier in this agent session. Producers that
// are not in failedJobs (i.e. they succeeded, or were never run by this
// agent — e.g. completed in a prior session before a grace-wake) are treated
// as satisfied: we only skip on observed failure, not on "not observed".
//
// AllowFailure refs are not skip-triggers.
func checkCloudAfter(job cloud.AgentJob, failedJobs []int64) (bool, string) {
	if len(job.CloudAfter) == 0 || len(failedJobs) == 0 {
		return false, ""
	}
	failed := make(map[int64]bool, len(failedJobs))
	for _, id := range failedJobs {
		failed[id] = true
	}
	for _, ref := range job.CloudAfter {
		if ref.AllowFailure {
			continue
		}
		if failed[ref.JobID] {
			return true, fmt.Sprintf("cloud_after_failed: producer job %d failed on this instance", ref.JobID)
		}
	}
	return false, ""
}

func hasOutputDirs(workDir string) bool {
	workDir = runner.ExpandTilde(workDir)
	for _, dir := range config.DefaultOutputDirs {
		dir = filepath.Clean(dir)
		info, err := os.Stat(filepath.Join(workDir, dir))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
