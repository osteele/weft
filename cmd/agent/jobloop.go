package main

import (
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

		// Write .started marker to R2
		r2Put(cfg.R2Bucket, r2keys.JobStarted(job.ID), fmt.Sprintf("%d", time.Now().Unix()))

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
			OnPhase: func(phase string) {
				full := fmt.Sprintf("%s:%d", phase, job.ID)
				if cfg.OnPhase != nil {
					cfg.OnPhase(full)
				}
				writePhase(cfg.R2Bucket, cfg.PhaseKey, full)
			},
		}

		ei, err := runJobWithProgress(cfg.R2Bucket, job.ID, cfg.LogDir, jobCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run-job %d failed: %v\n", job.ID, err)
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else if ei.ExitCode != 0 {
			fmt.Printf("Job %d failed (exit %d)\n", job.ID, ei.ExitCode)
			result.AnyFailed = true
			result.FailedJobs = append(result.FailedJobs, job.ID)
		} else {
			fmt.Printf("Job %d completed successfully\n", job.ID)
		}

		uploadPhase := fmt.Sprintf("uploading:%d", job.ID)
		if cfg.OnPhase != nil {
			cfg.OnPhase(uploadPhase)
		}
		writePhase(cfg.R2Bucket, cfg.PhaseKey, uploadPhase)

		// Upload output directories
		uploadOutputDirs(cfg.R2Bucket, job.ID, workDir)

		// Upload per-job results
		uploadJobResults(cfg.R2Bucket, job.ID, cfg.LogDir)

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
