package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// setupInventoryR2 wires OnJobStart/OnJobFinish hooks on the runner to upload
// logs and status to R2. This enables the CLI to fall back to R2 when SSH to
// inventory hosts is unavailable.
func setupInventoryR2(r *runner.Runner, r2Bucket string) {
	// Verify rclone is available
	if _, err := exec.LookPath("rclone"); err != nil {
		fmt.Println("Warning: rclone not found; R2 uploads disabled for inventory host")
		return
	}

	r.OnJobStart = func(jobID int64, logPath string) func() {
		// Write .started marker asynchronously to avoid blocking job startup
		fatalAgentGo("inventory-started-marker", func() {
			ts := fmt.Sprintf("%d", time.Now().Unix())
			if err := r2Put(r2Bucket, r2keys.JobStarted(jobID), ts); err != nil {
				oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
					oplog.WithDetail("inventory .started"), oplog.WithError(err))
			}
		})
		// Start live log uploader (reuse existing; runID=0 for inventory jobs)
		return startLogUploader(r2Bucket, jobID, 0, logPath)
	}

	r.OnJobFinish = func(jobID int64, logDir string, exitCode int) {
		// Upload the job's log directory to R2 via single rclone copy
		uploadInventoryJobResults(r2Bucket, jobID, logDir)
		// Write .complete marker with exit code
		if err := r2Put(r2Bucket, r2keys.JobComplete(jobID), fmt.Sprintf("%d", exitCode)); err != nil {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetail("inventory .complete"), oplog.WithError(err))
		}
	}

	fmt.Printf("R2 uploads enabled (bucket=%s)\n", r2Bucket)
}

// uploadInventoryJobResults uploads the job's log directory to R2 via rclone copy.
func uploadInventoryJobResults(bucket string, jobID int64, logDir string) {
	prefix := r2keys.JobResultsPrefix(jobID)
	dest := fmt.Sprintf("r2:%s/%s", bucket, prefix)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "rclone", "copy", logDir+"/", dest)
	if err := cmd.Run(); err != nil {
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetailf("inventory results dir=%s", runner.NewJobPaths(logDir, jobID).Log),
			oplog.WithError(err), oplog.WithDuration(time.Since(start)))
	}
}
