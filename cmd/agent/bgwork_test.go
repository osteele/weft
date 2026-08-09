package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

func TestBGWorkManager_BoundsWorkersAndPrioritizesDiagnostics(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := &publicationState{}
	m := newBGWorkManagerWithOptions(nil, true, 1, 8, state)
	defer m.Close()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var order []string
	m.stageRunner = func(work *queuedPostJobWork) bool {
		mu.Lock()
		order = append(order, fmt.Sprintf("%d:%s", work.pw.jobID, work.stage))
		mu.Unlock()
		if work.pw.jobID == 1 && work.stage == publicationDiagnostics {
			once.Do(func() { close(firstStarted) })
			<-releaseFirst
		}
		return work.stage == publicationFinalize
	}

	m.StartPostJobWork(postJobWork{jobID: 1, runID: 1, workDir: "/tmp/priority-a"})
	<-firstStarted
	m.StartPostJobWork(postJobWork{jobID: 2, runID: 2, workDir: "/tmp/priority-b"})
	close(releaseFirst)
	m.Barrier()

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 3 {
		t.Fatalf("stage order = %v, want at least three stages", order)
	}
	if order[0] != "1:diagnostic_results" || order[1] != "2:diagnostic_results" || order[2] != "1:required_artifact" {
		t.Fatalf("stage order = %v, want later diagnostics before earlier artifacts", order)
	}
	got := state.get()
	if got.QueuedItems != 0 || got.InflightItems != 0 || got.WorkersBusy != 0 {
		t.Fatalf("final publication snapshot = %+v, want drained", got)
	}
}

func TestBGWorkManager_QueueCapacityBackpressuresAdmission(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newBGWorkManagerWithOptions(nil, true, 1, 1, nil)
	defer m.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.stageRunner = func(work *queuedPostJobWork) bool {
		once.Do(func() { close(started) })
		<-release
		return true
	}

	m.StartPostJobWork(postJobWork{jobID: 1, runID: 1, workDir: "/tmp/cap-a"})
	<-started
	admitted := make(chan struct{})
	go func() {
		m.StartPostJobWork(postJobWork{jobID: 2, runID: 2, workDir: "/tmp/cap-b"})
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("second publication admitted above capacity while first was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("second publication did not admit after capacity became available")
	}
	m.Barrier()
}

func TestBGWorkManager_RetainedByteLimitBackpressuresAdmission(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	makeSource := func(name string) (string, string) {
		workdir := filepath.Join(t.TempDir(), name, "work")
		snapshot := filepath.Join(t.TempDir(), name, "logs")
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(snapshot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workdir, "payload"), make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snapshot, "completion"), make([]byte, 2), 0o644); err != nil {
			t.Fatal(err)
		}
		return workdir, snapshot
	}
	workA, logsA := makeSource("a")
	workB, logsB := makeSource("b")

	m := newBGWorkManagerWithOptions(nil, true, 1, 4, nil)
	m.maxRetainedBytes = 20
	defer m.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.stageRunner = func(*queuedPostJobWork) bool {
		once.Do(func() { close(started) })
		<-release
		return true
	}
	m.StartPostJobWork(postJobWork{jobID: 1, runID: 1, workDir: workA, logSnapshot: logsA})
	<-started
	admitted := make(chan struct{})
	go func() {
		m.StartPostJobWork(postJobWork{jobID: 2, runID: 2, workDir: workB, logSnapshot: logsB})
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("second publication admitted above retained-byte limit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("second publication did not admit after retained bytes drained")
	}
	m.Barrier()
}

func TestBGWorkManager_RecoversPersistedDescriptor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scope := "test-recovery"
	m1 := newBGWorkManagerForScope(nil, true, 1, 4, nil, scope)
	blocked := make(chan struct{})
	started := make(chan struct{})
	m1.stageRunner = func(*queuedPostJobWork) bool {
		close(started)
		<-blocked
		return true
	}
	m1.StartPostJobWork(postJobWork{jobID: 41, runID: 9, workDir: "/tmp/recover"})
	<-started

	// Simulate a fresh agent process by constructing a second manager against
	// the same durable descriptor. The first worker remains paused for the
	// duration of this assertion.
	m2 := newBGWorkManagerWithOptions(nil, true, 1, 4, nil)
	defer m2.Close()
	var recovered atomic.Int32
	m2.stageRunner = func(work *queuedPostJobWork) bool {
		if work.pw.jobID == 41 && work.pw.runID == 9 {
			recovered.Add(1)
		}
		return true
	}
	m2.mu.Lock()
	m2.descriptorDir = publicationDescriptorDir(scope)
	m2.recoverPublicationDescriptorsLocked()
	m2.updatePublicationStateLocked(time.Time{})
	m2.cond.Broadcast()
	m2.mu.Unlock()
	m2.Barrier()
	if recovered.Load() != 1 {
		t.Fatalf("recovered executions = %d, want 1", recovered.Load())
	}
	close(blocked)
	m1.Barrier()
	m1.Close()
}

func TestArtifactPublicationReportPreservesPendingFailedAndReady(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const jobID, runID = int64(51), int64(7)
	manifest := artifacts.Manifest{JobID: jobID, Artifacts: []artifacts.ArtifactSpec{
		{Name: "model", Path: "./output/model.pt"},
		{Name: "metrics", Path: "reports/metrics.json"},
	}}
	if err := artifacts.WriteManifestFile(runner.ExpandTilde(artifacts.RemoteManifestPath(jobID)), manifest); err != nil {
		t.Fatal(err)
	}
	pw := postJobWork{jobID: jobID, runID: runID, workDir: t.TempDir()}
	pending, state, reason := artifactPublicationReport(pw, runner.OutputUploadResult{}, runner.PublicationPending, time.Time{})
	if state != runner.PublicationPending || reason != "" || len(pending) != 2 {
		t.Fatalf("pending report = (%+v, %q, %q)", pending, state, reason)
	}
	if pending[0].PayloadKey != r2keys.JobAttemptOutputsPrefix(jobID, runID)+"output/model.pt" {
		t.Fatalf("model backing key = %q", pending[0].PayloadKey)
	}

	upload := runner.OutputUploadResult{Status: runner.UploadStatusPartial, Dirs: []runner.OutputDirUpload{
		{Dir: "./output/model.pt", Status: runner.UploadStatusOK},
		{Dir: "reports/metrics.json", Status: runner.UploadStatusFailed, Error: "upload stalled"},
	}}
	completedAt := time.Unix(1234, 0)
	completed, state, _ := artifactPublicationReport(pw, upload, runner.PublicationFailed, completedAt)
	if state != runner.PublicationFailed || completed[0].State != runner.PublicationReady || completed[0].ReadyAt == nil ||
		completed[1].State != runner.PublicationFailed || completed[1].Detail != "upload stalled" {
		t.Fatalf("completed report = %+v, state=%q", completed, state)
	}
}

func TestRepairR2MarkersTreatsAlreadyAbsentLiveKeysAsSuccess(t *testing.T) {
	previousPut, previousDelete := r2PutForAgent, r2DeleteForAgent
	t.Cleanup(func() {
		r2PutForAgent = previousPut
		r2DeleteForAgent = previousDelete
	})

	var deleteCalls int
	r2PutForAgent = func(_, _, _ string) error { return nil }
	r2DeleteForAgent = func(_, _ string) error {
		deleteCalls++
		return errors.New("object not found")
	}
	if err := repairR2Markers(postJobWork{r2Bucket: "bucket", jobID: 42, runID: 7}); err != nil {
		t.Fatalf("repairR2Markers returned cleanup error: %v", err)
	}
	if deleteCalls != 3 {
		t.Fatalf("delete calls = %d, want 3", deleteCalls)
	}

	markerErr := errors.New("completion marker upload failed")
	deleteCalls = 0
	r2PutForAgent = func(_, _, _ string) error { return markerErr }
	if err := repairR2Markers(postJobWork{r2Bucket: "bucket", jobID: 42, runID: 7}); !errors.Is(err, markerErr) {
		t.Fatalf("repairR2Markers error = %v, want %v", err, markerErr)
	}
	if deleteCalls != 0 {
		t.Fatalf("delete calls after marker failure = %d, want 0", deleteCalls)
	}
}

func TestBGWorkManager_TracksWorkdirs(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Dir: "/tmp/test-project-a"},
		{ID: 2, Dir: "/tmp/test-project-a"},
		{ID: 3, Dir: "/tmp/test-project-b"},
	}
	m := newBGWorkManager(jobs, true) // skip deletion
	defer m.Close()

	dirA := runner.ExpandTilde("/tmp/test-project-a")
	dirB := runner.ExpandTilde("/tmp/test-project-b")

	if _, ok := m.workdirs[dirA]; !ok {
		t.Errorf("dirA not tracked")
	}
	if _, ok := m.workdirs[dirB]; !ok {
		t.Errorf("dirB not tracked")
	}
	if len(m.workdirs) != 2 {
		t.Errorf("workdirs size = %d, want 2 (deduplicated)", len(m.workdirs))
	}
}

func TestPublicationReportSnapshotExcludesTerminalReportingChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newBGWorkManagerWithOptions(nil, true, 1, 4, nil)
	defer m.Close()

	remaining, workdirBytes, snapshotBytes := int64(12), int64(10), int64(2)
	work := &queuedPostJobWork{
		pw:             postJobWork{jobID: 77, runID: 3, workDir: "/tmp/reporting-chain"},
		estimatedBytes: &remaining,
		workdirBytes:   &workdirBytes,
		snapshotBytes:  &snapshotBytes,
	}
	m.mu.Lock()
	m.active[work.key()] = work
	m.updatePublicationStateLocked(time.Time{})
	m.mu.Unlock()

	got := m.runnerPublicationSnapshot(time.Now(), work)
	if got.QueuedItems == nil || *got.QueuedItems != 0 ||
		got.InflightItems == nil || *got.InflightItems != 0 ||
		got.WorkersBusy == nil || *got.WorkersBusy != 0 ||
		got.QueuedBytes == nil || *got.QueuedBytes != 0 ||
		got.InflightBytes == nil || *got.InflightBytes != 0 ||
		got.RetainedBytes == nil || *got.RetainedBytes != 0 {
		t.Fatalf("terminal report snapshot = %+v, want no outstanding work", got)
	}
}

func TestBGWorkManager_RegisterNewJobs(t *testing.T) {
	m := newBGWorkManager(nil, true)
	defer m.Close()
	m.RegisterNewJobs([]cloud.AgentJob{
		{ID: 10, Dir: "/tmp/test-new"},
	})

	dir := runner.ExpandTilde("/tmp/test-new")
	if _, ok := m.workdirs[dir]; !ok {
		t.Errorf("new workdir not tracked")
	}
}

// TestBGWorkManager_CleanupDeletesOnlyAtEnd verifies that workdirs are
// preserved through the campaign and only removed by CleanupWorkdirs. This
// is the regression test for the race that wiped a project mid-campaign
// when refcount-driven deletion fired between checkForNewJobs polls.
func TestBGWorkManager_CleanupDeletesOnlyAtEnd(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/pyproject.toml"
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	m := newBGWorkManager([]cloud.AgentJob{{ID: 1, Dir: dir}}, false)
	m.stageRunner = func(*queuedPostJobWork) bool { return true }
	defer m.Close()

	// Simulate the post-job background work completing for the only job.
	// Under the old refcount logic this would have spawned os.RemoveAll(dir);
	// under the new logic the dir must survive until CleanupWorkdirs.
	m.StartPostJobWork(postJobWork{jobID: 1, runID: 1, workDir: dir})
	m.Barrier()

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file missing after Barrier (eager deletion regressed): %v", err)
	}

	// Simulate a late-arriving job for the same workdir picked up by
	// checkForNewJobs after the initial batch finished. Cleanup must still
	// be deferred to the end-of-campaign call.
	m.RegisterNewJobs([]cloud.AgentJob{{ID: 2, Dir: dir}})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file missing after late RegisterNewJobs: %v", err)
	}

	m.CleanupWorkdirs()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workdir still present after CleanupWorkdirs: stat err=%v", err)
	}
}

func TestBGWorkManager_CleanupSkipDeletion(t *testing.T) {
	dir := t.TempDir()
	m := newBGWorkManager([]cloud.AgentJob{{ID: 1, Dir: dir}}, true) // skip
	m.CleanupWorkdirs()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workdir removed despite skipDeletion: %v", err)
	}
}

func TestBGWorkManager_Barrier(t *testing.T) {
	m := newBGWorkManager(nil, true)

	var counter atomic.Int32
	for range 3 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			time.Sleep(10 * time.Millisecond)
			counter.Add(1)
		}()
	}

	m.Barrier()
	if counter.Load() != 3 {
		t.Errorf("counter = %d, want 3", counter.Load())
	}
}

// Regression for wj2226/wj2227 cross-attribution (2026-05-28). See rule
// SharedWorkdirUploadBarrierBeforeNextJob in specs/job-lifecycle.allium.
func TestBGWorkManager_WaitForUploadsInWorkdir_BlocksUntilSameWorkdirUploadFinishes(t *testing.T) {
	m := newBGWorkManager(nil, true)
	dir := runner.ExpandTilde("/tmp/test-shared-wd")

	m.registerUpload(dir)

	released := atomic.Bool{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		released.Store(true)
		m.completeUpload(dir)
	}()

	m.WaitForUploadsInWorkdir(dir)
	if !released.Load() {
		t.Fatalf("WaitForUploadsInWorkdir returned before upload completed")
	}
}

func TestBGWorkManager_WaitForUploadsInWorkdir_DifferentWorkdirsDoNotBlock(t *testing.T) {
	m := newBGWorkManager(nil, true)
	dirA := runner.ExpandTilde("/tmp/test-wd-a")
	dirB := runner.ExpandTilde("/tmp/test-wd-b")

	m.registerUpload(dirA)

	doneB := make(chan struct{})
	go func() {
		m.WaitForUploadsInWorkdir(dirB)
		close(doneB)
	}()

	select {
	case <-doneB:
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("WaitForUploadsInWorkdir for dirB blocked on unrelated dirA upload")
	}
}

func TestBGWorkManager_WaitForUploadsInWorkdir_NoUploadsReturnsImmediately(t *testing.T) {
	m := newBGWorkManager(nil, true)
	done := make(chan struct{})
	go func() {
		m.WaitForUploadsInWorkdir("/tmp/no-uploads-here")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("WaitForUploadsInWorkdir blocked on a workdir with no in-flight uploads")
	}
}

func TestBGWorkManager_BarrierLogsErrors(t *testing.T) {
	m := newBGWorkManager(nil, true)
	m.recordError(42, "test-op", errForTest("test error"))
	m.recordError(43, "test-op2", errForTest("another"))

	// Barrier should drain errors
	m.Barrier()

	m.mu.Lock()
	remaining := len(m.errors)
	m.mu.Unlock()
	if remaining != 0 {
		t.Errorf("errors after barrier = %d, want 0", remaining)
	}
}

func TestCollectMaintenanceReportSamplesHostAndMeasuresTrees(t *testing.T) {
	workDir := t.TempDir()
	logSnapshot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "model.bin"), []byte("12345"), 0o644); err != nil {
		t.Fatalf("write workdir file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logSnapshot, "wj42.log"), []byte("log"), 0o644); err != nil {
		t.Fatalf("write log snapshot file: %v", err)
	}

	report, err := collectMaintenanceReport(postJobWork{
		jobID:       42,
		runID:       7,
		exitCode:    0,
		workDir:     workDir,
		logSnapshot: logSnapshot,
		diskPath:    workDir,
		phase:       "uploading:42",
	})
	if err != nil {
		t.Fatalf("collectMaintenanceReport: %v", err)
	}
	if report.JobID != 42 || report.RunID != 7 || report.ExitCode != 0 {
		t.Fatalf("unexpected report identity: %+v", report)
	}
	if report.Phase != "uploading:42" || report.Heartbeat.Phase != "uploading:42" {
		t.Fatalf("phase not propagated: %+v", report)
	}
	if report.WorkdirBytes != 5 {
		t.Fatalf("workdir bytes = %d, want 5", report.WorkdirBytes)
	}
	if report.LogSnapshotBytes != 3 {
		t.Fatalf("log snapshot bytes = %d, want 3", report.LogSnapshotBytes)
	}
	if report.Ts == 0 {
		t.Fatal("expected timestamp")
	}
}

func TestPruneStaleLogSnapshots(t *testing.T) {
	oldSnapshot := filepath.Join(os.TempDir(), "weft-logs-job-test-old")
	currentSnapshot := filepath.Join(os.TempDir(), "weft-logs-job-test-current")
	t.Cleanup(func() {
		os.RemoveAll(oldSnapshot)
		os.RemoveAll(currentSnapshot)
	})
	if err := os.MkdirAll(oldSnapshot, 0o755); err != nil {
		t.Fatalf("mkdir old snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldSnapshot, "old.log"), []byte("old-data"), 0o644); err != nil {
		t.Fatalf("write old snapshot: %v", err)
	}
	if err := os.MkdirAll(currentSnapshot, 0o755); err != nil {
		t.Fatalf("mkdir current snapshot: %v", err)
	}
	oldTime := time.Now().Add(-staleLogSnapshotAge - time.Hour)
	if err := os.Chtimes(oldSnapshot, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old snapshot: %v", err)
	}

	pruned, bytes, err := pruneStaleLogSnapshots(currentSnapshot, time.Now())
	if err != nil {
		t.Fatalf("pruneStaleLogSnapshots: %v", err)
	}
	if pruned < 1 {
		t.Fatalf("pruned = %d, want at least 1", pruned)
	}
	if bytes < int64(len("old-data")) {
		t.Fatalf("bytes = %d, want at least old-data size", bytes)
	}
	if _, err := os.Stat(oldSnapshot); !os.IsNotExist(err) {
		t.Fatalf("old snapshot still exists: %v", err)
	}
	if _, err := os.Stat(currentSnapshot); err != nil {
		t.Fatalf("current snapshot should be preserved: %v", err)
	}
}

type errForTest string

func (e errForTest) Error() string { return string(e) }
