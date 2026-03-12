package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/osteele/weft/internal/cloud"
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
	Workspace  string
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

	for i, job := range jobs {
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
		timeseriesPath := runner.NewJobPaths(cfg.LogDir, job.ID).Timeseries
		stopTimeseriesUploader := startTimeseriesUploader(cfg.R2Bucket, job.ID, job.RunID, timeseriesPath)

		workDir := job.Dir
		if workDir == "" {
			workDir = cfg.Workspace
		}

		// Compute per-job max time from remaining budget
		var jobMaxTime time.Duration
		if cfg.MaxTime > 0 {
			jobMaxTime = cfg.MaxTime - time.Since(cfg.StartTime)
		}

		jobCfg := runner.SingleJobConfig{
			JobID: job.ID,
			Job: ops.CommandJob{
				Cmd: job.Command,
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

		uploadPhase := fmt.Sprintf("uploading:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(uploadPhase)
		}
		oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(job.ID), oplog.WithDetail(uploadPhase))
		writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadPhase)

		// Upload output directories
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
		uploadJobResults(cfg.R2Bucket, job.ID, job.RunID, cfg.LogDir)
		stopTimeseriesUploader()
		r2Delete(cfg.R2Bucket, r2keys.JobAttemptLiveTimeseries(job.ID, job.RunID))

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
	if upload == nil {
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

	rec.OutputUpload = upload
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
