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
		// Write .complete marker with exit code. Post-job output/result
		// uploads run through the shared post-job manager below, which also
		// provides the same-workdir barrier before the next job starts.
		if err := r2Put(r2Bucket, r2keys.JobComplete(jobID), fmt.Sprintf("%d", exitCode)); err != nil {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetail("inventory .complete"), oplog.WithError(err))
		}
	}
	r.PostJobManager = newInventoryPostJobManager(r2Bucket)

	fmt.Printf("R2 uploads enabled (bucket=%s)\n", r2Bucket)
}

type inventoryPostJobManager struct {
	r2Bucket string
	bgm      *bgWorkManager
}

func newInventoryPostJobManager(r2Bucket string) *inventoryPostJobManager {
	return &inventoryPostJobManager{
		r2Bucket: r2Bucket,
		bgm:      newBGWorkManager(nil, true),
	}
}

func (m *inventoryPostJobManager) WaitForWorkdir(workdir string) {
	m.bgm.WaitForUploadsInWorkdir(runner.ExpandTilde(workdir))
}

func (m *inventoryPostJobManager) StartPostJob(capture runner.PostJobCapture) {
	logSnapshot, err := snapshotLogDir(capture.LogDir, capture.JobID, capture.RunID)
	if err != nil {
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(capture.JobID),
			oplog.WithDetail("inventory post-job snapshot"), oplog.WithError(err))
		return
	}
	m.bgm.StartPostJobWork(postJobWork{
		r2Bucket:              m.r2Bucket,
		jobID:                 capture.JobID,
		runID:                 capture.RunID,
		exitCode:              capture.ExitCode,
		workDir:               runner.ExpandTilde(capture.WorkDir),
		logSnapshot:           logSnapshot,
		phase:                 fmt.Sprintf("inventory_uploading:%d", capture.JobID),
		uploadStartedUnix:     time.Now().Unix(),
		outputWindowStartUnix: capture.StartTime,
		outputDirs:            capture.OutputDirs,
	})
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
