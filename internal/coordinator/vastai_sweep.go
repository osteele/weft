package coordinator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/vastai"
)

// sweepVastaiResults polls R2 for completed Vast.ai job results and processes them.
// It also checks for orphaned instances that should be destroyed.
func (c *Coordinator) sweepVastaiResults() {
	cfg, err := config.Load()
	if err != nil || cfg.Vastai.R2.Bucket == "" {
		return // R2 not configured
	}

	r2Cfg := r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}

	r2Client, err := r2.New(r2Cfg)
	if err != nil {
		c.logger.Printf("r2 client: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Check for completed results
	completedJobIDs, err := r2Client.ListCompleted(ctx, "jobs/")
	if err != nil {
		c.logger.Printf("r2 list completed: %v", err)
		return
	}

	for _, jobIDStr := range completedJobIDs {
		jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
		if err != nil {
			continue
		}
		c.processCompletedVastaiJob(ctx, r2Client, jobID)
	}

	// 2. Check for orphaned instances
	c.checkOrphanedVastaiInstances(cfg)
}

// processCompletedVastaiJob downloads results from R2, updates the job in the DB,
// writes logs to the log cache, and cleans up the R2 prefix.
func (c *Coordinator) processCompletedVastaiJob(ctx context.Context, r2Client *r2.Client, jobID int64) {
	prefix := fmt.Sprintf("jobs/%d", jobID)

	// Download results to temp dir
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("weft-vastai-%d-*", jobID))
	if err != nil {
		c.logger.Printf("vastai sweep: create temp dir for job %d: %v", jobID, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := r2Client.DownloadResults(ctx, prefix+"/results/", tmpDir); err != nil {
		c.logger.Printf("vastai sweep: download results for job %d: %v", jobID, err)
		return
	}

	// Read exit code
	exitCodeBytes, err := os.ReadFile(filepath.Join(tmpDir, "exit_code"))
	if err != nil {
		c.logger.Printf("vastai sweep: read exit_code for job %d: %v", jobID, err)
		return
	}
	exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitCodeBytes)))

	// Read end time
	endTimeBytes, err := os.ReadFile(filepath.Join(tmpDir, "end_time"))
	if err != nil {
		c.logger.Printf("vastai sweep: read end_time for job %d: %v", jobID, err)
		return
	}
	endTimeUnix, _ := strconv.ParseInt(strings.TrimSpace(string(endTimeBytes)), 10, 64)

	// Detect failure reason for non-zero exit codes (OOM, etc.)
	failureReason := ""
	if exitCode != 0 {
		failureReason = detectFailureReason(tmpDir, exitCode)
	}

	// Update job in DB — always StatusCompleted; exit code stored separately
	_, err = c.db.Exec(
		`UPDATE jobs SET status = ?, exit_code = ?, end_time = ?, last_synced_status = ?, failure_reason = ? WHERE id = ?`,
		db.StatusCompleted, exitCode, endTimeUnix, db.StatusCompleted, failureReason, jobID,
	)
	if err != nil {
		c.logger.Printf("vastai sweep: update job %d: %v", jobID, err)
		return
	}

	// Compute and record cost
	job, err := db.GetJobByID(c.db, jobID)
	if err == nil && job != nil && job.StartTime > 0 {
		runtime := time.Duration(endTimeUnix-job.StartTime) * time.Second
		if job.VastaiInstanceID != nil {
			// Estimate cost from runtime (we don't have cost_per_hour stored,
			// but the instance info might be available via the API)
			_ = runtime // Cost calculation requires offer data; handled at launch
		}
	}

	// Write logs to log cache
	writeVastaiLogsToCache(jobID, tmpDir)

	// Clean up R2 prefix
	if err := r2Client.DeletePrefix(ctx, prefix+"/"); err != nil {
		c.logger.Printf("vastai sweep: cleanup R2 for job %d: %v", jobID, err)
	}

	c.logger.Printf("vastai sweep: processed job %d (exit=%d, status=%s)", jobID, exitCode, db.StatusCompleted)
	if exitCode == 0 {
		oplog.LogJob(oplog.OpJobCompleted, jobID, "", oplog.WithDetailf("vastai exit=%d", exitCode))
	} else {
		oplog.LogJob(oplog.OpJobFailed, jobID, "", oplog.WithDetailf("vastai exit=%d reason=%s", exitCode, failureReason))
	}
}

// detectFailureReason examines debug artifacts to determine why a job failed.
// It checks for OOM indicators in dmesg, nvidia-smi output, and exit codes.
func detectFailureReason(tmpDir string, exitCode int) string {
	// Exit code 137 = SIGKILL (often OOM)
	if exitCode == 137 {
		return "oom"
	}

	// Check dmesg for OOM killer
	dmesgPath := filepath.Join(tmpDir, "debug", "dmesg.log")
	if dmesgBytes, err := os.ReadFile(dmesgPath); err == nil {
		dmesg := string(dmesgBytes)
		if strings.Contains(dmesg, "Out of memory") || strings.Contains(dmesg, "oom-kill") || strings.Contains(dmesg, "Killed process") {
			return "oom"
		}
	}

	// Check nvidia-smi for GPU memory issues
	nvidiaSmiPath := filepath.Join(tmpDir, "debug", "nvidia-smi.log")
	if nvBytes, err := os.ReadFile(nvidiaSmiPath); err == nil {
		nvOutput := string(nvBytes)
		if strings.Contains(nvOutput, "out of memory") || strings.Contains(nvOutput, "CUDA_ERROR_OUT_OF_MEMORY") {
			return "gpu_oom"
		}
	}

	if exitCode == 1 {
		return "error"
	}
	return fmt.Sprintf("exit_%d", exitCode)
}

// writeVastaiLogsToCache writes stdout/stderr from R2 results into the local log cache.
func writeVastaiLogsToCache(jobID int64, tmpDir string) {
	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")

	var combined strings.Builder

	if stdout, err := os.ReadFile(stdoutPath); err == nil {
		combined.Write(stdout)
	}
	if stderr, err := os.ReadFile(stderrPath); err == nil {
		if combined.Len() > 0 {
			combined.WriteString("\n--- stderr ---\n")
		}
		combined.Write(stderr)
	}

	if combined.Len() > 0 {
		_ = logcache.Write(jobID, combined.String())
	}
}

// checkOrphanedVastaiInstances looks for jobs that have been running too long
// or have instances that are no longer active.
func (c *Coordinator) checkOrphanedVastaiInstances(cfg *config.Config) {
	jobs, err := db.ListActiveVastaiJobs(c.db)
	if err != nil {
		c.logger.Printf("vastai sweep: list active jobs: %v", err)
		return
	}

	maxRuntime := 4 * time.Hour
	if cfg.Vastai.MaxRuntime != "" {
		if d, err := time.ParseDuration(cfg.Vastai.MaxRuntime); err == nil {
			maxRuntime = d
		}
	}

	client := vastai.NewClient()
	if err := client.Available(); err != nil {
		return // vastai CLI not available on coordinator
	}

	for _, job := range jobs {
		if job.VastaiInstanceID == nil {
			continue
		}

		age := time.Since(time.Unix(job.CreatedAt, 0))
		if age < maxRuntime {
			continue
		}

		// Job exceeded max runtime — check instance status
		instanceID := *job.VastaiInstanceID
		inst, err := client.ShowInstance(instanceID)
		if err != nil {
			// Instance not found — mark job as failed
			c.logger.Printf("vastai sweep: instance %d not found for job %d, marking failed", instanceID, job.ID)
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "orphaned", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as orphaned: %v", job.ID, err)
			}
			continue
		}

		if inst.Status == "running" {
			// Still running past max runtime — destroy it
			c.logger.Printf("vastai sweep: destroying orphaned instance %d (job %d, age %v)", instanceID, job.ID, age)
			if err := client.DestroyInstance(instanceID); err != nil {
				c.logger.Printf("vastai sweep: failed to destroy instance %d: %v", instanceID, err)
			}
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "timeout", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as timed out: %v", job.ID, err)
			}
			oplog.LogJob(oplog.OpJobKill, job.ID, "", oplog.WithDetailf("vastai orphan timeout instance=%d", instanceID))
		} else if inst.Status == "exited" || inst.Status == "error" {
			// Instance already exited but no results in R2 — mark failed
			if _, err := c.db.Exec(
				`UPDATE jobs SET status = ?, failure_reason = ?, end_time = ?, last_synced_status = ? WHERE id = ?`,
				db.StatusFailed, "instance_exited", time.Now().Unix(), db.StatusFailed, job.ID,
			); err != nil {
				c.logger.Printf("vastai sweep: failed to mark job %d as instance_exited: %v", job.ID, err)
			}
		}
	}
}
