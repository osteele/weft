package main

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// bgWorkManager manages background post-job goroutines (uploads) and tracks
// the set of workdirs touched during a campaign for end-of-campaign cleanup.
// It also provides a barrier for benchmark jobs that need a quiescent system.
//
// Workdir deletion is intentionally deferred until CleanupWorkdirs() is called
// at end of campaign. Eager per-job deletion (which previously fired when a
// refcount hit zero) was racy with checkForNewJobs: a new job for the same
// workdir could be picked up after the cleanup goroutine had already started
// os.RemoveAll, leaving the next job to run against a missing/partial source
// tree (e.g. pyproject.toml gone, uv sync silently skipped, ModuleNotFoundError).
type bgWorkManager struct {
	wg           sync.WaitGroup
	mu           sync.Mutex
	errors       []bgWorkError
	workdirs     map[string]struct{} // resolved absolute paths touched this campaign
	skipDeletion bool
}

type bgWorkError struct {
	JobID int64
	Op    string
	Err   error
}

// postJobWork describes the background work to do after a job completes.
type postJobWork struct {
	r2Bucket          string
	jobID             int64
	runID             int64
	workDir           string // job's working directory (resolved absolute path)
	logSnapshot       string // temp dir containing log snapshot
	uploadStartedUnix int64
}

func newBGWorkManager(jobs []cloud.AgentJob, skipDeletion bool) *bgWorkManager {
	m := &bgWorkManager{
		workdirs:     make(map[string]struct{}),
		skipDeletion: skipDeletion,
	}
	for _, job := range jobs {
		dir := runner.ExpandTilde(job.Dir)
		if dir != "" {
			m.workdirs[dir] = struct{}{}
		}
	}
	return m
}

// RegisterNewJobs records workdirs for dynamically submitted jobs so that
// CleanupWorkdirs cleans them up at end of campaign.
func (m *bgWorkManager) RegisterNewJobs(jobs []cloud.AgentJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range jobs {
		dir := runner.ExpandTilde(job.Dir)
		if dir != "" {
			m.workdirs[dir] = struct{}{}
		}
	}
}

// StartPostJobWork launches background goroutines for output uploads,
// result uploads, completion patching, and workdir cleanup.
func (m *bgWorkManager) StartPostJobWork(pw postJobWork) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		uploadResult := uploadOutputDirs(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir)
		if uploadResult.Status != "ok" {
			m.recordError(pw.jobID, "upload-outputs", fmt.Errorf("status=%s", uploadResult.Status))
		}

		artifactResult := uploadArtifactManifestEntries(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir)
		if artifactResult.Status != "ok" {
			m.recordError(pw.jobID, "upload-artifacts", fmt.Errorf("status=%s", artifactResult.Status))
		}
		uploadResult.FileCount += artifactResult.FileCount
		uploadResult.Bytes += artifactResult.Bytes
		if artifactResult.CompletedAtUnix > uploadResult.CompletedAtUnix {
			uploadResult.CompletedAtUnix = artifactResult.CompletedAtUnix
		}

		patchCompletionUpload(pw.logSnapshot, pw.jobID, &uploadResult)

		resultsUpload := uploadJobResults(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
		patchCompletionResultsUpload(pw.logSnapshot, pw.jobID, &resultsUpload)

		uploadEndedUnix := uploadResult.CompletedAtUnix
		if resultsUpload.CompletedAtUnix > uploadEndedUnix {
			uploadEndedUnix = resultsUpload.CompletedAtUnix
		}
		_ = patchPhaseUploadWindow(pw.logSnapshot, pw.jobID, pw.uploadStartedUnix, uploadEndedUnix)

		reuploadCompletion(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
		os.RemoveAll(pw.logSnapshot)
	}()
}

// Barrier blocks until all background work completes, then logs any errors.
func (m *bgWorkManager) Barrier() {
	m.wg.Wait()
	m.mu.Lock()
	errs := m.errors
	m.errors = nil
	m.mu.Unlock()
	for _, e := range errs {
		slog.Warn("background work error", "job_id", e.JobID, "op", e.Op, "error", e.Err)
	}
}

func (m *bgWorkManager) recordError(jobID int64, op string, err error) {
	m.mu.Lock()
	m.errors = append(m.errors, bgWorkError{JobID: jobID, Op: op, Err: err})
	m.mu.Unlock()
}

// CleanupWorkdirs deletes every workdir touched during the campaign. Call
// after Barrier() at end of campaign, when no more jobs will be picked up.
//
// Deferred to campaign end (rather than refcount-driven per-job) because
// checkForNewJobs can hand the agent additional jobs for an already-finished
// workdir; per-job cleanup races with that pickup and produces silent
// missing-source failures.
func (m *bgWorkManager) CleanupWorkdirs() {
	if m.skipDeletion {
		return
	}
	m.mu.Lock()
	dirs := make([]string, 0, len(m.workdirs))
	for d := range m.workdirs {
		dirs = append(dirs, d)
	}
	m.mu.Unlock()

	for _, d := range dirs {
		slog.Debug("deleting workdir", "path", d)
		if err := os.RemoveAll(d); err != nil {
			m.recordError(0, "delete-workdir", err)
		}
	}
}

// reuploadCompletion reads the patched completion.json from the log snapshot
// and writes it back to R2 so the coordinator sees the final upload metadata.
func reuploadCompletion(bucket string, jobID, runID int64, snapshotDir string) {
	paths := runner.NewJobPaths(snapshotDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		return // no completion record to re-upload
	}
	key := r2keys.JobAttemptResultsPrefix(jobID, runID) + fmt.Sprintf("%d.completion.json", jobID)
	r2Put(bucket, key, string(data))
}
