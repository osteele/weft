package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

	r.OnJobStart = func(jobID, runID int64, logPath string) func() {
		if runID <= 0 {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetail("inventory .started"), oplog.WithError(fmt.Errorf("missing run ID")))
			return nil
		}
		// Write .started marker asynchronously to avoid blocking job startup
		fatalAgentGo("inventory-started-marker", func() {
			ts := fmt.Sprintf("%d", time.Now().Unix())
			if err := r2Put(r2Bucket, r2keys.JobAttemptStarted(jobID, runID), ts); err != nil {
				oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
					oplog.WithDetail("inventory .started"), oplog.WithError(err))
			}
		})
		return startLogUploader(r2Bucket, jobID, runID, logPath)
	}

	r.OnJobFinish = func(jobID, runID int64, logDir string, exitCode int) {
		if runID <= 0 {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetail("inventory .complete"), oplog.WithError(fmt.Errorf("missing run ID")))
			return
		}
		// Write .complete marker with exit code. Post-job output/result
		// uploads run through the shared post-job manager below, which also
		// provides the same-workdir barrier before the next job starts.
		if err := r2Put(r2Bucket, r2keys.JobAttemptComplete(jobID, runID), fmt.Sprintf("%d", exitCode)); err != nil {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetail("inventory .complete"), oplog.WithError(err))
		}
	}
	r.PostJobManager = newInventoryPostJobManager(r2Bucket)
	r.RecoverJobPublication = func(ctx context.Context, capture runner.PostJobCapture) {
		marker, err := r2GetContext(ctx, r2Bucket, r2keys.JobAttemptComplete(capture.JobID, capture.RunID))
		if err != nil {
			oplog.Log(oplog.OpR2Get, oplog.WithJobID(capture.JobID),
				oplog.WithDetail("inventory completion recovery"), oplog.WithError(err))
			return
		}
		if marker != "" {
			return
		}
		// Queue the durable snapshot before publishing the terminal marker.
		// Existing descriptor recovery deduplicates an interrupted handoff.
		if capture.WorkDir != "" {
			r.PostJobManager.StartPostJob(capture)
		}
		if err := r2PutReaderContext(ctx, r2Bucket, r2keys.JobAttemptComplete(capture.JobID, capture.RunID),
			strings.NewReader(fmt.Sprintf("%d", capture.ExitCode))); err != nil {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(capture.JobID),
				oplog.WithDetail("inventory completion recovery"), oplog.WithError(err))
		}
	}

	fmt.Printf("R2 uploads enabled (bucket=%s)\n", r2Bucket)
}

type inventoryPostJobManager struct {
	r2Bucket string
	bgm      *bgWorkManager
}

func newInventoryPostJobManager(r2Bucket string) *inventoryPostJobManager {
	return &inventoryPostJobManager{
		r2Bucket: r2Bucket,
		bgm: newBGWorkManagerForScope(
			nil, true, defaultPublicationWorkers, defaultPublicationQueueCapacity, nil, "inventory",
		),
	}
}

func (m *inventoryPostJobManager) WaitForWorkdir(workdir string) {
	m.bgm.WaitForUploadsInWorkdir(runner.ExpandTilde(workdir))
}

func (m *inventoryPostJobManager) WaitForAll() {
	m.bgm.Barrier()
}

func (m *inventoryPostJobManager) StartPostJob(capture runner.PostJobCapture) {
	if capture.RunID <= 0 {
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(capture.JobID),
			oplog.WithDetail("inventory post-job snapshot"), oplog.WithError(fmt.Errorf("missing run ID")))
		return
	}
	logSnapshot, err := snapshotInventoryJobLogDir(capture.LogDir, capture.JobID, capture.RunID)
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
		cleanupDir:            capture.CleanupDir,
	})
}
