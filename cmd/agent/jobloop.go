package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	R2Bucket   string
	InstanceID int64
	PhaseKey   string
	LogDir     string
	MaxTime    time.Duration // 0 = no limit
	StartTime  time.Time     // for time budget accounting
	OnPhase    func(string)  // update current phase string (for heartbeat)
}

// jobSequenceResult holds the outcome of running a sequence of jobs.
type jobSequenceResult struct {
	FailedJobs []int64
	AnyFailed  bool
}

// runJobSequence runs a slice of agent jobs sequentially, handling time budgets,
// R2 markers, output uploads, uv manifest promotion, log cleanup, opslog re-init,
// and between-job reuse checks. Both run-campaign and grace-wait call this.
func runJobSequence(jobs []cloud.AgentJob, cfg jobSequenceConfig) jobSequenceResult {
	var result jobSequenceResult

	for i := 0; i < len(jobs); i++ {
		job := jobs[i]
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
		timeseriesPath := paths.Timeseries
		telemetryPath := paths.Telemetry
		stopTimeseriesUploader := startTimeseriesUploader(cfg.R2Bucket, job.ID, job.RunID, timeseriesPath)
		stopTelemetryUploader := startTelemetryUploader(cfg.R2Bucket, job.ID, job.RunID, telemetryPath)

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

		ei, err := runJobWithProgress(cfg.R2Bucket, job.ID, job.RunID, cfg.LogDir, jobCfg)
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

		finalizePhase := fmt.Sprintf("finalizing:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(finalizePhase)
		}
		oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(finalizePhase))
		writePhase(cfg.R2Bucket, cfg.PhaseKey, finalizePhase)

		uploadStartedAt := time.Now()
		uploadStartedUnix := uploadStartedAt.Unix()
		if err := patchPhaseUploadWindow(cfg.LogDir, job.ID, uploadStartedUnix, 0); err != nil {
			fmt.Fprintf(os.Stderr, "patch phase timing for job %d: upload start: %v\n", job.ID, err)
		}

		// Upload output directories when the job produced convention-based outputs.
		if hasOutputDirs(workDir) {
			uploadPhase := fmt.Sprintf("uploading:%d", job.ID)
			if cfg.OnPhase != nil {
				cfg.OnPhase(uploadPhase)
			}
			oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(uploadPhase))
			writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadPhase)
		}

		uploadResult := uploadOutputDirs(cfg.R2Bucket, job.ID, job.RunID, workDir)
		if uploadResult.Status != "ok" {
			failPhase := fmt.Sprintf("upload-failed:%d", job.ID)
			if cfg.OnPhase != nil {
				cfg.OnPhase(failPhase)
			}
			oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(failPhase))
			writePhase(cfg.R2Bucket, cfg.PhaseKey, failPhase)
		}
		patchCompletionUpload(cfg.LogDir, job.ID, &uploadResult)

		// Upload per-job results
		resultsPhase := fmt.Sprintf("uploading-results:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(resultsPhase)
		}
		oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(resultsPhase))
		writePhase(cfg.R2Bucket, cfg.PhaseKey, resultsPhase)
		resultsUpload := uploadJobResults(cfg.R2Bucket, job.ID, job.RunID, cfg.LogDir)
		patchCompletionResultsUpload(cfg.LogDir, job.ID, &resultsUpload)
		uploadEndedUnix := time.Now().Unix()
		if err := patchPhaseUploadWindow(cfg.LogDir, job.ID, uploadStartedUnix, uploadEndedUnix); err != nil {
			fmt.Fprintf(os.Stderr, "patch phase timing for job %d: upload end: %v\n", job.ID, err)
		}
		stopTimeseriesUploader()
		stopTelemetryUploader()
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTimeseries(job.ID, job.RunID))
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTelemetry(job.ID, job.RunID))

		// Promote uv manifest
		promoteUVManifest(cfg.R2Bucket, cfg.LogDir)

		// Upload opslog checkpoint and clean log dir for next job
		uploadOpslog(cfg.R2Bucket, cfg.InstanceID, cfg.LogDir)
		if i < len(jobs)-1 {
			cleanLogDir(cfg.LogDir)
			oplog.Init(filepath.Join(cfg.LogDir, agentOpslogFile), 0)
		}

		// Check for newly submitted jobs via R2 (between-job reuse)
		if newJobs := checkForNewJobs(cfg.R2Bucket, cfg.InstanceID); len(newJobs) > 0 {
			fmt.Printf("Picked up %d new job(s) from R2\n", len(newJobs))
			jobs = append(jobs, newJobs...)
		}
	}

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
