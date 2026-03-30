package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
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
	MaxTime             time.Duration // 0 = no limit
	StartTime           time.Time     // for time budget accounting
	OnPhase             func(string)  // update current phase string (for heartbeat)
	SkipWorkdirDeletion bool          // disable background workdir cleanup (for debugging)
	GPUWarmup           bool          // run CUDA warmup before first benchmark job
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

	for i := 0; i < len(jobs); i++ {
		job := jobs[i]

		// Benchmark barrier: wait for all background uploads/deletions
		if slices.Contains(job.Tags, "benchmark") {
			bgm.Barrier()
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

		// Write .started marker to R2
		r2Put(cfg.R2Bucket, r2keys.JobAttemptStarted(job.ID, job.RunID), fmt.Sprintf("%d", time.Now().Unix()))
		paths := runner.NewJobPaths(cfg.LogDir, job.ID)
		stopTimeseriesUploader := startTimeseriesUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Timeseries)
		stopTelemetryUploader := startTelemetryUploader(cfg.R2Bucket, job.ID, job.RunID, paths.Telemetry)

		workDir := job.Dir

		// Compute per-job max time from remaining budget
		var jobMaxTime time.Duration
		if cfg.MaxTime > 0 {
			jobMaxTime = cfg.MaxTime - time.Since(cfg.StartTime)
		}

		jobCfg := runner.SingleJobConfig{
			JobID: job.ID,
			Job: ops.CommandJob{
				Cmd:  job.Command,
				Tags: append([]string(nil), job.Tags...),
			},
			LogDir:     cfg.LogDir,
			WorkingDir: workDir,
			MaxTime:    jobMaxTime,
			OnPhase:    phaseCallback(cfg.R2Bucket, cfg.PhaseKey, job.ID, cfg.OnPhase),
		}

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

		// Write .complete marker synchronously so the coordinator sees
		// this job as finished before the next job's .started marker.
		exitCode := 1
		if err == nil {
			exitCode = ei.ExitCode
		}
		r2Put(cfg.R2Bucket, r2keys.JobAttemptComplete(job.ID, job.RunID), fmt.Sprintf("%d", exitCode))

		// === Synchronous post-job work ===
		finalizePhase := fmt.Sprintf("finalizing:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(finalizePhase)
		}
		oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(finalizePhase))
		writePhase(cfg.R2Bucket, cfg.PhaseKey, finalizePhase)

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

		// === Background post-job work (uploads + workdir cleanup) ===
		bgm.StartPostJobWork(postJobWork{
			r2Bucket:          cfg.R2Bucket,
			jobID:             job.ID,
			runID:             job.RunID,
			workDir:           runner.ExpandTilde(workDir),
			logSnapshot:       logSnapshot,
			uploadStartedUnix: uploadStartedUnix,
		})

		// Update phase so TUI shows "uploading" during background uploads
		uploadingPhase := fmt.Sprintf("uploading:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(uploadingPhase)
		}
		writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadingPhase)

		// Check for newly submitted jobs via R2 (between-job reuse)
		if newJobs := checkForNewJobs(cfg.R2Bucket, cfg.InstanceID); len(newJobs) > 0 {
			fmt.Printf("Picked up %d new job(s) from R2\n", len(newJobs))
			bgm.RegisterNewJobs(newJobs)
			jobs = append(jobs, newJobs...)
		}
	}

	// Wait for remaining background work
	bgm.Barrier()
	return result
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
		if filepath.Ext(name) == ".log" {
			continue // log content lives in R2 live-log chunks
		}
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
