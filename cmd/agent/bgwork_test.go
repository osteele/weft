package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/runner"
)

func TestBGWorkManager_TracksWorkdirs(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Dir: "/tmp/test-project-a"},
		{ID: 2, Dir: "/tmp/test-project-a"},
		{ID: 3, Dir: "/tmp/test-project-b"},
	}
	m := newBGWorkManager(jobs, true) // skip deletion

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

func TestBGWorkManager_RegisterNewJobs(t *testing.T) {
	m := newBGWorkManager(nil, true)
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

	// Simulate the post-job background work completing for the only job.
	// Under the old refcount logic this would have spawned os.RemoveAll(dir);
	// under the new logic the dir must survive until CleanupWorkdirs.
	m.StartPostJobWork(postJobWork{jobID: 1, workDir: dir})
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
