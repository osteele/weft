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

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
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
	cond             *sync.Cond
	errors           []bgWorkError
	summaries        map[int64]runner.JobCompletionSummary
	workdirs         map[string]struct{} // resolved absolute paths touched this campaign
	uploadsByWorkdir map[string]*sync.WaitGroup
	skipDeletion     bool
	queue            []*queuedPostJobWork
	active           map[string]*queuedPostJobWork
	known            map[string]*queuedPostJobWork
	queueCapacity    int
	workerLimit      int
	maxRetainedBytes int64
	closed           bool
	state            *publicationState
	descriptorDir    string
	stageRunner      func(*queuedPostJobWork) bool
}

const (
	defaultPublicationWorkers       = 1
	defaultPublicationQueueCapacity = 16
)

type publicationStage int

const (
	publicationDiagnostics publicationStage = iota
	publicationRequiredArtifacts
	publicationConventionOutputs
	publicationFinalize
)

func (s publicationStage) String() string {
	switch s {
	case publicationDiagnostics:
		return "diagnostic_results"
	case publicationRequiredArtifacts:
		return "required_artifact"
	case publicationConventionOutputs:
		return "convention_output"
	case publicationFinalize:
		return "maintenance"
	default:
		return "unknown"
	}
}

type queuedPostJobWork struct {
	pw             postJobWork
	stage          publicationStage
	enqueuedAt     time.Time
	startedAt      time.Time
	estimatedBytes *int64
	retainedBytes  *int64
	workdirBytes   *int64
	snapshotBytes  *int64
	resultsUpload  runner.UploadSummary
	artifactResult runner.OutputUploadResult
	outputResult   runner.OutputUploadResult
	report         runner.PublicationReport
}

func (w *queuedPostJobWork) key() string {
	return fmt.Sprintf("%d/%d", w.pw.jobID, w.pw.runID)
}

// publicationSnapshot is embedded in agent heartbeats and maintenance reports.
// Pointer-valued byte fields preserve unknown separately from measured zero.
type publicationSnapshot struct {
	QueuedItems        int    `json:"queued_items"`
	QueuedBytes        *int64 `json:"queued_bytes,omitempty"`
	InflightItems      int    `json:"inflight_items"`
	InflightBytes      *int64 `json:"inflight_bytes,omitempty"`
	OldestQueuedAtUnix int64  `json:"oldest_queued_at_unix,omitempty"`
	RetainedBytes      *int64 `json:"retained_bytes,omitempty"`
	WorkerLimit        int    `json:"worker_limit"`
	WorkersBusy        int    `json:"workers_busy"`
	LastProgressAtUnix int64  `json:"last_progress_at_unix,omitempty"`
}

type publicationState struct {
	mu       sync.RWMutex
	snapshot publicationSnapshot
}

const publicationDescriptorVersion = 1

type publicationDescriptor struct {
	Version               int                       `json:"version"`
	Stage                 publicationStage          `json:"stage"`
	R2Bucket              string                    `json:"r2_bucket"`
	InstanceID            int64                     `json:"instance_id,omitempty"`
	JobID                 int64                     `json:"job_id"`
	RunID                 int64                     `json:"run_id"`
	ExitCode              int                       `json:"exit_code"`
	WorkDir               string                    `json:"work_dir"`
	LogSnapshot           string                    `json:"log_snapshot"`
	DiskPath              string                    `json:"disk_path,omitempty"`
	Phase                 string                    `json:"phase,omitempty"`
	UploadStartedUnix     int64                     `json:"upload_started_unix,omitempty"`
	OutputWindowStartUnix int64                     `json:"output_window_start_unix,omitempty"`
	OutputDirs            []string                  `json:"output_dirs,omitempty"`
	CleanupDir            string                    `json:"cleanup_dir,omitempty"`
	EnqueuedAtUnix        int64                     `json:"enqueued_at_unix"`
	ResultsUpload         runner.UploadSummary      `json:"results_upload,omitempty"`
	ArtifactResult        runner.OutputUploadResult `json:"artifact_result,omitempty"`
	OutputResult          runner.OutputUploadResult `json:"output_result,omitempty"`
	Report                runner.PublicationReport  `json:"report,omitempty"`
}

func (s *publicationState) set(snapshot publicationSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot = snapshot
	s.mu.Unlock()
}

func (s *publicationState) get() publicationSnapshot {
	if s == nil {
		return publicationSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func publicationDescriptorDir(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return ""
	}
	scope = filepath.Base(strings.TrimSpace(scope))
	if scope == "." || scope == string(filepath.Separator) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "weft", "publication", scope)
}

func (m *bgWorkManager) descriptorPath(work *queuedPostJobWork) string {
	if m == nil || m.descriptorDir == "" || work == nil {
		return ""
	}
	return filepath.Join(m.descriptorDir, fmt.Sprintf("%d-%d.json", work.pw.jobID, work.pw.runID))
}

func descriptorForWork(work *queuedPostJobWork) publicationDescriptor {
	pw := work.pw
	return publicationDescriptor{
		Version:               publicationDescriptorVersion,
		Stage:                 work.stage,
		R2Bucket:              pw.r2Bucket,
		InstanceID:            pw.instanceID,
		JobID:                 pw.jobID,
		RunID:                 pw.runID,
		ExitCode:              pw.exitCode,
		WorkDir:               pw.workDir,
		LogSnapshot:           pw.logSnapshot,
		DiskPath:              pw.diskPath,
		Phase:                 pw.phase,
		UploadStartedUnix:     pw.uploadStartedUnix,
		OutputWindowStartUnix: pw.outputWindowStartUnix,
		OutputDirs:            append([]string(nil), pw.outputDirs...),
		CleanupDir:            pw.cleanupDir,
		EnqueuedAtUnix:        work.enqueuedAt.Unix(),
		ResultsUpload:         work.resultsUpload,
		ArtifactResult:        work.artifactResult,
		OutputResult:          work.outputResult,
		Report:                work.report,
	}
}

func workFromDescriptor(d publicationDescriptor) *queuedPostJobWork {
	enqueuedAt := time.Unix(d.EnqueuedAtUnix, 0)
	if d.EnqueuedAtUnix <= 0 {
		enqueuedAt = time.Now()
	}
	pw := postJobWork{
		r2Bucket:              d.R2Bucket,
		instanceID:            d.InstanceID,
		jobID:                 d.JobID,
		runID:                 d.RunID,
		exitCode:              d.ExitCode,
		workDir:               d.WorkDir,
		logSnapshot:           d.LogSnapshot,
		diskPath:              d.DiskPath,
		phase:                 d.Phase,
		uploadStartedUnix:     d.UploadStartedUnix,
		outputWindowStartUnix: d.OutputWindowStartUnix,
		outputDirs:            append([]string(nil), d.OutputDirs...),
		cleanupDir:            d.CleanupDir,
	}
	estimated, retained, workdirBytes, snapshotBytes := estimatePostJobBytes(pw)
	work := &queuedPostJobWork{
		pw:             pw,
		stage:          d.Stage,
		enqueuedAt:     enqueuedAt,
		estimatedBytes: estimated,
		retainedBytes:  retained,
		workdirBytes:   workdirBytes,
		snapshotBytes:  snapshotBytes,
		resultsUpload:  d.ResultsUpload,
		artifactResult: d.ArtifactResult,
		outputResult:   d.OutputResult,
		report:         d.Report,
	}
	if d.Stage > publicationDiagnostics {
		consumeEstimatedBytes(work, d.ResultsUpload.Bytes)
	}
	if d.Stage > publicationRequiredArtifacts {
		consumeEstimatedBytes(work, d.ArtifactResult.Bytes)
	}
	if d.Stage > publicationConventionOutputs {
		consumeEstimatedBytes(work, d.OutputResult.Bytes)
	}
	return work
}

func (m *bgWorkManager) persistPublicationDescriptorLocked(work *queuedPostJobWork) error {
	path := m.descriptorPath(work)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(descriptorForWork(work), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *bgWorkManager) removePublicationDescriptorLocked(work *queuedPostJobWork) {
	path := m.descriptorPath(work)
	if path != "" {
		_ = os.Remove(path)
	}
}

func (m *bgWorkManager) recoverPublicationDescriptorsLocked() {
	if m.descriptorDir == "" {
		return
	}
	entries, err := os.ReadDir(m.descriptorDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.descriptorDir, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			m.errors = append(m.errors, bgWorkError{Op: "recover-publication", Err: readErr})
			continue
		}
		var d publicationDescriptor
		if parseErr := json.Unmarshal(data, &d); parseErr != nil || d.Version != publicationDescriptorVersion || d.JobID <= 0 {
			if parseErr == nil {
				parseErr = fmt.Errorf("unsupported publication descriptor version %d", d.Version)
			}
			m.errors = append(m.errors, bgWorkError{JobID: d.JobID, Op: "recover-publication", Err: parseErr})
			continue
		}
		work := workFromDescriptor(d)
		if _, exists := m.known[work.key()]; exists {
			continue
		}
		m.known[work.key()] = work
		m.registerUploadLocked(work.pw.workDir)
		m.queue = append(m.queue, work)
		m.wg.Add(1)
	}
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
	// cleanupDir is owned by the publication chain and removed only after its
	// payload sources are no longer needed. Inventory R2-isolated jobs use this
	// to keep their per-job source tree alive until output upload completes.
	cleanupDir string
}

type maintenanceReport struct {
	Ts                          int64               `json:"ts"`
	JobID                       int64               `json:"job_id"`
	RunID                       int64               `json:"run_id,omitempty"`
	ExitCode                    int                 `json:"exit_code"`
	Phase                       string              `json:"phase,omitempty"`
	Heartbeat                   HeartbeatSample     `json:"heartbeat"`
	WorkdirBytes                int64               `json:"workdir_bytes,omitempty"`
	LogSnapshotBytes            int64               `json:"log_snapshot_bytes,omitempty"`
	PrunedStaleLogSnapshots     int                 `json:"pruned_stale_log_snapshots,omitempty"`
	PrunedStaleLogSnapshotBytes int64               `json:"pruned_stale_log_snapshot_bytes,omitempty"`
	Publication                 publicationSnapshot `json:"publication"`
}

func newBGWorkManager(jobs []cloud.AgentJob, skipDeletion bool) *bgWorkManager {
	return newBGWorkManagerWithOptions(jobs, skipDeletion, defaultPublicationWorkers, defaultPublicationQueueCapacity, nil)
}

func newBGWorkManagerWithOptions(
	jobs []cloud.AgentJob,
	skipDeletion bool,
	workerLimit int,
	queueCapacity int,
	state *publicationState,
) *bgWorkManager {
	return newBGWorkManagerForScope(jobs, skipDeletion, workerLimit, queueCapacity, state, "")
}

func newBGWorkManagerForScope(
	jobs []cloud.AgentJob,
	skipDeletion bool,
	workerLimit int,
	queueCapacity int,
	state *publicationState,
	scope string,
) *bgWorkManager {
	if workerLimit <= 0 {
		workerLimit = defaultPublicationWorkers
	}
	if queueCapacity <= 0 {
		queueCapacity = defaultPublicationQueueCapacity
	}
	if state == nil {
		state = &publicationState{}
	}
	m := &bgWorkManager{
		summaries:        make(map[int64]runner.JobCompletionSummary),
		workdirs:         make(map[string]struct{}),
		uploadsByWorkdir: make(map[string]*sync.WaitGroup),
		active:           make(map[string]*queuedPostJobWork),
		known:            make(map[string]*queuedPostJobWork),
		skipDeletion:     skipDeletion,
		workerLimit:      workerLimit,
		queueCapacity:    queueCapacity,
		state:            state,
		descriptorDir:    publicationDescriptorDir(scope),
	}
	m.cond = sync.NewCond(&m.mu)
	for _, job := range jobs {
		dir := runner.ExpandTilde(job.Dir)
		if dir != "" {
			m.workdirs[dir] = struct{}{}
		}
	}
	m.recoverPublicationDescriptorsLocked()
	m.updatePublicationStateLocked(time.Time{})
	for i := 0; i < workerLimit; i++ {
		workerID := i
		fatalAgentGo(fmt.Sprintf("publication-worker-%d", workerID), m.runWorker)
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

// StartPostJobWork admits one post-job publication chain to the bounded queue.
// A worker executes one stage at a time and requeues the chain at its next
// priority. This lets a later job's diagnostic record run before an earlier
// job's bulk convention output without creating one goroutine per job.
func (m *bgWorkManager) StartPostJobWork(pw postJobWork) {
	estimated, retained, workdirBytes, snapshotBytes := estimatePostJobBytes(pw)
	work := &queuedPostJobWork{
		pw:             pw,
		stage:          publicationDiagnostics,
		enqueuedAt:     time.Now(),
		estimatedBytes: estimated,
		retainedBytes:  retained,
		workdirBytes:   workdirBytes,
		snapshotBytes:  snapshotBytes,
	}

	m.mu.Lock()
	for !m.closed && m.admissionBlockedLocked(work) {
		m.cond.Wait()
	}
	if m.closed {
		m.mu.Unlock()
		m.recordError(pw.jobID, "enqueue", errors.New("publication manager is closed"))
		return
	}
	if _, exists := m.known[work.key()]; exists {
		m.mu.Unlock()
		return
	}
	m.registerUploadLocked(pw.workDir)
	m.known[work.key()] = work
	if err := m.persistPublicationDescriptorLocked(work); err != nil {
		m.errors = append(m.errors, bgWorkError{JobID: pw.jobID, Op: "persist-publication", Err: err})
	}
	m.wg.Add(1)
	m.queue = append(m.queue, work)
	m.updatePublicationStateLocked(time.Time{})
	m.cond.Signal()
	m.mu.Unlock()
}

func (m *bgWorkManager) admissionBlockedLocked(incoming *queuedPostJobWork) bool {
	if len(m.queue)+len(m.active) >= m.queueCapacity {
		return true
	}
	if m.maxRetainedBytes <= 0 || len(m.queue)+len(m.active) == 0 {
		return false
	}
	works := append([]*queuedPostJobWork(nil), m.queue...)
	for _, active := range m.active {
		works = append(works, active)
	}
	works = append(works, incoming)
	retained, known := sumRetainedBytes(works)
	// Unknown disk retention cannot authorize admitting more work. Once the
	// current chain drains, a single unknown-size chain is allowed so the
	// queue can make progress rather than deadlock.
	return !known || retained > m.maxRetainedBytes
}

func (m *bgWorkManager) runWorker() {
	for {
		m.mu.Lock()
		for len(m.queue) == 0 && !m.closed {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		idx := nextPublicationWork(m.queue)
		work := m.queue[idx]
		m.queue = append(m.queue[:idx], m.queue[idx+1:]...)
		work.startedAt = time.Now()
		m.active[work.key()] = work
		m.updatePublicationStateLocked(time.Time{})
		m.cond.Broadcast()
		m.mu.Unlock()

		complete := false
		if m.stageRunner != nil {
			complete = m.stageRunner(work)
		} else {
			complete = m.runPublicationStage(work)
		}
		progressAt := time.Now()

		m.mu.Lock()
		delete(m.active, work.key())
		if complete {
			delete(m.known, work.key())
			m.removePublicationDescriptorLocked(work)
			m.wg.Done()
		} else {
			work.stage++
			work.enqueuedAt = progressAt
			if err := m.persistPublicationDescriptorLocked(work); err != nil {
				m.errors = append(m.errors, bgWorkError{JobID: work.pw.jobID, Op: "persist-publication", Err: err})
			}
			m.queue = append(m.queue, work)
		}
		m.updatePublicationStateLocked(progressAt)
		m.cond.Broadcast()
		m.mu.Unlock()

		if complete {
			m.completeUpload(work.pw.workDir)
		}
	}
}

func nextPublicationWork(queue []*queuedPostJobWork) int {
	best := 0
	for i := 1; i < len(queue); i++ {
		if queue[i].stage < queue[best].stage ||
			(queue[i].stage == queue[best].stage && queue[i].enqueuedAt.Before(queue[best].enqueuedAt)) {
			best = i
		}
	}
	return best
}

func (m *bgWorkManager) runPublicationStage(work *queuedPostJobWork) bool {
	pw := work.pw
	switch work.stage {
	case publicationDiagnostics:
		work.resultsUpload = uploadJobResults(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
		consumeEstimatedBytes(work, work.resultsUpload.Bytes)
		if work.resultsUpload.Status != runner.UploadStatusOK {
			m.recordError(pw.jobID, "upload-results", fmt.Errorf("status=%s", work.resultsUpload.Status))
		}
		m.publishPublicationReport(work, runner.PublicationPending, runner.PublicationPending, time.Time{})
	case publicationRequiredArtifacts:
		work.artifactResult = uploadArtifactManifestEntries(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir, pw.outputDirs)
		consumeEstimatedBytes(work, work.artifactResult.Bytes)
		if work.artifactResult.Status != runner.UploadStatusOK {
			m.recordError(pw.jobID, "upload-artifacts", fmt.Errorf("status=%s", work.artifactResult.Status))
		}
		requiredState := runner.PublicationReady
		if work.artifactResult.Status != runner.UploadStatusOK {
			requiredState = runner.PublicationFailed
		}
		m.publishPublicationReport(work, requiredState, runner.PublicationPending, time.Now())
	case publicationConventionOutputs:
		work.outputResult = uploadOutputDirs(pw.r2Bucket, pw.jobID, pw.runID, pw.workDir, pw.outputWindowStartUnix, pw.outputDirs)
		consumeEstimatedBytes(work, work.outputResult.Bytes)
		if work.outputResult.Status != runner.UploadStatusOK {
			m.recordError(pw.jobID, "upload-outputs", fmt.Errorf("status=%s", work.outputResult.Status))
		}
	case publicationFinalize:
		m.finalizePostJobWork(work)
		return true
	}
	return false
}

func consumeEstimatedBytes(work *queuedPostJobWork, completed int64) {
	if work == nil || work.estimatedBytes == nil || completed <= 0 {
		return
	}
	remaining := *work.estimatedBytes - completed
	if remaining < 0 {
		remaining = 0
	}
	work.estimatedBytes = &remaining
}

func (m *bgWorkManager) finalizePostJobWork(work *queuedPostJobWork) {
	pw := work.pw
	uploadResult := work.artifactResult
	mergeUploadResults(&uploadResult, &work.outputResult)
	patchCompletionUpload(pw.logSnapshot, pw.jobID, &uploadResult)
	patchCompletionResultsUpload(pw.logSnapshot, pw.jobID, &work.resultsUpload)
	drainState := runner.PublicationReady
	if uploadResult.Status != runner.UploadStatusOK || work.resultsUpload.Status != runner.UploadStatusOK {
		drainState = runner.PublicationFailed
	}

	uploadEndedUnix := uploadResult.CompletedAtUnix
	if work.resultsUpload.CompletedAtUnix > uploadEndedUnix {
		uploadEndedUnix = work.resultsUpload.CompletedAtUnix
	}
	_ = patchPhaseUploadWindow(pw.logSnapshot, pw.jobID, pw.uploadStartedUnix, uploadEndedUnix)

	report, err := collectMaintenanceReport(pw)
	report.Publication = m.state.get()
	if err != nil {
		m.recordError(pw.jobID, "collect-maintenance", err)
		drainState = runner.PublicationFailed
	}
	if err := uploadMaintenanceReport(pw.r2Bucket, pw.instanceID, report); err != nil {
		m.recordError(pw.jobID, "upload-maintenance", err)
		drainState = runner.PublicationFailed
	}
	if report.PrunedStaleLogSnapshots > 0 {
		slog.Debug("pruned stale log snapshots",
			"count", report.PrunedStaleLogSnapshots,
			"bytes", report.PrunedStaleLogSnapshotBytes)
	}

	if err := repairR2Markers(pw); err != nil {
		m.recordError(pw.jobID, "repair-markers", err)
		drainState = runner.PublicationFailed
	}
	m.publishPublicationReport(work, work.report.Facets.RequiredArtifactsState, drainState, time.Now())
	reuploadCompletion(pw.r2Bucket, pw.jobID, pw.runID, pw.logSnapshot)
	if summary, _ := summarizeJobCompletion(pw.logSnapshot, pw.jobID); summary.JobID != 0 {
		m.recordSummary(summary)
	}
	_ = os.RemoveAll(pw.logSnapshot)
	if pw.cleanupDir != "" {
		_ = os.RemoveAll(pw.cleanupDir)
	}
}

// publishPublicationReport writes an attempt-scoped readiness record outside
// the bulk results prefix and mirrors it into the completion record. A failed
// observation never becomes evidence that an artifact is absent: callers see
// unknown until a later, positively observed report succeeds.
func (m *bgWorkManager) publishPublicationReport(work *queuedPostJobWork, requiredState, drainState string, completedAt time.Time) {
	if work == nil {
		return
	}
	now := time.Now()
	var excludeFromSnapshot *queuedPostJobWork
	if drainState == runner.PublicationReady || drainState == runner.PublicationFailed {
		excludeFromSnapshot = work
	}
	report := runner.PublicationReport{
		Sequence:       work.report.Sequence + 1,
		ObservedAtUnix: now.Unix(),
		Facets: runner.CompletionFacets{
			ExecutionState:         runner.ExecutionComplete,
			RequiredArtifactsState: requiredState,
			DrainState:             drainState,
		},
		Snapshot: m.runnerPublicationSnapshot(now, excludeFromSnapshot),
	}
	if end := completionEndTime(work.pw.logSnapshot, work.pw.jobID); end != nil {
		report.Facets.ExecutionCompletedAtUnix = end
	}
	report.Artifacts, report.Facets.RequiredArtifactsState, report.Facets.UnknownReason = artifactPublicationReport(
		work.pw, work.artifactResult, requiredState, completedAt)
	if report.Facets.RequiredArtifactsState == runner.PublicationReady &&
		work.report.Facets.RequiredArtifactsState == runner.PublicationReady &&
		work.report.Facets.RequiredArtifactsReadyAt != nil {
		report.Facets.RequiredArtifactsReadyAt = work.report.Facets.RequiredArtifactsReadyAt
	}
	previousReady := make(map[string]*int64, len(work.report.Artifacts))
	for _, artifact := range work.report.Artifacts {
		if artifact.ReadyAt != nil {
			previousReady[artifact.Name+"\x00"+artifact.Path] = artifact.ReadyAt
		}
	}
	for i := range report.Artifacts {
		key := report.Artifacts[i].Name + "\x00" + report.Artifacts[i].Path
		if readyAt := previousReady[key]; readyAt != nil {
			report.Artifacts[i].ReadyAt = readyAt
		}
	}
	if !completedAt.IsZero() {
		t := completedAt.Unix()
		if report.Facets.RequiredArtifactsState == runner.PublicationReady && report.Facets.RequiredArtifactsReadyAt == nil {
			report.Facets.RequiredArtifactsReadyAt = &t
		}
		if drainState == runner.PublicationReady || drainState == runner.PublicationFailed {
			report.Facets.DrainCompletedAtUnix = &t
		}
	}
	work.report = report
	patchCompletionRecord(work.pw.logSnapshot, work.pw.jobID, func(rec *runner.CompletionRecord) {
		rec.Publication = &report
	})
	data, err := json.Marshal(report)
	if err != nil {
		m.recordError(work.pw.jobID, "encode-publication", err)
		return
	}
	if err := r2Put(work.pw.r2Bucket, r2keys.JobAttemptPublicationReport(work.pw.jobID, work.pw.runID), string(data)); err != nil {
		m.recordError(work.pw.jobID, "upload-publication", err)
	}
}

func completionEndTime(logDir string, jobID int64) *int64 {
	data, err := os.ReadFile(runner.NewJobPaths(logDir, jobID).Completion)
	if err != nil {
		return nil
	}
	var rec runner.CompletionRecord
	if json.Unmarshal(data, &rec) != nil || rec.EndTime <= 0 {
		return nil
	}
	end := rec.EndTime
	return &end
}

func (m *bgWorkManager) runnerPublicationSnapshot(now time.Time, exclude *queuedPostJobWork) runner.PublicationSnapshot {
	m.mu.Lock()
	snapshot := m.state.get()
	queuedWorks := append([]*queuedPostJobWork(nil), m.queue...)
	activeWorks := make([]*queuedPostJobWork, 0, len(m.active))
	for _, work := range m.active {
		if work != exclude {
			activeWorks = append(activeWorks, work)
		}
	}
	m.mu.Unlock()

	queuedItems, inflightItems := len(queuedWorks), len(activeWorks)
	workerLimit, workersBusy := snapshot.WorkerLimit, len(activeWorks)
	result := runner.PublicationSnapshot{
		QueuedItems:   &queuedItems,
		InflightItems: &inflightItems,
		WorkerLimit:   &workerLimit,
		WorkersBusy:   &workersBusy,
	}
	if queuedBytes, known := sumPublicationBytes(queuedWorks); known {
		result.QueuedBytes = &queuedBytes
	}
	if inflightBytes, known := sumPublicationBytes(activeWorks); known {
		result.InflightBytes = &inflightBytes
	}
	allWorks := append(append([]*queuedPostJobWork(nil), queuedWorks...), activeWorks...)
	if retainedBytes, known := sumRetainedBytes(allWorks); known {
		result.RetainedBytes = &retainedBytes
	}
	if snapshot.OldestQueuedAtUnix > 0 {
		age := now.Sub(time.Unix(snapshot.OldestQueuedAtUnix, 0)).Milliseconds()
		if age < 0 {
			age = 0
		}
		result.OldestQueuedAgeMS = &age
	}
	if snapshot.LastProgressAtUnix > 0 {
		last := snapshot.LastProgressAtUnix
		result.LastProgressAt = &last
	}
	return result
}

func artifactPublicationReport(pw postJobWork, upload runner.OutputUploadResult, requestedState string, completedAt time.Time) ([]runner.ArtifactPublication, string, string) {
	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(pw.jobID))
	manifest, err := artifacts.ReadManifestFile(manifestPath, pw.jobID)
	if err != nil {
		return nil, runner.PublicationUnknown, "artifact manifest could not be observed: " + err.Error()
	}
	outcomes := make(map[string][]runner.OutputDirUpload)
	for _, outcome := range upload.Dirs {
		outcomes[outcome.Dir] = append(outcomes[outcome.Dir], outcome)
	}
	artifactsReport := make([]runner.ArtifactPublication, 0, len(manifest.Artifacts))
	overall := requestedState
	for _, spec := range manifest.Artifacts {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.Path
		}
		state := requestedState
		detail := ""
		if requestedState != runner.PublicationPending {
			entries := outcomes[spec.Path]
			if len(entries) == 0 {
				state = runner.PublicationUnknown
				detail = "upload outcome was not observed"
				overall = runner.PublicationUnknown
			} else {
				outcome := entries[0]
				outcomes[spec.Path] = entries[1:]
				if outcome.Status == runner.UploadStatusOK {
					state = runner.PublicationReady
				} else {
					state = runner.PublicationFailed
					detail = outcome.Error
					overall = runner.PublicationFailed
				}
			}
		}
		publication := runner.ArtifactPublication{
			Name: name, Path: spec.Path, State: state, Detail: detail,
			PayloadKey: artifactPayloadKey(pw, manifest, spec.Path),
		}
		if state == runner.PublicationReady && !completedAt.IsZero() {
			t := completedAt.Unix()
			publication.ReadyAt = &t
		}
		artifactsReport = append(artifactsReport, publication)
	}
	if len(manifest.Artifacts) == 0 && requestedState != runner.PublicationPending {
		overall = runner.PublicationReady
	}
	return artifactsReport, overall, ""
}

func artifactPayloadKey(pw postJobWork, manifest artifacts.Manifest, artifactPath string) string {
	if rel, ok := r2resolve.ConventionOutputRelPath(manifest, artifactPath, pw.outputDirs); ok {
		return r2keys.JobAttemptOutputsPrefix(pw.jobID, pw.runID) + rel
	}
	return r2keys.JobAttemptArtifactFilesPrefix(pw.jobID, pw.runID) + filepath.ToSlash(artifacts.LocalRelativePath(artifactPath))
}

func (m *bgWorkManager) updatePublicationStateLocked(progressAt time.Time) {
	snapshot := publicationSnapshot{
		QueuedItems:   len(m.queue),
		InflightItems: len(m.active),
		WorkerLimit:   m.workerLimit,
		WorkersBusy:   len(m.active),
	}
	if !progressAt.IsZero() {
		previous := m.state.get()
		snapshot.LastProgressAtUnix = progressAt.Unix()
		if previous.LastProgressAtUnix > snapshot.LastProgressAtUnix {
			snapshot.LastProgressAtUnix = previous.LastProgressAtUnix
		}
	} else if m.state != nil {
		snapshot.LastProgressAtUnix = m.state.get().LastProgressAtUnix
	}
	queuedBytes, queuedKnown := sumPublicationBytes(m.queue)
	if queuedKnown {
		snapshot.QueuedBytes = &queuedBytes
	}
	activeWorks := make([]*queuedPostJobWork, 0, len(m.active))
	for _, work := range m.active {
		activeWorks = append(activeWorks, work)
	}
	inflightBytes, inflightKnown := sumPublicationBytes(activeWorks)
	if inflightKnown {
		snapshot.InflightBytes = &inflightBytes
	}
	all := append(append([]*queuedPostJobWork(nil), m.queue...), activeWorks...)
	retainedBytes, retainedKnown := sumRetainedBytes(all)
	if retainedKnown {
		snapshot.RetainedBytes = &retainedBytes
	}
	for _, work := range m.queue {
		if snapshot.OldestQueuedAtUnix == 0 || work.enqueuedAt.Unix() < snapshot.OldestQueuedAtUnix {
			snapshot.OldestQueuedAtUnix = work.enqueuedAt.Unix()
		}
	}
	m.state.set(snapshot)
}

func sumPublicationBytes(works []*queuedPostJobWork) (int64, bool) {
	var total int64
	for _, work := range works {
		if work.estimatedBytes == nil {
			return 0, false
		}
		total += *work.estimatedBytes
	}
	return total, true
}

func sumRetainedBytes(works []*queuedPostJobWork) (int64, bool) {
	var total int64
	seenWorkdirs := map[string]bool{}
	for _, work := range works {
		if work.workdirBytes == nil || work.snapshotBytes == nil {
			return 0, false
		}
		if work.pw.workDir == "" || !seenWorkdirs[work.pw.workDir] {
			total += *work.workdirBytes
			seenWorkdirs[work.pw.workDir] = true
		}
		total += *work.snapshotBytes
	}
	return total, true
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

// Close stops idle publication workers. Call only after Barrier when no
// producer can enqueue more work. Benchmark barriers intentionally do not
// close the manager because later jobs reuse it.
func (m *bgWorkManager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cond.Broadcast()
	m.mu.Unlock()
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
	m.registerUploadLocked(workdir)
	m.mu.Unlock()
}

func (m *bgWorkManager) registerUploadLocked(workdir string) {
	if workdir == "" {
		return
	}
	wg, ok := m.uploadsByWorkdir[workdir]
	if !ok {
		wg = &sync.WaitGroup{}
		m.uploadsByWorkdir[workdir] = wg
	}
	wg.Add(1)
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

func repairR2Markers(pw postJobWork) error {
	if pw.r2Bucket == "" {
		return nil
	}
	if err := r2PutForAgent(pw.r2Bucket, r2keys.JobAttemptComplete(pw.jobID, pw.runID), fmt.Sprintf("%d", pw.exitCode)); err != nil {
		return err
	}
	// These keys are opportunistic live views. Earlier completion handling may
	// already have removed them, and rclone deletefile reports a missing object
	// as an error. Their absence is the desired final state; only failure to
	// publish the authoritative completion marker makes the drain fail.
	_ = r2DeleteForAgent(pw.r2Bucket, r2keys.JobAttemptProgress(pw.jobID, pw.runID))
	_ = r2DeleteForAgent(pw.r2Bucket, r2keys.JobAttemptLiveTimeseries(pw.jobID, pw.runID))
	_ = r2DeleteForAgent(pw.r2Bucket, r2keys.JobAttemptLiveTelemetry(pw.jobID, pw.runID))
	return nil
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

func uploadMaintenanceReport(bucket string, instanceID int64, report maintenanceReport) error {
	if bucket == "" {
		return nil
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	var errs []error
	if report.JobID > 0 {
		errs = append(errs, r2Put(bucket, r2keys.JobAttemptMaintenance(report.JobID, report.RunID), string(data)))
	}
	if instanceID > 0 {
		errs = append(errs, r2Put(bucket, r2keys.InstanceMaintenance(instanceID), string(data)))
	}
	return errors.Join(errs...)
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

// estimatePostJobBytes returns the unique source bytes intended for
// publication and the local bytes retained until publication completes.
// A nil pointer means a source could not be inspected; measured empty work is
// represented by a non-nil zero. File paths are deduplicated across convention
// outputs and declared artifacts so the estimate cannot double-count wb20's
// overlapping spelling.
func estimatePostJobBytes(pw postJobWork) (estimated, retained, workdirBytes, snapshotBytes *int64) {
	unique := map[string]int64{}
	known := true
	addTree := func(root string, since time.Time) {
		if root == "" {
			return
		}
		info, err := os.Stat(root)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				known = false
			}
			return
		}
		if !info.IsDir() {
			if since.IsZero() || !info.ModTime().Before(since) {
				unique[filepath.Clean(root)] = info.Size()
			}
			return
		}
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				known = false
				return nil
			}
			if d.IsDir() {
				return nil
			}
			fileInfo, statErr := d.Info()
			if statErr != nil {
				known = false
				return nil
			}
			if !since.IsZero() && fileInfo.ModTime().Before(since) {
				return nil
			}
			unique[filepath.Clean(path)] = fileInfo.Size()
			return nil
		}); err != nil {
			known = false
		}
	}

	addTree(pw.logSnapshot, time.Time{})
	window := runner.AttemptOutputThreshold(pw.outputWindowStartUnix)
	for _, dir := range config.EffectiveOutputDirs(pw.outputDirs) {
		addTree(filepath.Join(pw.workDir, filepath.FromSlash(strings.TrimRight(dir, "/"))), window)
	}

	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(pw.jobID))
	manifest, err := artifacts.ReadManifestFile(manifestPath, pw.jobID)
	if err != nil && !errors.Is(err, artifacts.ErrManifestMissing) {
		known = false
	} else {
		root := runner.ExpandTilde(artifacts.ResolveArtifactRoot(manifest, pw.workDir))
		for _, spec := range manifest.Artifacts {
			if strings.TrimSpace(spec.Path) == "" {
				continue
			}
			addTree(runner.ExpandTilde(artifacts.ResolveRemotePath(root, spec.Path)), time.Time{})
		}
	}
	if known {
		var total int64
		for _, size := range unique {
			total += size
		}
		estimated = &total
	}

	workdirSize, workdirKnown := measureLocalTreeKnown(pw.workDir)
	snapshotSize, snapshotKnown := measureLocalTreeKnown(pw.logSnapshot)
	if workdirKnown {
		workdirBytes = &workdirSize
	}
	if snapshotKnown {
		snapshotBytes = &snapshotSize
	}
	if workdirKnown && snapshotKnown {
		retainedTotal := workdirSize + snapshotSize
		retained = &retainedTotal
	}
	return estimated, retained, workdirBytes, snapshotBytes
}

func measureLocalTreeKnown(root string) (int64, bool) {
	if root == "" {
		return 0, true
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, errors.Is(err, os.ErrNotExist)
	}
	if !info.IsDir() {
		return info.Size(), true
	}
	var bytes int64
	known := true
	if err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			known = false
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fileInfo, statErr := d.Info()
		if statErr != nil {
			known = false
			return nil
		}
		bytes += fileInfo.Size()
		return nil
	}); err != nil {
		return 0, false
	}
	return bytes, known
}
