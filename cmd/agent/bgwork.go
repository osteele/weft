package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

const staleLogSnapshotAge = 6 * time.Hour

// bgWorkManager manages background post-job goroutines (uploads) and tracks
// the set of workdirs touched during a campaign for end-of-campaign cleanup.
// It also provides a barrier for benchmark jobs that need a quiescent system.
//
// Workdir deletion is deferred until CleanupWorkdirs() so a later job in the
// same campaign cannot race a per-job cleanup for a shared source tree.
type bgWorkManager struct {
	wg               sync.WaitGroup
	mu               sync.Mutex
	errors           []bgWorkError
	summaries        map[int64]runner.JobCompletionSummary
	workdirs         map[string]struct{} // resolved absolute paths touched this campaign
	uploadsByWorkdir map[string]*sync.WaitGroup
	skipDeletion     bool
}

type bgWorkError struct {
	JobID int64
	Op    string
	Err   error
}

// postJobWork describes the background work to do after a job completes.
type postJobWork struct {
	r2Bucket          string
	instanceID        int64
	jobID             int64
	runID             int64
	exitCode          int
	workDir           string // job's working directory (resolved absolute path)
	logSnapshot       string // temp dir containing log snapshot
	diskPath          string
	phase             string
	uploadStartedUnix int64
	// outputWindowStartUnix windows the convention-output upload to this
	// attempt (see uploadOutputDirs); 0 disables the window. Callers apply
	// policy before setting it (e.g. restaged-output attempts pass 0).
	outputWindowStartUnix int64
	// outputDirs carries the job's configured convention output dirs;
	// empty falls back to the defaults (see config.EffectiveOutputDirs).
	outputDirs []string
}

type maintenanceReport struct {
	Ts                          int64           `json:"ts"`
	JobID                       int64           `json:"job_id"`
	RunID                       int64           `json:"run_id,omitempty"`
	ExitCode                    int             `json:"exit_code"`
	Phase                       string          `json:"phase,omitempty"`
	Heartbeat                   HeartbeatSample `json:"heartbeat"`
	WorkdirBytes                int64           `json:"workdir_bytes,omitempty"`
	LogSnapshotBytes            int64           `json:"log_snapshot_bytes,omitempty"`
	PrunedStaleLogSnapshots     int             `json:"pruned_stale_log_snapshots,omitempty"`
	PrunedStaleLogSnapshotBytes int64           `json:"pruned_stale_log_snapshot_bytes,omitempty"`
}

func newBGWorkManager(jobs []cloud.AgentJob, skipDeletion bool) *bgWorkManager {
	m := &bgWorkManager{
		summaries:        make(map[int64]runner.JobCompletionSummary),
		workdirs:         make(map[string]struct{}),
		uploadsByWorkdir: make(map[string]*sync.WaitGroup),
		skipDeletion:     skipDeletion,
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
	m.registerUpload(pw.workDir)
	fatalAgentGo("post-job-work", func() {
		defer m.wg.Done()
		defer m.completeUpload(pw.workDir)

		uploadResult := uploadOutputDirs(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir, pw.outputWindowStartUnix, pw.outputDirs)
		if uploadResult.Status != "ok" {
			m.recordError(pw.jobID, "upload-outputs", fmt.Errorf("status=%s", uploadResult.Status))
		}

		artifactResult := uploadArtifactManifestEntries(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir)
		if artifactResult.Status != "ok" {
			m.recordError(pw.jobID, "upload-artifacts", fmt.Errorf("status=%s", artifactResult.Status))
		}
		mergeUploadResults(&uploadResult, &artifactResult)

		patchCompletionUpload(pw.logSnapshot, pw.jobID, &uploadResult)

		resultsUpload := uploadJobResults(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
		patchCompletionResultsUpload(pw.logSnapshot, pw.jobID, &resultsUpload)

		uploadEndedUnix := uploadResult.CompletedAtUnix
		if resultsUpload.CompletedAtUnix > uploadEndedUnix {
			uploadEndedUnix = resultsUpload.CompletedAtUnix
		}
		_ = patchPhaseUploadWindow(pw.logSnapshot, pw.jobID, pw.uploadStartedUnix, uploadEndedUnix)
		if summary, _ := summarizeJobCompletion(pw.logSnapshot, pw.jobID); summary.JobID != 0 {
			m.recordSummary(summary)
		}

		reuploadCompletion(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
		repairR2Markers(pw)
		report, err := collectMaintenanceReport(pw)
		if err != nil {
			m.recordError(pw.jobID, "collect-maintenance", err)
		}
		uploadMaintenanceReport(pw.r2Bucket, pw.instanceID, report)
		if report.PrunedStaleLogSnapshots > 0 {
			slog.Debug("pruned stale log snapshots",
				"count", report.PrunedStaleLogSnapshots,
				"bytes", report.PrunedStaleLogSnapshotBytes)
		}
		os.RemoveAll(pw.logSnapshot)
	})
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

// WaitForUploadsInWorkdir blocks until every post-job upload currently in
// flight for workdir has completed. See rule
// SharedWorkdirUploadBarrierBeforeNextJob in specs/job-lifecycle.allium.
func (m *bgWorkManager) WaitForUploadsInWorkdir(workdir string) {
	if workdir == "" {
		return
	}
	m.mu.Lock()
	wg := m.uploadsByWorkdir[workdir]
	m.mu.Unlock()
	if wg != nil {
		wg.Wait()
	}
}

func (m *bgWorkManager) registerUpload(workdir string) {
	if workdir == "" {
		return
	}
	m.mu.Lock()
	wg, ok := m.uploadsByWorkdir[workdir]
	if !ok {
		wg = &sync.WaitGroup{}
		m.uploadsByWorkdir[workdir] = wg
	}
	wg.Add(1)
	m.mu.Unlock()
}

func (m *bgWorkManager) completeUpload(workdir string) {
	if workdir == "" {
		return
	}
	m.mu.Lock()
	wg := m.uploadsByWorkdir[workdir]
	m.mu.Unlock()
	if wg != nil {
		wg.Done()
	}
}

func (m *bgWorkManager) recordError(jobID int64, op string, err error) {
	m.mu.Lock()
	m.errors = append(m.errors, bgWorkError{JobID: jobID, Op: op, Err: err})
	m.mu.Unlock()
}

func (m *bgWorkManager) recordSummary(summary runner.JobCompletionSummary) {
	m.mu.Lock()
	m.summaries[summary.JobID] = summary
	m.mu.Unlock()
}

func (m *bgWorkManager) CompletionSummaries() []runner.JobCompletionSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	summaries := make([]runner.JobCompletionSummary, 0, len(m.summaries))
	for _, summary := range m.summaries {
		summaries = append(summaries, summary)
	}
	return summaries
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
// and writes it back to R2 so local sync sees the final upload metadata.
func reuploadCompletion(bucket string, jobID, runID int64, snapshotDir string) {
	paths := runner.NewJobPaths(snapshotDir, jobID)
	data, err := os.ReadFile(paths.Completion)
	if err != nil {
		return // no completion record to re-upload
	}
	key := r2keys.JobAttemptResultsPrefix(jobID, runID) + fmt.Sprintf("%d.completion.json", jobID)
	r2Put(bucket, key, string(data))
}

func repairR2Markers(pw postJobWork) {
	if pw.r2Bucket == "" {
		return
	}
	_ = r2Put(pw.r2Bucket, r2keys.JobAttemptComplete(pw.jobID, pw.runID), fmt.Sprintf("%d", pw.exitCode))
	_ = r2Delete(pw.r2Bucket, r2keys.JobAttemptProgress(pw.jobID, pw.runID))
	_ = r2Delete(pw.r2Bucket, r2keys.JobAttemptLiveTimeseries(pw.jobID, pw.runID))
	_ = r2Delete(pw.r2Bucket, r2keys.JobAttemptLiveTelemetry(pw.jobID, pw.runID))
}

func collectMaintenanceReport(pw postJobWork) (maintenanceReport, error) {
	report := maintenanceReport{
		Ts:        time.Now().Unix(),
		JobID:     pw.jobID,
		RunID:     pw.runID,
		ExitCode:  pw.exitCode,
		Phase:     pw.phase,
		Heartbeat: collectHeartbeat(pw.phase, pw.diskPath),
	}
	if pw.workDir != "" {
		report.WorkdirBytes = measureLocalTree(pw.workDir)
	}
	if pw.logSnapshot != "" {
		report.LogSnapshotBytes = measureLocalTree(pw.logSnapshot)
	}
	pruned, bytes, err := pruneStaleLogSnapshots(pw.logSnapshot, time.Now())
	report.PrunedStaleLogSnapshots = pruned
	report.PrunedStaleLogSnapshotBytes = bytes
	return report, err
}

func uploadMaintenanceReport(bucket string, instanceID int64, report maintenanceReport) {
	if bucket == "" {
		return
	}
	data, err := json.Marshal(report)
	if err != nil {
		return
	}
	if report.JobID > 0 {
		_ = r2Put(bucket, r2keys.JobAttemptMaintenance(report.JobID, report.RunID), string(data))
	}
	if instanceID > 0 {
		_ = r2Put(bucket, r2keys.InstanceMaintenance(instanceID), string(data))
	}
}

func pruneStaleLogSnapshots(current string, now time.Time) (int, int64, error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0, 0, err
	}
	current = filepath.Clean(current)
	var pruned int
	var bytes int64
	var errs []error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "weft-logs-job-") {
			continue
		}
		path := filepath.Join(os.TempDir(), entry.Name())
		if filepath.Clean(path) == current {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if now.Sub(info.ModTime()) < staleLogSnapshotAge {
			continue
		}
		size := measureLocalTree(path)
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
			continue
		}
		pruned++
		bytes += size
	}
	return pruned, bytes, errors.Join(errs...)
}

func measureLocalTree(root string) int64 {
	var bytes int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		bytes += info.Size()
		return nil
	})
	return bytes
}
